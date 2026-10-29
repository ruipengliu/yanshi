package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yanshi/internal/outbox"
)

func (s *Server) outboxRequest(w http.ResponseWriter, r *http.Request) (outbox.Request, bool) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return outbox.Request{}, false
	}
	q := r.URL.Query()
	if requested := q.Get("business_line"); requested != "" && requested != bl {
		s.fail(w, errForbidden)
		return outbox.Request{}, false
	}
	req := outbox.Request{BusinessLine: bl, After: q.Get("after")}
	var err error
	if q.Has("shard") {
		req.Shard, err = strconv.Atoi(q.Get("shard"))
		if err != nil {
			s.outboxError(w, outbox.ErrInvalid)
			return req, false
		}
	}
	if q.Has("limit") {
		req.Limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || req.Limit < 1 {
			s.outboxError(w, outbox.ErrInvalid)
			return req, false
		}
	}
	if q.Has("kinds") {
		for _, value := range q["kinds"] {
			for _, kind := range strings.Split(value, ",") {
				req.Kinds = append(req.Kinds, outbox.Kind(kind))
			}
		}
	}
	req, err = req.Normalize()
	if err != nil {
		s.outboxError(w, err)
		return req, false
	}
	if s.Outbox == nil {
		s.outboxError(w, errors.New("outbox_unavailable"))
		return req, false
	}
	return req, true
}

func (s *Server) outboxError(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	message := "outbox_unavailable"
	switch {
	case errors.Is(err, outbox.ErrInvalid), errors.Is(err, outbox.ErrCursor):
		code, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, outbox.ErrScope):
		code, message = http.StatusForbidden, err.Error()
	case errors.Is(err, outbox.ErrShard):
		code, message = http.StatusNotFound, err.Error()
	case errors.Is(err, outbox.ErrFilter):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, outbox.ErrEpoch), errors.Is(err, outbox.ErrExpired):
		code, message = http.StatusGone, err.Error()
	}
	// 数据库错误不回传 SQL 或参数，消费者只需知道本分片不可用。
	writeJSON(w, code, map[string]string{"error": message})
}

func (s *Server) readOutbox(w http.ResponseWriter, r *http.Request) {
	req, ok := s.outboxRequest(w, r)
	if !ok {
		return
	}
	wait, recheck := s.OutboxWait, s.OutboxRecheck
	if wait <= 0 {
		wait = 25 * time.Second
	}
	if recheck <= 0 {
		recheck = time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	if expires := principal(r).Expires; !expires.IsZero() {
		var expireCancel context.CancelFunc
		ctx, expireCancel = context.WithDeadline(ctx, expires)
		defer expireCancel()
	}
	ch, unsubscribe := s.Outbox.Subscribe(req.BusinessLine)
	defer unsubscribe()
	ticker := time.NewTicker(recheck)
	defer ticker.Stop()
	var last outbox.Page
	for {
		page, err := s.Outbox.Read(ctx, req)
		if err == nil {
			last = page
			if len(page.Events) > 0 {
				break
			}
			// 每轮保留安全扫描进度，空闲业务线也不会永久拿着旧位置。
			req.After = page.Next
		} else {
			if ctx.Err() != nil {
				break
			}
			s.outboxError(w, err)
			return
		}
		select {
		case <-ctx.Done():
		case <-ch:
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	if r.Context().Err() != nil {
		return
	}
	if expires := principal(r).Expires; !expires.IsZero() && !time.Now().Before(expires) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	if last.Next == "" {
		s.outboxError(w, errors.New("outbox_unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, last)
}

func (s *Server) outboxShards(w http.ResponseWriter, r *http.Request) {
	req, ok := s.outboxRequest(w, r)
	if !ok {
		return
	}
	// H 不依赖旧位置，过期恢复必须能用相同过滤条件取得新的安全起点。
	if req.After != "" {
		s.outboxError(w, outbox.ErrInvalid)
		return
	}
	next, err := s.Outbox.Head(r.Context(), req)
	if err != nil {
		s.outboxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shards": []map[string]any{{"shard": 0, "status": "ready", "next": next}}})
}
