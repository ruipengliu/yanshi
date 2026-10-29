package kernel

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenSource 按业务线取得服务令牌；实现必须可并发调用，不得回退到其他业务线。
type TokenSource interface {
	Token(context.Context, string) (string, error)
}

type businessLineKey struct{}

// WithBusinessLine 从持久任务的 owner 传播身份，不能从裸 Session ID 猜测或修改共享 Client。
func WithBusinessLine(ctx context.Context, line string) context.Context {
	if previous, ok := ctx.Value(businessLineKey{}).(string); ok && previous != line {
		line = "" // 冲突保持拒绝，不能被后续嵌套调用重新覆盖。
	}
	return context.WithValue(ctx, businessLineKey{}, line)
}

type TokenLocation struct {
	TokenEnv  string `json:"token_env,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	KeyFile   string `json:"key_file,omitempty"`
}

type TokenOptions struct {
	Audience      string
	TTL           time.Duration
	RefreshBefore time.Duration
	Now           func() time.Time
}

type tokenEntry struct {
	location TokenLocation
	token    string
	expires  time.Time
}

type serviceTokens struct {
	mu      sync.Mutex
	entries map[string]*tokenEntry
	opts    TokenOptions
}

var errToken = errors.New("kernel: service token unavailable")

// NewTokenSource 支持按业务线混合配置静态令牌与私钥；静态来源每次重读，私钥在刷新时重读。
// 令牌与驱动错误都不出现在错误中，避免调用方日志泄露凭据。
func NewTokenSource(locations map[string]TokenLocation, opts TokenOptions) (TokenSource, error) {
	if opts.Audience == "" {
		opts.Audience = "yanshi"
	}
	if opts.TTL == 0 {
		opts.TTL = 15 * time.Minute
	}
	if opts.RefreshBefore == 0 {
		opts.RefreshBefore = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if len(locations) == 0 || opts.TTL <= time.Second || opts.TTL > time.Hour || opts.RefreshBefore < 0 || opts.RefreshBefore >= opts.TTL {
		return nil, errToken
	}
	s := &serviceTokens{entries: make(map[string]*tokenEntry), opts: opts}
	for line, location := range locations {
		count := 0
		for _, value := range []string{location.TokenEnv, location.TokenFile, location.KeyFile} {
			if value != "" {
				count++
			}
		}
		if line == "" || count != 1 {
			return nil, errToken
		}
		s.entries[line] = &tokenEntry{location: location}
	}
	return s, nil
}

func (s *serviceTokens) Token(ctx context.Context, line string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e := s.entries[line]
	if e == nil {
		return "", errToken
	}
	now := s.opts.Now()
	if e.location.KeyFile != "" {
		if e.token != "" && now.Add(s.opts.RefreshBefore).Before(e.expires) {
			return e.token, nil
		}
		token, expires, err := s.sign(e.location.KeyFile, line, now)
		if err != nil {
			return "", errToken
		}
		e.token, e.expires = token, expires
		return token, nil
	}
	value := ""
	if e.location.TokenEnv != "" {
		value = os.Getenv(e.location.TokenEnv)
	} else {
		data, err := os.ReadFile(e.location.TokenFile)
		if err != nil {
			return "", errToken
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	// 此处只检查选择与到期，不替代内核验签。静态提供方须在刷新窗口前替换文件或环境值。
	var c serviceClaims
	if _, _, err := jwt.NewParser().ParseUnverified(value, &c); err != nil || c.Issuer != line || c.Scope != "service" || c.Subject != "" || c.ExpiresAt == nil || c.IssuedAt == nil || c.IssuedAt.After(now.Add(time.Minute)) || !now.Add(s.opts.RefreshBefore).Before(c.ExpiresAt.Time) {
		return "", errToken
	}
	found := false
	for _, audience := range c.Audience {
		if audience == s.opts.Audience {
			found = true
		}
	}
	if !found {
		return "", errToken
	}
	return value, nil
}

type serviceClaims struct {
	Scope string `json:"scope"`
	jwt.RegisteredClaims
}

func (s *serviceTokens) sign(path, line string, now time.Time) (string, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, errToken
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || block.Headers["business_line"] != line || block.Headers["kid"] == "" {
		return "", time.Time{}, errToken
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", time.Time{}, errToken
	}
	var method jwt.SigningMethod
	switch k := key.(type) {
	case ed25519.PrivateKey:
		method = jwt.SigningMethodEdDSA
	case *ecdsa.PrivateKey:
		if k.Curve == elliptic.P256() {
			method = jwt.SigningMethodES256
		}
	case *rsa.PrivateKey:
		if k.N.BitLen() >= 2048 {
			method = jwt.SigningMethodRS256
		}
	}
	if method == nil {
		return "", time.Time{}, errToken
	}
	expires := now.Add(s.opts.TTL).Truncate(time.Second)
	claims := serviceClaims{Scope: "service", RegisteredClaims: jwt.RegisteredClaims{Issuer: line, Audience: jwt.ClaimStrings{s.opts.Audience}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires)}}
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = block.Headers["kid"]
	value, err := token.SignedString(key)
	return value, expires, err
}
