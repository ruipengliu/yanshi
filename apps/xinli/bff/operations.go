package bff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"persona/domain"
	"persona/query"
	"persona/store"
	"xinli/account"
	"xinli/kernel"
)

type Operations interface {
	CreateSessionWithOptions(context.Context, string, string, string, kernel.SessionOptions) (string, error)
	ThreadRequest(context.Context, string, string, string, string, any) (json.RawMessage, error)
	Stream(context.Context, string, string, uint64) (io.ReadCloser, error)
	DeleteMemory(context.Context, string, string, string) error
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any, empty bool) bool {
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	err := d.Decode(value)
	if empty && err == io.EOF {
		return true
	}
	if err != nil || d.Decode(new(any)) != io.EOF {
		fail(w, &query.Error{Status: 400, Code: "invalid_request"})
		return false
	}
	return true
}

// userThread 是列表、详情和操作共同的正向允许规则；未知用途默认不可见。
func userThread(owner store.Owner, id, line, user, agent string) bool {
	return id != "" && line == owner.BusinessLine && user == owner.EndUser && agent == "xinli-assistant"
}

func (o Options) threadView(ctx context.Context, owner store.Owner, id string) (kernel.SessionView, error) {
	view, err := o.Kernel.SessionView(ctx, owner.BusinessLine, id)
	if err != nil {
		var remote *kernel.HTTPError
		if errors.As(err, &remote) && (remote.StatusCode == 403 || remote.StatusCode == 404 || remote.StatusCode == 410) {
			return view, domain.ErrQueryNotFound
		}
		return view, err
	}
	if view.ID != id || !userThread(owner, view.ID, view.BusinessLine, view.EndUser, view.Agent.Name) {
		return view, domain.ErrQueryNotFound
	}
	return view, nil
}

func (o Options) operations() (Operations, error) {
	ops, ok := o.Kernel.(Operations)
	if !ok {
		return nil, errors.New("operations unavailable")
	}
	return ops, nil
}

func (o Options) operationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/threads", o.createThread)
	mux.HandleFunc("DELETE /v1/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if decodeBody(w, r, &in, true) {
			o.forward(w, r, http.MethodDelete, "", nil, 204)
		}
	})
	mux.HandleFunc("POST /v1/threads/{id}/inputs", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			InputID string            `json:"input_id"`
			Text    string            `json:"text,omitempty"`
			Content []json.RawMessage `json:"content,omitempty"`
		}
		if !decodeBody(w, r, &in, false) {
			return
		}
		if in.InputID == "" {
			fail(w, &query.Error{Status: 400, Code: "input_id_required"})
			return
		}
		o.forward(w, r, http.MethodPost, "/inputs", in, 202)
	})
	mux.HandleFunc("POST /v1/threads/{id}/redo", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			InputID       string `json:"input_id"`
			ExpectedRunID string `json:"expected_run_id"`
		}
		if !decodeBody(w, r, &in, false) {
			return
		}
		if in.InputID == "" || in.ExpectedRunID == "" {
			fail(w, &query.Error{Status: 400, Code: "invalid_request"})
			return
		}
		o.forward(w, r, http.MethodPost, "/inputs", map[string]any{"redo": true, "input_id": in.InputID, "expected_run_id": in.ExpectedRunID}, 202)
	})
	mux.HandleFunc("POST /v1/threads/{id}/interrupt", o.interrupt)
	mux.HandleFunc("POST /v1/threads/{id}/answers/{call}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Selected []string          `json:"selected,omitempty"`
			Values   map[string]string `json:"values,omitempty"`
			Text     string            `json:"text,omitempty"`
		}
		if decodeBody(w, r, &in, false) {
			o.forward(w, r, http.MethodPost, "/answers/"+url.PathEscape(r.PathValue("call")), in, 204)
		}
	})
	mux.HandleFunc("POST /v1/threads/{id}/approvals/{call}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Approve *bool `json:"approve"`
		}
		if !decodeBody(w, r, &in, false) {
			return
		}
		if in.Approve == nil {
			fail(w, &query.Error{Status: 400, Code: "invalid_request"})
			return
		}
		o.forward(w, r, http.MethodPost, "/approvals/"+url.PathEscape(r.PathValue("call")), in, 204)
	})
	mux.HandleFunc("POST /v1/threads/{id}/card-actions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CardID string            `json:"card_id"`
			CallID string            `json:"call_id"`
			Action string            `json:"action"`
			Values map[string]string `json:"values,omitempty"`
			Seq    json.RawMessage   `json:"seq,omitempty"`
		}
		if decodeBody(w, r, &in, false) {
			// 只包装 ContentBlock；卡片失效、动作校验和输入 ID 都由内核决定。
			o.forward(w, r, http.MethodPost, "/inputs", map[string]any{"content": []any{map[string]any{"card_action": in}}}, 202)
		}
	})
	mux.HandleFunc("POST /v1/threads/{id}/schedules", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ScheduleID string     `json:"schedule_id"`
			Input      string     `json:"input"`
			At         string     `json:"at,omitempty"`
			RRule      string     `json:"rrule,omitempty"`
			TimeZone   string     `json:"time_zone"`
			Until      *time.Time `json:"until,omitempty"`
		}
		if decodeBody(w, r, &in, false) {
			o.forward(w, r, http.MethodPost, "/schedules", in, 201)
		}
	})
	mux.HandleFunc("GET /v1/threads/{id}/schedules", func(w http.ResponseWriter, r *http.Request) {
		o.forward(w, r, http.MethodGet, "/schedules", nil, 200)
	})
	mux.HandleFunc("DELETE /v1/threads/{id}/schedules/{schedule}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if decodeBody(w, r, &in, true) {
			o.forward(w, r, http.MethodDelete, "/schedules/"+url.PathEscape(r.PathValue("schedule")), nil, 204)
		}
	})
	mux.HandleFunc("GET /v1/threads/{id}/questions", o.pending)
	mux.HandleFunc("GET /v1/threads/{id}/approvals", o.pending)
	mux.HandleFunc("GET /v1/threads/{id}/stream", o.stream)
}

