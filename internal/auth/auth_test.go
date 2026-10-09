package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"yanshi/internal/clock"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*JWT, *SigningKey, *SigningKey) {
	t.Helper()
	demo, err := GenerateDevKey("demo", "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := GenerateDevKey("other", "other-1")
	if err != nil {
		t.Fatal(err)
	}
	var lines []BusinessLine
	for _, k := range []*SigningKey{demo, other} {
		bl, err := k.Public()
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, bl)
	}
	v, err := NewJWT("yanshi", clock.NewFake(t0), lines...)
	if err != nil {
		t.Fatal(err)
	}
	return v, demo, other
}

func TestVerifyUserAndServiceTokens(t *testing.T) {
	v, demo, _ := setup(t)
	tok, _ := demo.Sign("yanshi", "u1", time.Hour, t0)
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.BusinessLine != "demo" || p.EndUser != "u1" || p.Service() || !p.Expires.Equal(t0.Add(time.Hour)) {
		t.Fatalf("principal = %+v", p)
	}
	if !p.Allows("demo", "u1") || p.Allows("demo", "u2") || p.Allows("other", "u1") {
		t.Fatal("user token scope is wrong")
	}

	tok, _ = demo.Sign("yanshi", "", time.Hour, t0)
	p, err = v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Service() || !p.Allows("demo", "anyone") || p.Allows("other", "u1") {
		t.Fatalf("service principal = %+v", p)
	}
}

// forge 用 demo 的密钥签发任意声明，用于构造各种不合规令牌。
func forge(t *testing.T, k *SigningKey, kid string, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(k.Key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRejectsInvalidTokens(t *testing.T) {
	v, demo, other := setup(t)
	valid := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": "demo", "sub": "u1", "aud": "yanshi", "iat": t0.Unix(), "exp": t0.Add(time.Hour).Unix()}
	}
	with := func(k string, val any) jwt.MapClaims {
		c := valid()
		if val == nil {
			delete(c, k)
		} else {
			c[k] = val
		}
		return c
	}
	good := forge(t, demo, "demo-1", valid())
	parts := strings.Split(good, ".")
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"demo-1"}`))

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	es := jwt.NewWithClaims(jwt.SigningMethodES256, valid())
	es.Header["kid"] = "demo-1"
	esToken, _ := es.SignedString(ecKey)

	cases := map[string]string{
		"empty":                   "",
		"garbage":                 "a.b.c",
		"tampered payload":        parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"demo","sub":"u2","aud":"yanshi"}`)) + "." + parts[2],
		"alg none":                noneHeader + "." + parts[1] + ".",
		"alg differs from key":    esToken,
		"unknown kid":             forge(t, demo, "nope", valid()),
		"signed by other line":    forge(t, other, "demo-1", valid()),
		"issuer does not own key": forge(t, other, "other-1", valid()),
		"wrong audience":          forge(t, demo, "demo-1", with("aud", "staging")),
		"expired":                 forge(t, demo, "demo-1", with("exp", t0.Add(-2*time.Minute).Unix())),
		"missing exp":             forge(t, demo, "demo-1", with("exp", nil)),
		"missing iat":             forge(t, demo, "demo-1", with("iat", nil)),
		"lifetime over max_ttl":   forge(t, demo, "demo-1", with("exp", t0.Add(2*time.Hour).Unix())),
		"user token without sub":  forge(t, demo, "demo-1", with("sub", nil)),
		"service token with sub":  forge(t, demo, "demo-1", with("scope", "service")),
		"unknown scope":           forge(t, demo, "demo-1", with("scope", "admin")),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(tok); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
	// 时钟偏差在 Leeway 内可接受。
	if _, err := v.Verify(forge(t, demo, "demo-1", with("exp", t0.Add(-30*time.Second).Unix()))); err != nil {
		t.Fatalf("token within leeway rejected: %v", err)
	}
}

func TestConfigRejectsMismatchedKeys(t *testing.T) {
	demo, _ := GenerateDevKey("demo", "k1")
	bl, _ := demo.Public()
	bl.Keys[0].Alg = "RS256"
	if _, err := NewJWT("yanshi", clock.Real{}, bl); err == nil {
		t.Fatal("accepted an Ed25519 key declared as RS256")
	}
	bl.Keys[0].Alg = "EdDSA"
	if _, err := NewJWT("yanshi", clock.Real{}, bl, bl); err == nil {
		t.Fatal("accepted duplicate business line")
	}
	bl.MaxTTL = 48 * time.Hour
	if _, err := NewJWT("yanshi", clock.Real{}, bl); err == nil {
		t.Fatal("accepted max_ttl above limit")
	}
}

func TestSigningKeyRoundTrip(t *testing.T) {
	k, _ := GenerateDevKey("demo", "demo-1")
	b, err := k.MarshalPEM()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "demo.key")
	if err := writeFile(path, b); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}
	bl, _ := k.Public()
	v, _ := NewJWT("yanshi", clock.Real{}, bl)
	tok, _ := loaded.Sign("yanshi", "u1", time.Minute, time.Now())
	if p, err := v.Verify(tok); err != nil || p.EndUser != "u1" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o600) }
