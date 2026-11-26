package extloop

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 步令牌（ADR-0063 第 2 条）：平台在每步请求中签发、外部服务拿来调用内核模型端点的短期令牌。以这个循环登记的
// HMAC 密钥签发，而不是平台级的非对称私钥：一把平台私钥泄露，所有业务线的步令牌都可伪造（ADR-0014 正是为避免
// yanshi 持有签名私钥而选了业务线签发）；按登记分开的共享密钥把影响限制在一条业务线的一个循环。
const (
	stepIssuer   = "yanshi"
	stepAudience = "yanshi-loop-step"
	// minSecret 是 HMAC 密钥的最短长度（字节），与 HS256 的输出长度相同。
	minSecret = 32
	// clockLeeway 容忍签发方（Worker 所在实例）与验证方（网关实例）的时钟偏差：没有它，签发方时钟略快时
	// iat 落在验证方的"未来"，令牌被拒。
	clockLeeway = 5 * time.Second
)

// TokenKey 是签发步令牌的一把 HMAC 密钥（至少 32 个随机字节的编码，如 openssl rand -base64 32）。登记可列多把：
// 签发用第一把，验证接受全部。签发（worker 角色）与验证（gateway 角色）各读自己的登记，所以轮换分两次发布：
// 先在所有进程加上新钥（排在后面），再把它置首；等旧令牌过期（一步的时限）后删旧钥。
type TokenKey struct {
	KID    string `yaml:"kid"`
	Secret string `yaml:"secret"`
}

// StepClaims 是步令牌的声明：这一步属于哪条业务线、哪个循环、Session、Run 与 EndUser。Attempt 与 Turn 只用于观察，
// 模型端点不核对它们是否当前（ADR-0063 第 2 条）。
type StepClaims struct {
	jwt.RegisteredClaims
	BusinessLine string `json:"business_line"`
	Loop         string `json:"loop"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	Attempt      uint32 `json:"attempt"`
	Turn         uint32 `json:"turn"`
	EndUser      string `json:"end_user"`
	AgentName    string `json:"agent_name"`
	AgentVersion string `json:"agent_version"`
	// Background 标记后台 Run 的步：经端点的调用在提供商准入处让交互调用先行（ADR-0029）。
	Background bool `json:"background,omitempty"`
}

// SignStep 以 keys 的第一把签发步令牌，有效期从 now 起 ttl。keys 为空时返回空串：登记没有配置密钥。
func SignStep(keys []TokenKey, c StepClaims, now time.Time, ttl time.Duration) (string, error) {
	if len(keys) == 0 {
		return "", nil
	}
	c.RegisteredClaims = jwt.RegisteredClaims{
		Issuer: stepIssuer, Audience: jwt.ClaimStrings{stepAudience},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	t.Header["kid"] = keys[0].KID
	return t.SignedString([]byte(keys[0].Secret))
}

// ErrStepToken 是步令牌无效（签名、有效期、密钥或声明）。不区分原因：错误会回给外部服务。
var ErrStepToken = errors.New("invalid step token")

// KeyLookup 按 (业务线, 循环) 取出登记的密钥与一步的时限；没有这个登记时 keys 为空。
type KeyLookup func(businessLine, loop string) (keys []TokenKey, stepTimeout time.Duration)

// VerifyStep 验证步令牌：按声明中的 (业务线, 循环) 取出登记的密钥，按头中的 kid 选一把；有效期不得长于登记的
// 一步时限。持有密钥的一方（业务线，或从外部服务处窃得密钥者）能签发任意声明的令牌，验证方因此不信令牌自报的
// 有效期：否则一张令牌可以用到 2099 年，"至多一步"与"等旧令牌过期再删旧钥"都不成立。
func VerifyStep(token string, keys KeyLookup, now time.Time) (*StepClaims, error) {
	c := &StepClaims{}
	var ttl time.Duration
	_, err := jwt.ParseWithClaims(token, c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		// 解析阶段的声明尚未验证，只用来找密钥：伪造的业务线或循环名找到的是别人的密钥，签名验不过。
		var ks []TokenKey
		ks, ttl = keys(c.BusinessLine, c.Loop)
		for _, k := range ks {
			if k.KID == kid {
				return []byte(k.Secret), nil
			}
		}
		return nil, errors.New("unknown key")
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(stepIssuer),
		jwt.WithAudience(stepAudience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(clockLeeway),
		jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || c.BusinessLine == "" || c.Loop == "" || c.SessionID == "" || c.RunID == "" || c.AgentName == "" || c.EndUser == "" {
		return nil, ErrStepToken
	}
	// iat 必填：有效期按 exp - iat 核对。秒级精度的取整由 leeway 吸收。
	if c.IssuedAt == nil || c.ExpiresAt.Sub(c.IssuedAt.Time) > ttl+clockLeeway {
		return nil, ErrStepToken
	}
	return c, nil
}

func validateKeys(keys []TokenKey) error {
	seen := map[string]bool{}
	for _, k := range keys {
		if k.KID == "" || seen[k.KID] {
			return fmt.Errorf("token_keys: kid must be non-empty and unique (%q)", k.KID)
		}
		seen[k.KID] = true
		if len(k.Secret) < minSecret {
			return fmt.Errorf("token_keys %s: secret must be at least %d bytes", k.KID, minSecret)
		}
	}
	return nil
}
