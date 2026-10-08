// Package httpapi 以 HTTP/JSON + SSE 暴露 Session 操作。
//
//	POST /v1/sessions                              创建 Session
//	GET  /v1/sessions/{id}                         Session 投影
//	POST /v1/sessions/{id}/inputs                  提交输入（新 Run 或 Steer）
//	POST /v1/sessions/{id}/runs/{run}/interrupt    中断 Run
//	GET  /v1/sessions/{id}/events?after=N&limit=M  已提交事件（JSON）
//	GET  /v1/sessions/{id}/stream?after=N          已提交事件 + 实时增量（SSE，支持 Last-Event-ID 续传）
//	POST /v1/sessions/{id}/approvals/{call}        审批决定 {"approve": bool}
//	GET  /v1/nodes?business_line=&end_user=        EndUser 的 Node 列表
//
// M0 无鉴权，仅用于单机开发。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

type Server struct {
	Service *service.Service
	Live    live.Bus
	// Nodes 为 nil 时不提供 Node 相关信息。
	Nodes  node.Directory
	Logger *slog.Logger
}

var pj = protojson.MarshalOptions{UseProtoNames: true}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", s.create)
	mux.HandleFunc("GET /v1/sessions/{id}", s.get)
	mux.HandleFunc("POST /v1/sessions/{id}/inputs", s.submit)
	mux.HandleFunc("POST /v1/sessions/{id}/runs/{run}/interrupt", s.interrupt)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.events)
	mux.HandleFunc("GET /v1/sessions/{id}/stream", s.stream)
	mux.HandleFunc("POST /v1/sessions/{id}/approvals/{call}", s.decide)
	mux.HandleFunc("GET /v1/nodes", s.listNodes)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, service.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, service.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, service.ErrConflict):
		code = http.StatusConflict
	}
	if code == http.StatusInternalServerError && s.Logger != nil {
		s.Logger.Error("request failed", "err", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("%w: %v", service.ErrInvalid, err)
	}
	return nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BusinessLine string `json:"business_line"`
		EndUser      string `json:"end_user"`
		Agent        string `json:"agent"`
		AgentVersion string `json:"agent_version"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id, err := s.Service.Create(r.Context(), service.CreateRequest{
		BusinessLine: req.BusinessLine, EndUser: req.EndUser, Agent: req.Agent, AgentVersion: req.AgentVersion,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"session_id": id})
}

type runView struct {
	RunID   string       `json:"run_id"`
	Status  string       `json:"status"`
	Attempt uint32       `json:"attempt"`
	Turns   int          `json:"turns"`
	Waiting *waitingView `json:"waiting,omitempty"`
}

// waitingView 说明挂起中的 Run 在等什么。
type waitingView struct {
	// Kind 为 "approval" 或 "node"。
	Kind       string    `json:"kind"`
	CallID     string    `json:"call_id"`
	Capability string    `json:"capability"`
	Summary    string    `json:"summary,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeOnline *bool     `json:"node_online,omitempty"`
	Deadline   time.Time `json:"deadline"`
}

