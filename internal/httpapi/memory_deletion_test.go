package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
)

// 删除记录而非已清空的投影提供授权依据；拒绝访问时不能顺带清掉保留的 Memory。
func TestDeletedSessionMemoryAuthorization(t *testing.T) {
	for _, completed := range []bool{false, true} {
		for _, tc := range []struct {
			name, who, query string
			want             int
		}{
			{"service", "demo/", "", http.StatusNoContent},
			{"service_ignores_query", "demo/", "?business_line=other", http.StatusNoContent},
			{"other_service", "other/", "?business_line=demo", http.StatusNotFound},
			{"owner", "demo/u1", "?business_line=demo", http.StatusNotFound},
			{"other_user", "demo/u2", "", http.StatusNotFound},
			{"other_line_user", "other/u1", "", http.StatusNotFound},
		} {
			stage := "deleting/"
			if completed {
				stage = "deleted/"
			}
			t.Run(stage+tc.name, func(t *testing.T) {
				e := newAuthzEnv(t)
				ctx := context.Background()
				if err := e.api.Service.Delete(ctx, e.sid, lifecycle.ReasonErasure, ""); err != nil {
					t.Fatal(err)
				}
				if completed {
					if err := e.api.Service.Store.Log.Delete(ctx, e.sid); err != nil {
						t.Fatal(err)
					}
					if err := e.api.Service.Deletions.Complete(ctx, e.sid, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := e.api.Memory.Store.Get(ctx, "u1", e.memory); err != nil {
					t.Fatalf("erasure lost memory: %v", err)
				}
				path := "/v1/sessions/" + e.sid + "/memories" + tc.query
				code, body := e.do(t, http.MethodDelete, path, e.token(t, tc.who, time.Hour), nil)
				if code != tc.want {
					t.Fatalf("delete: %d %s, want %d", code, body, tc.want)
				}
				_, err := e.api.Memory.Store.Get(ctx, "u1", e.memory)
				if tc.want == http.StatusNoContent {
					if !errors.Is(err, memory.ErrNotFound) || body != "" {
						t.Fatalf("memory remains or response already written: %v %q", err, body)
					}
					if code, body := e.do(t, http.MethodDelete, path, e.token(t, tc.who, time.Hour), nil); code != http.StatusNoContent {
						t.Fatalf("retry: %d %s", code, body)
					}
				} else if err != nil {
					t.Fatalf("denied request changed memory: %v", err)
				}
			})
		}
	}
}

func TestForgetSessionMemoriesWithoutDeletionRecord(t *testing.T) {
	e := newAuthzEnv(t)
	ctx := context.Background()
	// 模拟日志不存在但没有删除记录；不能仅凭请求中的 Session ID 撤销 Memory。
	if err := e.api.Service.Store.Log.Delete(ctx, e.sid); err != nil {
		t.Fatal(err)
	}
	code, body := e.do(t, http.MethodDelete, "/v1/sessions/"+e.sid+"/memories", e.token(t, "demo/", time.Hour), nil)
	if code != http.StatusNotFound {
		t.Fatalf("no tombstone: %d %s", code, body)
	}
	if _, err := e.api.Memory.Store.Get(ctx, "u1", e.memory); err != nil {
		t.Fatalf("memory changed: %v", err)
	}
}

func TestDeletedSessionMemoriesUnrestricted(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?business_line=demo", http.StatusNoContent},
		{"?business_line=other", http.StatusNotFound},
		{"", http.StatusBadRequest},
	} {
		t.Run(tc.query, func(t *testing.T) {
			e := newAuthzEnv(t)
			ctx := context.Background()
			if err := e.api.Service.Delete(ctx, e.sid, lifecycle.ReasonErasure, ""); err != nil {
				t.Fatal(err)
			}
			if err := e.api.Service.Store.Log.Delete(ctx, e.sid); err != nil {
				t.Fatal(err)
			}
			if err := e.api.Service.Deletions.Complete(ctx, e.sid, time.Now()); err != nil {
				t.Fatal(err)
			}
			// 复制装配，不在正在服务的测试 HTTP Server 上改鉴权配置。
			api := *e.api
			api.Auth = nil
			h := api.Handler()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+e.sid+"/memories"+tc.query, nil))
			if w.Code != tc.want {
				t.Fatalf("unrestricted: %d %s", w.Code, w.Body.String())
			}
			_, err := e.api.Memory.Store.Get(ctx, "u1", e.memory)
			if tc.want == http.StatusNoContent {
				if !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("memory remains: %v", err)
				}
			} else if err != nil {
				t.Fatalf("denied request changed memory: %v", err)
			}
		})
	}
}
