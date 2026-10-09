// Package auth 验证业务线签发的令牌，给出调用方身份（docs/design/auth.md，ADR-0014）。
//
// 令牌是 JWT，由业务线用自己的私钥签发，yanshi 只持有公钥：用户令牌代表一个 EndUser，
// 服务令牌代表业务线服务端。验证完全在本地完成，任意实例都可以处理。
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gopkg.in/yaml.v3"

	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/usage"
)

// Principal 是已验证的调用方身份。
type Principal struct {
	BusinessLine string
	// EndUser 为空表示服务令牌：可访问该 BusinessLine 下所有 EndUser 的资源。
	EndUser string
	// Unrestricted 只用于 -auth none 的开发模式：信任请求中自报的身份。
	Unrestricted bool
	// Expires 是令牌到期时间，长连接在此刻断开；零值表示不过期。
	Expires time.Time
}

// Service 报告 p 是否为服务令牌。
func (p Principal) Service() bool { return !p.Unrestricted && p.EndUser == "" }

// Allows 报告 p 能否访问属于 (businessLine, endUser) 的资源。
func (p Principal) Allows(businessLine, endUser string) bool {
	if p.Unrestricted {
		return true
	}
	return p.BusinessLine == businessLine && (p.EndUser == "" || p.EndUser == endUser)
}

// ErrUnauthenticated 表示令牌缺失或无效。
var ErrUnauthenticated = errors.New("unauthenticated")

type Verifier interface {
	Verify(token string) (Principal, error)
}

// Insecure 不校验令牌，所有调用方都是 Unrestricted；只允许在回环地址上使用（docs/design/auth.md §5）。
type Insecure struct{}

func (Insecure) Verify(string) (Principal, error) { return Principal{Unrestricted: true}, nil }

// Key 是业务线登记的一把公钥。
type Key struct {
	KID string `yaml:"kid"`
	// Alg 是该密钥唯一接受的签名算法：EdDSA、ES256 或 RS256。
	Alg       string `yaml:"alg"`
	PublicKey string `yaml:"public_key"`
}

// BusinessLine 是一条业务线的鉴权配置（businesslines/*.yaml）。
type BusinessLine struct {
	Name string `yaml:"name"`
	Keys []Key  `yaml:"keys"`
	// MaxTTL 是令牌允许的最长有效期，默认 1 小时。
	MaxTTL time.Duration `yaml:"max_ttl"`
	// Retention 是该业务线 Session 的保留策略（docs/design/m2-session-lifecycle.md §5）。
	Retention lifecycle.Retention `yaml:"retention"`
	// Quota 是该业务线的配额（docs/design/m4-quota-usage.md §3）。
	Quota usage.Limits `yaml:"quota,omitempty"`
}

// LoadDir 读取 dir 下所有 *.yaml 业务线配置。
func LoadDir(dir string) ([]BusinessLine, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []BusinessLine
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var bl BusinessLine
		if err := yaml.Unmarshal(b, &bl); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := bl.Quota.Prepare(); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, bl)
	}
	return out, nil
}

// MaxTTLLimit 是任何业务线可配置的 max_ttl 上限：令牌不可吊销，有效期必须短。
const MaxTTLLimit = 24 * time.Hour

// Leeway 是允许的时钟偏差。
const Leeway = time.Minute

type verifyKey struct {
	businessLine string
	alg          string
	pub          any
	maxTTL       time.Duration
}

// JWT 校验业务线签发的令牌。
type JWT struct {
	audience string
	clock    clock.Clock
	keys     map[string]*verifyKey
}

