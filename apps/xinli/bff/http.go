// Package bff 提供 xinli 的产品查询与操作；owner 只来自可撤销应用登录态。
package bff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"persona/command"
	"persona/domain"
	"persona/paging"
	"persona/query"
	"persona/store"
	"xinli/account"
	"xinli/delivery"
	"xinli/kernel"
	"xinli/task"
)

type Kernel interface {
	Sessions(context.Context, string, string) ([]kernel.SessionInfo, error)
	SessionView(context.Context, string, string) (kernel.SessionView, error)
	Events(context.Context, string, string, uint64, int) (kernel.EventPage, error)
	Quota(context.Context, string, string) ([]kernel.QuotaPeriod, error)
}

type Options struct {
	Accounts            *account.Service
	Persona             query.Reader
	Commands            command.Writer
	Kernel              Kernel
	Inbox               delivery.Store
	Tasks               task.Reader
	Now                 func() time.Time
	StreamCheckInterval time.Duration
}

func Handler(opts Options) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	mux := http.NewServeMux()
	for _, route := range query.Routes {
		if route.Resource == "access" {
			continue
		}
		mux.HandleFunc("GET /v1/"+strings.TrimPrefix(route.Path, "/"), func(w http.ResponseWriter, r *http.Request) {
			q, err := query.ParseHTTP(r, route.Resource)
			if err != nil {
				query.WriteError(w, err)
				return
			}
			opts.persona(w, r, q)
		})
	}
	mux.HandleFunc("GET /v1/today", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			query.WriteError(w, query.ErrInvalid)
			return
		}
		p, _ := account.FromContext(r.Context())
		record, err := opts.Accounts.Store.ReadAccount(r.Context(), p.Owner)
		if err != nil {
			fail(w, err)
			return
		}
		zone, err := time.LoadLocation(record.Account.TimeZone)
		if err != nil {
			fail(w, err)
			return
		}
		opts.persona(w, r, query.Request{Resource: "day", Date: opts.Now().In(zone).Format(time.DateOnly), Limit: 30})
	})
	mux.HandleFunc("GET /v1/threads", opts.threads)
	mux.HandleFunc("GET /v1/threads/{id}/events", opts.events)
	mux.HandleFunc("GET /v1/inbox", opts.inbox)
	mux.HandleFunc("POST /v1/inbox/{id}/read", opts.readInbox)
	mux.HandleFunc("GET /v1/tasks", opts.tasks)
	mux.HandleFunc("GET /v1/tasks/{id}", opts.task)
	mux.HandleFunc("GET /v1/entitlements", opts.entitlements)
	opts.operationRoutes(mux)
	opts.personaRoutes(mux)
	opts.accountRoutes(mux)
	if opts.Accounts == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, errors.New("accounts unavailable")) })
	}
	secured := opts.Accounts.Middleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// 未挂载的开发或未来路由保持 404，不因 BFF 的统一鉴权变成已存在接口。
		if _, pattern := mux.Handler(r); pattern == "" {
			mux.ServeHTTP(w, r)
			return
		}
		// 长流自行做有界鉴权；注销进度使用撤销后仍有效的专用凭据。
		if _, pattern := mux.Handler(r); pattern == "GET /v1/threads/{id}/stream" || pattern == "GET /v1/account/deletion" {
			mux.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		secured.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ownerOf(r *http.Request) store.Owner {
	p, _ := account.FromContext(r.Context())
	return store.Owner{BusinessLine: p.Owner.BusinessLine, EndUser: p.Owner.EndUser}
}

func (o Options) persona(w http.ResponseWriter, r *http.Request, q query.Request) {
	if o.Persona == nil {
		fail(w, errors.New("persona unavailable"))
		return
	}
	out, err := o.Persona.Read(r.Context(), ownerOf(r), q)
	if err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, out)
}

// 内核与应用任务查询也经过 Persona 注销屏障，不能在跨库清理途中返回业务数据。
func (o Options) accessible(r *http.Request) error {
	if o.Persona == nil {
		return errors.New("persona unavailable")
	}
	_, err := o.Persona.Read(r.Context(), ownerOf(r), query.Request{Resource: "access", Limit: 30})
	return err
}

func pageRequest(r *http.Request, owner store.Owner, resource string) (int, string, string, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", "", query.ErrInvalid
	}
	for k, v := range values {
		if (k != "limit" && k != "cursor") || len(v) != 1 {
			return 0, "", "", query.ErrInvalid
		}
	}
	limit, err := paging.Limit(values)
	if err != nil {
		return 0, "", "", err
	}
	scope := paging.Scope(owner.BusinessLine, owner.EndUser, resource, r.PathValue("id"))
	after, err := paging.Decode(values.Get("cursor"), scope)
	return limit, after, scope, err
}

