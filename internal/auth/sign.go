package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// SigningKey 是业务线的私钥。它只供开发工具与测试扮演业务线服务端签发令牌；
// yanshi 服务本身从不加载私钥（ADR-0014）。
type SigningKey struct {
	KID          string
	BusinessLine string
	Alg          string
	Key          crypto.Signer
}

// GenerateDevKey 生成 Ed25519 开发密钥。
func GenerateDevKey(businessLine, kid string) (*SigningKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &SigningKey{KID: kid, BusinessLine: businessLine, Alg: "EdDSA", Key: priv}, nil
}

// Sign 签发令牌；endUser 为空时签发服务令牌。
func (k *SigningKey) Sign(audience, endUser string, ttl time.Duration, now time.Time) (string, error) {
	c := claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: k.BusinessLine, Subject: endUser, Audience: jwt.ClaimStrings{audience},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}}
	if endUser == "" {
		c.Scope = ScopeService
	}
	t := jwt.NewWithClaims(jwt.GetSigningMethod(k.Alg), c)
	t.Header["kid"] = k.KID
	return t.SignedString(k.Key)
}

// Public 返回登记到 yanshi 的业务线配置（只含公钥）。
func (k *SigningKey) Public() (BusinessLine, error) {
	der, err := x509.MarshalPKIXPublicKey(k.Key.Public())
	if err != nil {
		return BusinessLine{}, err
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return BusinessLine{Name: k.BusinessLine, Keys: []Key{{KID: k.KID, Alg: k.Alg, PublicKey: string(pub)}}}, nil
}

// MarshalPEM 以 PKCS#8 编码私钥，kid 与业务线记在 PEM 头中，使密钥文件自描述。
func (k *SigningKey) MarshalPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Headers: map[string]string{
		"kid": k.KID, "business_line": k.BusinessLine,
	}, Bytes: der}), nil
}

// LoadSigningKey 读取 MarshalPEM 写出的私钥文件。
func LoadSigningKey(path string) (*SigningKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	k := &SigningKey{KID: block.Headers["kid"], BusinessLine: block.Headers["business_line"]}
	if k.KID == "" || k.BusinessLine == "" {
		return nil, fmt.Errorf("%s: PEM headers kid and business_line are required", path)
	}
	switch p := priv.(type) {
	case ed25519.PrivateKey:
		k.Alg, k.Key = "EdDSA", p
	case *ecdsa.PrivateKey:
		k.Alg, k.Key = "ES256", p
	case *rsa.PrivateKey:
		k.Alg, k.Key = "RS256", p
	default:
		return nil, errors.New("unsupported private key type")
	}
	return k, nil
}