// NewJWT 由业务线配置构造校验器；audience 是本部署的标识，令牌的 aud 必须包含它。
func NewJWT(audience string, c clock.Clock, lines ...BusinessLine) (*JWT, error) {
	if audience == "" {
		return nil, errors.New("auth: audience is required")
	}
	v := &JWT{audience: audience, clock: c, keys: map[string]*verifyKey{}}
	names := map[string]bool{}
	for _, bl := range lines {
		if bl.Name == "" || names[bl.Name] {
			return nil, fmt.Errorf("auth: business line name %q missing or duplicated", bl.Name)
		}
		names[bl.Name] = true
		ttl := bl.MaxTTL
		if ttl == 0 {
			ttl = time.Hour
		}
		if ttl < 0 || ttl > MaxTTLLimit {
			return nil, fmt.Errorf("auth: business line %s: max_ttl must be in (0, %s]", bl.Name, MaxTTLLimit)
		}
		for _, k := range bl.Keys {
			if k.KID == "" || v.keys[k.KID] != nil {
				return nil, fmt.Errorf("auth: business line %s: kid %q missing or duplicated", bl.Name, k.KID)
			}
			pub, err := parsePublicKey(k.Alg, k.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("auth: business line %s key %s: %w", bl.Name, k.KID, err)
			}
			v.keys[k.KID] = &verifyKey{businessLine: bl.Name, alg: k.Alg, pub: pub, maxTTL: ttl}
		}
	}
	return v, nil
}

// parsePublicKey 解析 PEM 公钥，并要求密钥类型与算法一致，防止算法混淆。
func parsePublicKey(alg, pemText string) (any, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("public_key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		if alg == "EdDSA" {
			return k, nil
		}
	case *ecdsa.PublicKey:
		if alg == "ES256" && k.Curve == elliptic.P256() {
			return k, nil
		}
	case *rsa.PublicKey:
		if alg == "RS256" && k.N.BitLen() >= 2048 {
			return k, nil
		}
	}
	return nil, fmt.Errorf("key type %T does not match alg %q (EdDSA | ES256 with P-256 | RS256 ≥ 2048 bit)", pub, alg)
}

type claims struct {
	// Scope 是 "user"（默认）或 "service"。
	Scope string `json:"scope,omitempty"`
	jwt.RegisteredClaims
}

const (
	ScopeUser    = "user"
	ScopeService = "service"
)

func (v *JWT) Verify(token string) (Principal, error) {
	var c claims
	var key *verifyKey
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"EdDSA", "ES256", "RS256"}),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(Leeway),
		jwt.WithTimeFunc(v.clock.Now),
	)
	_, err := parser.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key = v.keys[kid]
		if key == nil {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		if t.Method.Alg() != key.alg {
			return nil, fmt.Errorf("alg %s not allowed for kid %s", t.Method.Alg(), kid)
		}
		return key.pub, nil
	})
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	switch {
	case c.Issuer != key.businessLine:
		return Principal{}, fmt.Errorf("%w: issuer %q does not own the signing key", ErrUnauthenticated, c.Issuer)
	case c.IssuedAt == nil:
		return Principal{}, fmt.Errorf("%w: iat is required", ErrUnauthenticated)
	case c.ExpiresAt.Sub(c.IssuedAt.Time) > key.maxTTL:
		return Principal{}, fmt.Errorf("%w: token lifetime exceeds %s", ErrUnauthenticated, key.maxTTL)
	}
	p := Principal{BusinessLine: key.businessLine, EndUser: c.Subject, Expires: c.ExpiresAt.Time}
	switch c.Scope {
	case "", ScopeUser:
		if c.Subject == "" {
			return Principal{}, fmt.Errorf("%w: user token without sub", ErrUnauthenticated)
		}
	case ScopeService:
		if c.Subject != "" {
			return Principal{}, fmt.Errorf("%w: service token must not have sub", ErrUnauthenticated)
		}
	default:
		return Principal{}, fmt.Errorf("%w: unknown scope %q", ErrUnauthenticated, c.Scope)
	}
	return p, nil
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext 返回请求的调用方；未经 Middleware 的请求没有身份。
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// BearerToken 取出 Authorization: Bearer 中的令牌。
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Middleware 校验每个请求的令牌并把身份放入 context；失败时返回 401。
func Middleware(v Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := BearerToken(r)
		if _, insecure := v.(Insecure); !insecure && token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		p, err := v.Verify(token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
