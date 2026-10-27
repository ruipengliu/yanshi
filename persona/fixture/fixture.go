// Package fixture 仅在开发与评测时装配，不提供评分 Check，也不用于生产的公开入口。
//
// 所有路径以 /v1/fixture/{businessLine}/{endUser} 为前缀，身份取自路径：
// GET /state 返回 domain.State；POST /seed 的整个请求体是 domain.SeedData（字段名为 snake_case）。
// POST /bind {project,session}；POST /submitted {session,input}；POST /edit {id,status,due_date}；
// POST /resolve {session,exchange,project}；POST /resolve_merge {id,accept}；POST /archive {}。
// GET /week 返回 domain.WeekPlan；POST /bind_week {from,session} 绑定每周回顾 Session。
// archive 返回项目 ID 数组；其余成功的 POST 返回 {}。错误返回 4xx 与简短英文 error。
// 应用层编排器也可以直接在 store.Update/View 中使用 Book 的同名方法。
package fixture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"persona/domain"
	"persona/store"
)

type Options struct{ Now func() time.Time }

func Handler(s store.Store, opts Options) http.Handler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	prefix := "/v1/fixture/{businessLine}/{endUser}"
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	register := func(method, path string, readOnly bool, action func(*domain.Book, *http.Request) (any, error)) {
		mux.HandleFunc(method+" "+prefix+path, func(w http.ResponseWriter, r *http.Request) {
			var out any
			apply := func(book *domain.Book) error { var err error; out, err = action(book, r); return err }
			transact := s.Update
			if readOnly {
				transact = s.View
			}
			if err := transact(r.Context(), r.PathValue("businessLine"), r.PathValue("endUser"), apply); err != nil {
				write(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			write(w, http.StatusOK, out)
		})
	}
	register("GET", "/state", true, func(b *domain.Book, _ *http.Request) (any, error) { return b.State(), nil })
	register("GET", "/week", true, func(b *domain.Book, _ *http.Request) (any, error) { return b.ReviewWeek(now()), nil })
	register("POST", "/seed", false, func(b *domain.Book, r *http.Request) (any, error) {
		var seed domain.SeedData
		if err := decode(r, &seed); err != nil {
			return nil, err
		}
		b.Seed(seed, now())
		return struct{}{}, nil
	})
	for _, op := range []string{"bind", "submitted", "edit", "resolve", "resolve_merge", "archive", "bind_week"} {
		register("POST", "/"+op, false, func(b *domain.Book, r *http.Request) (any, error) {
			var in struct {
				Project  string `json:"project"`
				Session  string `json:"session"`
				Input    string `json:"input"`
				ID       string `json:"id"`
				Status   string `json:"status"`
				DueDate  string `json:"due_date"`
				Exchange string `json:"exchange"`
				Accept   bool   `json:"accept"`
				From     string `json:"from"`
			}
			if err := decode(r, &in); err != nil {
				return nil, err
			}
			var err error
			switch op {
			case "bind":
				b.Bind(in.Project, in.Session)
			case "submitted":
				b.Submitted(in.Session, in.Input)
			case "edit":
				err = b.Edit(in.ID, in.Status, in.DueDate, now())
			case "resolve":
				err = b.Resolve(in.Session, in.Exchange, in.Project, now())
			case "resolve_merge":
				err = b.ResolveMerge(in.ID, in.Accept)
			case "archive":
				ids := b.Archive(now())
				if ids == nil {
					ids = []string{}
				}
				return ids, nil
			case "bind_week":
				if _, parseErr := time.Parse(time.DateOnly, in.From); parseErr != nil {
					return nil, errDate
				}
				if in.Session == "" {
					return nil, errSession
				}
				b.BindWeek(in.From, in.Session)
			}
			return struct{}{}, err
		})
	}
	return mux
}

// decode 拒绝多份 JSON 和超大请求；非法输入不进入领域事务。
func decode(r *http.Request, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return errJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errJSON
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errJSON
	}
	return nil
}