func (o Options) forward(w http.ResponseWriter, r *http.Request, method, suffix string, in any, status int) {
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
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
	ops, err := o.operations()
	if err != nil {
		fail(w, err)
		return
	}
	out, err := ops.ThreadRequest(r.Context(), ownerOf(r).BusinessLine, r.PathValue("id"), method, suffix, in)
	if err != nil {
		fail(w, err)
		return
	}
	if status == 204 {
		w.WriteHeader(status)
		return
	}
	query.WriteJSON(w, status, out)
}

// threadKey 把 App 给的键映射为内核幂等键：摘要限制了键长与字符，前缀与编排器的创建意图键分开。
// 内核按 (业务线, EndUser, 键) 派生 Session ID，不同用户的同一个键互不影响。
func threadKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "xt-" + hex.EncodeToString(sum[:])
}

func (o Options) createThread(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeBody(w, r, &in, false) {
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	// 回包丢失时 App 以同一个键重试，得到同一个线程，不会多出空线程（ADR-0059）。
	if in.IdempotencyKey == "" {
		fail(w, &query.Error{Status: 400, Code: "idempotency_key_required"})
		return
	}
	if len(in.IdempotencyKey) > 128 {
		fail(w, &query.Error{Status: 400, Code: "invalid_request"})
		return
	}
	ops, err := o.operations()
	if err != nil {
		fail(w, err)
		return
	}
	owner := ownerOf(r)
	p, _ := account.FromContext(r.Context())
	record, err := o.Accounts.Store.ReadAccount(r.Context(), p.Owner)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := ops.CreateSessionWithOptions(r.Context(), owner.BusinessLine, owner.EndUser, "xinli-assistant", kernel.SessionOptions{TimeZone: record.Account.TimeZone, IdempotencyKey: threadKey(in.IdempotencyKey)})
	if err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 201, map[string]string{"id": id})
}

func (o Options) interrupt(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !decodeBody(w, r, &in, true) {
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	view, err := o.threadView(r.Context(), ownerOf(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	for _, run := range view.Runs {
		if run.Status == "queued" || run.Status == "running" || run.Status == "suspended" {
			o.forward(w, r, http.MethodPost, "/runs/"+url.PathEscape(run.ID)+"/interrupt", nil, 204)
			return
		}
	}
	w.WriteHeader(204)
}