func (o Options) threads(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	limit, after, scope, err := pageRequest(r, owner, "threads")
	if err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	items, err := o.Kernel.Sessions(r.Context(), owner.BusinessLine, owner.EndUser)
	if err != nil {
		fail(w, err)
		return
	}
	out := []kernel.SessionInfo{}
	seen := map[string]bool{}
	for _, item := range items {
		if item.EndUser != owner.EndUser || item.ID == "" {
			fail(w, errors.New("invalid owner"))
			return
		}
		// 正向允许问答用途；新 Agent、day/project/week/brief 都不会意外变成线程。
		if userThread(owner, item.ID, owner.BusinessLine, item.EndUser, item.Agent) && !seen[item.ID] {
			out = append(out, item)
			seen[item.ID] = true
		}
	}
	// 内核当前按可变的最后输入时间分页。产品游标改用不可变 ID，输入不会移动已展示线程。
	slices.SortFunc(out, func(a, b kernel.SessionInfo) int { return strings.Compare(a.ID, b.ID) })
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, paging.Slice(out, limit, after, scope, func(s kernel.SessionInfo) string { return s.ID }))
}

func (o Options) thread(r *http.Request) error {
	_, err := o.threadView(r.Context(), ownerOf(r), r.PathValue("id"))
	return err
}

func (o Options) events(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	limit, key, scope, err := pageRequest(r, owner, "events")
	var after uint64
	if err == nil && key != "" {
		after, err = strconv.ParseUint(key, 10, 64)
		if err != nil {
			err = query.ErrInvalid
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	if err := o.thread(r); err != nil {
		fail(w, err)
		return
	}
	page, err := o.Kernel.Events(r.Context(), owner.BusinessLine, r.PathValue("id"), after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	// 读完后复查可见性，删除或注销过程中读到的旧日志整页丢弃。
	if err := o.thread(r); err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	out := paging.Page[json.RawMessage]{Items: page.Events}
	if len(page.Events) == limit {
		out.NextCursor = paging.Encode(scope, strconv.FormatUint(page.Last, 10))
	}
	query.WriteJSON(w, 200, out)
}

type taskView struct {
	ID        string `json:"id"`
	Date      string `json:"date,omitempty"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
}

func visibleTask(t task.Task) taskView {
	status := "processing"
	switch t.State {
	case task.Succeeded:
		status = "completed"
	case task.Failed:
		status = "failed"
	case task.Terminated:
		status = "cancelled"
	}
	if t.Operation == "delete" && status == "processing" {
		status = "deleting"
	}
	return taskView{ID: t.TaskID, Date: t.Date, Operation: t.Operation, Status: status}
}

func (o Options) tasks(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	limit, key, scope, err := pageRequest(r, owner, "tasks")
	var after int64
	if err == nil && key != "" {
		after, err = strconv.ParseInt(key, 10, 64)
		if err != nil || after < 0 {
			err = query.ErrInvalid
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	p, _ := account.FromContext(r.Context())
	items, err := o.Tasks.List(r.Context(), p.Owner, after, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	out := paging.Page[taskView]{Items: []taskView{}}
	for _, item := range items[:min(limit, len(items))] {
		out.Items = append(out.Items, visibleTask(item))
	}
	if len(items) > limit {
		out.NextCursor = paging.Encode(scope, strconv.FormatInt(items[limit-1].Sequence, 10))
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, out)
}

func (o Options) task(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	p, _ := account.FromContext(r.Context())
	item, err := o.Tasks.Read(r.Context(), p.Owner, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, visibleTask(item))
}

func (o Options) entitlements(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	owner := ownerOf(r)
	periods, err := o.Kernel.Quota(r.Context(), owner.BusinessLine, owner.EndUser)
	if err != nil {
		fail(w, err)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, struct {
		Periods []kernel.QuotaPeriod `json:"periods"`
	}{periods})
}

func fail(w http.ResponseWriter, err error) {
	var remote *kernel.HTTPError
	if errors.As(err, &remote) {
		code := map[int]string{400: "invalid_request", 401: "upstream_unauthorized", 403: "forbidden", 404: "not_found", 409: "conflict", 410: "gone", 413: "too_large", 422: "rejected", 429: "quota_exceeded"}[remote.StatusCode]
		if code != "" {
			body := map[string]string{"error": code}
			if remote.Reason != "" {
				body["reason"] = remote.Reason
			}
			query.WriteJSON(w, remote.StatusCode, body)
			return
		}
	}

	if errors.Is(err, task.ErrNotFound) || errors.Is(err, kernel.ErrNotFound) {
		err = domain.ErrQueryNotFound
	}
	query.WriteError(w, err)
}
