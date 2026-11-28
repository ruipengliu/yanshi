package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/feed"
	"yanshi/internal/live"
)

var pj = protojson.MarshalOptions{UseProtoNames: true}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.session(w, r); !ok {
		return
	}
	after, err := parseUint(r.URL.Query().Get("after"))
	if err != nil {
		s.fail(w, err)
		return
	}
	limit, err := parseUint(r.URL.Query().Get("limit"))
	if err != nil {
		s.fail(w, err)
		return
	}
	events, err := s.Service.Store.Log.Read(r.Context(), id, after, int(limit))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		b, err := pj.Marshal(feed.Public(e))
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("after")
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		cursor = h
	}
	after, err := parseUint(cursor)
	if err != nil {
		s.fail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	// 令牌到期时断开，客户端带新令牌与 Last-Event-ID 重连（docs/design/auth.md §4）。
	if exp := principal(r).Expires; !exp.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, exp)
		defer cancel()
	}
	src := feed.Source{Log: s.Service.Store.Log, Live: s.Live, Deletions: s.Service.Deletions, Hub: s.Feeds}
	err = src.Stream(ctx, st, after, sseSink{w: w, f: flusher})
	switch {
	case errors.Is(err, feed.ErrDeleted):
		fmt.Fprint(w, "event: session_deleted\ndata: {}\n\n")
	case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
		fmt.Fprint(w, "event: token_expired\ndata: {}\n\n")
	}
}

// sseSink 把事件流写成 SSE：已提交事件带 id（seq），供 Last-Event-ID 续传。
type sseSink struct {
	w io.Writer
	f http.Flusher
}

func (k sseSink) Event(e *v1.Event) error {
	b, err := pj.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(k.w, "id: %d\nevent: event\ndata: %s\n\n", e.GetSeq(), b)
	return err
}

func (k sseSink) Delta(d live.Delta) error {
	b, _ := json.Marshal(d)
	_, err := fmt.Fprintf(k.w, "event: delta\ndata: %s\n\n", b)
	return err
}

func (k sseSink) Ping() error {
	_, err := fmt.Fprint(k.w, ": ping\n\n")
	return err
}

func (k sseSink) Flush() error {
	k.f.Flush()
	return nil
}
