package account

import (
	"context"
	"net/http"
	"strings"

	"persona/domain"
	"persona/store"
	"xinli/task"
)

// PersonaAccess 供采集和夹具使用；注销驱动必须持有原始 Store，清理不受 active 限制。
// 原文写入与账号禁用共享 owner gate，避免只在鉴权时检查留下写入窗口。
type PersonaAccess struct {
	store.Store
	Guard Guard
}

func (s PersonaAccess) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Guard.WithActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error { return s.Store.Update(ctx, bl, eu, fn) })
}
func (s PersonaAccess) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Guard.WithActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error { return s.Store.View(ctx, bl, eu, fn) })
}

// DevelopmentHandler 只给回环 dev 的既有夹具加状态闸门；不是生产鉴权替代品。
func (s *Service) DevelopmentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "v1" || parts[1] != "dev" && parts[1] != "fixture" {
			next.ServeHTTP(w, r)
			return
		}
		err := s.WithActive(r.Context(), task.Owner{BusinessLine: parts[2], EndUser: parts[3]}, func(ctx context.Context) error { next.ServeHTTP(w, r.WithContext(ctx)); return nil })
		if err != nil {
			http.Error(w, "account inactive", 403)
		}
	})
}
