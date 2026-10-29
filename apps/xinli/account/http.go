package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"xinli/task"
)

type principalKey struct{}
type Principal struct {
	Owner    task.Owner
	DeviceID string
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
func bearer(r *http.Request) string {
	prefix := "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimPrefix(value, prefix)
}

// Middleware 由 BFF 包裹有界请求；长流须逐次读取时重新验证，不能把首次鉴权当永久授权。
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := s.WithToken(r.Context(), bearer(r), func(ctx context.Context, owner task.Owner, device string) error {
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, principalKey{}, Principal{owner, device})))
			return nil
		})
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	})
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler 只提供账号登录面；产品数据面在 B1/B2，管理权益和禁用不暴露给 App。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BusinessLine string `json:"business_line"`
			Proof        string `json:"proof"`
			DeviceID     string `json:"device_id"`
			Platform     string `json:"platform"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := s.Login(r.Context(), in.BusinessLine, in.Proof, in.DeviceID, in.Platform)
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := s.Refresh(r.Context(), in.RefreshToken)
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		writeJSON(w, 200, out)
	})
	for _, path := range []string{"logout", "logout_all"} {
		mux.Handle("POST /v1/auth/"+path, s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := FromContext(r.Context())
			device := p.DeviceID
			if path == "logout_all" {
				device = ""
			}
			if err := s.Logout(r.Context(), p.Owner, device); err != nil {
				http.Error(w, "logout failed", 503)
				return
			}
			w.WriteHeader(204)
		})))
	}
	mux.Handle("GET /v1/auth/account", s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		record, err := s.Store.ReadAccount(r.Context(), p.Owner)
		if err != nil {
			http.Error(w, "read failed", 503)
			return
		}
		// 有效档位来自内核成功回包；不把尚未同步的权益冒充配额。
		writeJSON(w, 200, struct {
			Owner       task.Owner        `json:"owner"`
			TimeZone    string            `json:"time_zone"`
			Preferences json.RawMessage   `json:"preferences,omitempty"`
			Tier        string            `json:"tier"`
			Devices     map[string]Device `json:"devices"`
		}{p.Owner, record.Account.TimeZone, record.Account.Preferences, record.Account.EffectiveTier.Name, record.Devices})
	})))
	mux.Handle("DELETE /v1/auth/devices/{id}", s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		if err := s.Logout(r.Context(), p.Owner, r.PathValue("id")); err != nil {
			http.Error(w, "revoke failed", 503)
			return
		}
		w.WriteHeader(204)
	})))
	mux.Handle("DELETE /v1/auth/account", s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		token, err := s.RequestDeletion(r.Context(), p.Owner)
		if err != nil {
			http.Error(w, "deletion failed", 503)
			return
		}
		writeJSON(w, 202, map[string]string{"deletion_token": token})
	})))
	mux.HandleFunc("GET /v1/auth/deletion", func(w http.ResponseWriter, r *http.Request) {
		progress, err := s.DeletionWithToken(r.Context(), bearer(r))
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		writeJSON(w, 200, progress)
	})
	return mux
}
