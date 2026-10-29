package bff

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"persona/query"
	"persona/store"
	"xinli/task"
)

func (o Options) stream(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, query.ErrInvalid)
		return
	}
	for k, v := range values {
		if k != "after" || len(v) != 1 {
			fail(w, query.ErrInvalid)
			return
		}
	}
	cursor := values.Get("after")
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		cursor = h
	}
	var after uint64
	if cursor != "" {
		after, err = strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			fail(w, query.ErrInvalid)
			return
		}
	}
	token := appToken(r)
	var owner store.Owner
	check := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return o.Accounts.WithToken(ctx, token, func(ctx context.Context, p task.Owner, _ string) error {
			if owner.EndUser == "" {
				owner = store.Owner{BusinessLine: p.BusinessLine, EndUser: p.EndUser}
			}
			if o.Persona == nil {
				return &query.Error{Status: 503, Code: "unavailable"}
			}
			if _, err := o.Persona.Read(ctx, owner, query.Request{Resource: "access", Limit: 30}); err != nil {
				return err
			}
			_, err := o.threadView(ctx, owner, r.PathValue("id"))
			return err
		})
	}
	if err := check(r.Context()); err != nil {
		accountError(w, err)
		return
	}
	ops, err := o.operations()
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	interval := o.StreamCheckInterval
	if interval <= 0 || interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	// 不让流占住 owner gate，禁用、注销、登出和刷新才能及时生效。
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if check(ctx) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	body, err := ops.Stream(ctx, owner.BusinessLine, r.PathValue("id"), after)
	if err != nil {
		fail(w, err)
		return
	}
	defer body.Close()
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	if controller.Flush() != nil {
		return
	}
	defer controller.SetWriteDeadline(time.Time{})
	buf := make([]byte, 32<<10)
	for {
		n, readErr := body.Read(buf)
		if ctx.Err() != nil {
			return
		}
		if n > 0 {
			// 慢客户端不能永久占住上游；写入有界，已提交 id 与无 id 的草稿都原样保留。
			_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				cancel()
			}
			return
		}
	}
}
