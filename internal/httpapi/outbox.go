package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	var status int
	var body errorResponse
	switch {
	case errors.Is(err, outbox.ErrInvalid), errors.Is(err, outbox.ErrCursor):
		status, body.Code = http.StatusBadRequest, codeInvalidArgument
	case errors.Is(err, outbox.ErrScope):
		status, body.Code = http.StatusForbidden, codeForbidden
	case errors.Is(err, outbox.ErrShard):
		status, body.Code = http.StatusNotFound, codeNotFound
	case errors.Is(err, outbox.ErrFilter):
		status, body.Code, body.Reason = http.StatusConflict, codeConflict, reasonOutboxFilterChanged
	case errors.Is(err, outbox.ErrEpoch):
		status, body.Code, body.Reason = http.StatusGone, codeGone, reasonOutboxEpochChanged
	case errors.Is(err, outbox.ErrExpired):
		status, body.Code, body.Reason = http.StatusGone, codeGone, reasonOutboxCursorExpired
	default:
		// 数据库错误不回传 SQL 或参数，消费者只需知道本分片不可用。
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "outbox_unavailable", Code: codeOutboxUnavailable})
		return
	}
	body.Error = err.Error()
	writeJSON(w, status, body)
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
	if shards, ok := s.Outbox.(outbox.Shards); ok {
		unsubscribe()
		ch, unsubscribe = shards.SubscribeShard(req.Shard, req.BusinessLine)
	}
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
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthenticated", Code: codeUnauthenticated})
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
	ids := []int{0}
	version, digest := "", ""
	if shards, ok := s.Outbox.(outbox.Shards); ok {
		ids = shards.ShardIDs()
		version, digest = shards.Identity()
	} else {
		next, err := s.Outbox.Head(r.Context(), req)
		if err != nil {
			s.outboxError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"shards": []map[string]any{{"shard": 0, "status": "ready", "next": next}}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	rows := make([]map[string]any, len(ids))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(4, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				part := req
				part.Shard = ids[i]
				next, err := s.Outbox.Head(ctx, part)
				status := "ready"
				if err != nil {
					status, next = "unavailable", ""
				}
				rows[i] = map[string]any{"shard": ids[i], "status": status, "next": next, "slots": s.Outbox.(outbox.Shards).Slots(ids[i])}
			}
		}()
	}
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"shards": rows, "version": version, "identity": digest})
}