func (s *Server) waiting(r *http.Request, run *session.Run) *waitingView {
	c := run.PendingCall()
	if run.Status.Terminal() || c == nil {
		return nil
	}
	switch {
	case c.AwaitingApproval():
		return &waitingView{Kind: "approval", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			Summary: c.Approval.Summary, Deadline: c.Approval.Deadline}
	case c.Dispatched():
		v := &waitingView{Kind: "node", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			NodeID: c.NodeID, Deadline: c.Deadline}
		if s.Nodes != nil {
			if info, err := s.Nodes.Get(r.Context(), c.NodeID); err == nil {
				v.NodeOnline = &info.Online
			}
		}
		return v
	}
	return nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	st, err := s.Service.Load(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	runs := []runView{}
	for _, run := range st.Runs {
		runs = append(runs, runView{RunID: run.ID, Status: run.Status.String(), Attempt: run.Attempt, Turns: run.Turns,
			Waiting: s.waiting(r, run)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    st.SessionID,
		"seq":           st.Seq,
		"business_line": st.Created.GetBusinessLine(),
		"end_user":      st.Created.GetEndUser(),
		"agent":         map[string]string{"name": st.Created.GetAgent().GetName(), "version": st.Created.GetAgent().GetVersion()},
		"runs":          runs,
	})
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		// Content 是 ContentBlock 的 JSON 数组；与 Text 同时给出时 Text 在前。
		Content []json.RawMessage `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var input []*v1.ContentBlock
	if req.Text != "" {
		input = model.TextBlocks(req.Text)
	}
	for _, raw := range req.Content {
		b := &v1.ContentBlock{}
		if err := protojson.Unmarshal(raw, b); err != nil {
			s.fail(w, fmt.Errorf("%w: content: %v", service.ErrInvalid, err))
			return
		}
		input = append(input, b)
	}
	res, err := s.Service.Submit(r.Context(), r.PathValue("id"), input)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": res.RunID, "steered": res.Steered})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.Service.Interrupt(r.Context(), r.PathValue("id"), r.PathValue("run")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approve *bool  `json:"approve"`
		By      string `json:"by"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if req.Approve == nil {
		s.fail(w, fmt.Errorf("%w: approve is required", service.ErrInvalid))
		return
	}
	if req.By == "" {
		req.By = "api"
	}
	if err := s.Service.Decide(r.Context(), r.PathValue("id"), r.PathValue("call"), *req.Approve, req.By); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type nodeView struct {
	NodeID       string   `json:"node_id"`
	Label        string   `json:"label"`
	Kind         string   `json:"kind"`
	HostApp      string   `json:"host_app"`
	Online       bool     `json:"online"`
	Capabilities []string `json:"capabilities"`
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	if s.Nodes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"nodes": []nodeView{}})
		return
	}
	q := r.URL.Query()
	nodes, err := s.Nodes.List(r.Context(), node.Scope{BusinessLine: q.Get("business_line"), EndUser: q.Get("end_user")})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []nodeView{}
	for _, n := range nodes {
		v := nodeView{NodeID: n.NodeID, Label: n.Label, Kind: n.Kind, HostApp: n.HostApp, Online: n.Online, Capabilities: []string{}}
		for _, c := range n.Capabilities {
			v.Capabilities = append(v.Capabilities, c.GetName())
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a non-negative integer", service.ErrInvalid, s)
	}
	return n, nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Service.Load(r.Context(), id); err != nil {
		s.fail(w, err)
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
		b, err := pj.Marshal(e)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Service.Load(r.Context(), id); err != nil {
		s.fail(w, err)
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
	ctx := r.Context()
	// 先订阅增量再读日志，避免两者之间的增量丢失。
	deltas, unsubscribe := s.Live.Subscribe(id)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	heads := make(chan struct{}, 1)
	go waitHeads(ctx, s.Service.Store.Log, id, after, heads)
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		events, err := eventlog.ReadAll(ctx, s.Service.Store.Log, id, after)
		if err != nil {
			return
		}
		for _, e := range events {
			b, err := pj.Marshal(e)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n", e.GetSeq(), b)
			after = e.GetSeq()
		}
		flusher.Flush()

		select {
		case <-ctx.Done():
			return
		case <-heads:
		case d := <-deltas:
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "event: delta\ndata: %s\n\n", b)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
	}
}

// waitHeads 每当日志前进时向 heads 发一个合并后的通知。
func waitHeads(ctx context.Context, log eventlog.Log, id string, after uint64, heads chan<- struct{}) {
	for {
		head, err := log.Wait(ctx, id, after)
		if err != nil {
			return
		}
		after = head
		select {
		case heads <- struct{}{}:
		default:
		}
	}
}
