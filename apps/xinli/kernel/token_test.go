package kernel

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func writeKey(t *testing.T, path, line, kid string, key any) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Headers: map[string]string{"business_line": line, "kid": kid}, Bytes: der})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSigningTokenRefreshConcurrentAndRotation(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var unix atomic.Int64
	unix.Store(now.Unix())
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	writeKey(t, path, "bl", "old", key)
	source, err := NewTokenSource(map[string]TokenLocation{"bl": {KeyFile: path}}, TokenOptions{Now: func() time.Time {
		return time.Unix(unix.Load(), 0)
	}})
	if err != nil {
		t.Fatal(err)
	}
	get := func() string {
		value, err := source.Token(context.Background(), "bl")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := get()
	var claims serviceClaims
	parsed, err := jwt.ParseWithClaims(first, &claims, func(token *jwt.Token) (any, error) {
		return pub, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("yanshi"), jwt.WithTimeFunc(func() time.Time {
		return now
	}))
	if err != nil || !parsed.Valid || parsed.Header["kid"] != "old" || claims.Issuer != "bl" || claims.Subject != "" || claims.Scope != "service" || claims.ExpiresAt.Sub(claims.IssuedAt.Time) != 15*time.Minute {
		t.Fatalf("JWT contract: %v", err)
	}
	unix.Store(now.Add(14 * time.Minute).Unix())
	_, rotated, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, path, "bl", "new", rotated)
	var wg sync.WaitGroup
	values := make(chan string, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := source.Token(context.Background(), "bl")
			if err != nil {
				t.Error(err)
			}
			values <- value
		}()
	}
	wg.Wait()
	close(values)
	refreshed := ""
	for value := range values {
		if value == first || value == "" {
			t.Fatal("not refreshed")
		}
		if refreshed != "" && refreshed != value {
			t.Fatal("concurrent refresh returned different tokens")
		}
		refreshed = value
	}
	var next serviceClaims
	token, _, err := jwt.NewParser().ParseUnverified(refreshed, &next)
	if err != nil || token.Header["kid"] != "new" {
		t.Fatal("key rotation ignored")
	}
	if _, err := source.Token(context.Background(), "other"); err == nil {
		t.Fatal("unknown business line accepted")
	}
	unix.Store(now.Add(28 * time.Minute).Unix())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background(), "bl"); err == nil {
		t.Fatal("refresh failure used stale token")
	}
}

func TestSigningAlgorithms(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		key    any
		public any
	}{{"ES256", ec, &ec.PublicKey}, {"RS256", rsaKey, &rsaKey.PublicKey}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key.pem")
			writeKey(t, path, "bl", "kid", tc.key)
			source, err := NewTokenSource(map[string]TokenLocation{"bl": {KeyFile: path}}, TokenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			value, err := source.Token(context.Background(), "bl")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jwt.Parse(value, func(*jwt.Token) (any, error) {
				return tc.public, nil
			}, jwt.WithValidMethods([]string{tc.name}), jwt.WithAudience("yanshi")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStaticTokenSourcesReloadAndReject(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(line string, expires time.Time) string {
		token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, serviceClaims{Scope: "service", RegisteredClaims: jwt.RegisteredClaims{Issuer: line, Audience: jwt.ClaimStrings{"yanshi"}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires)}}).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	path := filepath.Join(t.TempDir(), "token")
	first, second := sign("bl", now.Add(time.Hour)), sign("bl", now.Add(2*time.Hour))
	for _, kind := range []string{"env", "file"} {
		t.Run(kind, func(t *testing.T) {
			location := TokenLocation{TokenEnv: "TEST_STATIC_TOKEN"}
			put := func(value string) {
				t.Setenv("TEST_STATIC_TOKEN", value)
			}
			if kind == "file" {
				location = TokenLocation{TokenFile: path}
				put = func(value string) {
					if err := os.WriteFile(path, []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			source, err := NewTokenSource(map[string]TokenLocation{"bl": location}, TokenOptions{Now: func() time.Time {
				return now
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{first, second} {
				put(value)
				got, err := source.Token(context.Background(), "bl")
				if err != nil || got != value {
					t.Fatal("static source did not reload")
				}
			}
			for _, value := range []string{"", "private-invalid", sign("other", now.Add(time.Hour)), sign("bl", now.Add(time.Minute)), sign("bl", now.Add(-time.Minute))} {
				put(value)
				if _, err := source.Token(context.Background(), "bl"); err != errToken {
					t.Fatal("unsafe static token accepted")
				}
			}
		})
	}
}

func TestClientBusinessLineIsolation(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+r.URL.Query().Get("business_line") {
			t.Error("wrong business line token")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Tokens: lineTokens{}}
	var wg sync.WaitGroup
	for _, line := range []string{"a", "b"} {
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := client.Deletion(context.Background(), line, "s"); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	for _, ctx := range []context.Context{context.Background(), WithBusinessLine(WithBusinessLine(context.Background(), "a"), "b")} {
		if _, err := client.Session(ctx, "s"); err == nil {
			t.Fatal("missing or conflicting business line accepted")
		}
	}
	if _, err := client.Deletion(WithBusinessLine(context.Background(), "a"), "b", "s"); err == nil {
		t.Fatal("explicit business line overrode context")
	}
	if calls.Load() != 40 {
		t.Fatal("invalid context reached HTTP")
	}
}

type lineTokens struct{}

func (lineTokens) Token(_ context.Context, line string) (string, error) {
	return line, nil
}
