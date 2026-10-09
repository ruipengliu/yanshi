// Package httpapi 以 HTTP/JSON + SSE 暴露 Session 操作。
//
//	POST /v1/sessions                              创建 Session
//	GET  /v1/sessions?end_user=&cursor=&limit=     列出 Session（按最近输入倒序）
//	GET  /v1/sessions/{id}                         Session 投影
//	POST /v1/sessions/{id}/close                   关闭 Session（不可逆）
//	DELETE /v1/sessions/{id}                       删除 Session（异步，202）
//	DELETE /v1/end_users/{id}                      删除 EndUser 的全部数据（仅服务令牌，异步，202）
//	GET  /v1/deletions/{id}                        删除请求的进度（仅服务令牌）
//	POST /v1/sessions/{id}/inputs                  提交输入（新 Run 或 Steer）
//	POST /v1/sessions/{id}/runs/{run}/interrupt    中断 Run
//	GET  /v1/sessions/{id}/events?after=N&limit=M  已提交事件（JSON）
//	GET  /v1/sessions/{id}/stream?after=N          已提交事件 + 实时增量（SSE，支持 Last-Event-ID 续传）
//	POST /v1/sessions/{id}/approvals/{call}        审批决定 {"approve": bool}
//	GET  /v1/nodes?business_line=&end_user=        EndUser 的 Node 列表
//	POST /v1/sessions/{id}/artifacts?name=         上传工件（请求体为文件内容，Content-Type 为 MIME 类型）
//	GET  /v1/sessions/{id}/artifacts               Session 的工件列表
//	GET  /v1/artifacts/{id}                        下载工件
//	GET  /v1/artifacts/{id}/meta                   工件元数据
//
// 除 /healthz 外，所有请求须携带业务线签发的令牌（Authorization: Bearer），
// 调用方只能访问自己的 Session、Node 与工件，其余一律 404（docs/design/auth.md）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/auth"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

type Server struct {
	// Auth 校验令牌；nil 时等同 auth.Insecure（信任自报身份，仅限回环地址开发）。
	Auth    auth.Verifier
	Service *service.Service
	Live    live.Bus
	// Nodes 为 nil 时不提供 Node 相关信息。
	Nodes node.Directory
	// Artifacts 为 nil 时不提供工件接口。
	Artifacts *artifact.Service
	Logger    *slog.Logger
}

var pj = protojson.MarshalOptions{UseProtoNames: true}

// routes 是全部需要鉴权的接口；授权矩阵测试据此检查每个接口都被覆盖。
func (s *Server) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /v1/sessions":                           s.create,
		"GET /v1/sessions":                            s.list,
		"GET /v1/sessions/{id}":                       s.get,
		"POST /v1/sessions/{id}/close":                s.close,
		"DELETE /v1/sessions/{id}":                    s.delete,
		"DELETE /v1/end_users/{id}":                   s.deleteEndUser,
		"GET /v1/deletions/{id}":                      s.deletion,
		"POST /v1/sessions/{id}/inputs":               s.submit,
		"POST /v1/sessions/{id}/runs/{run}/interrupt": s.interrupt,
		"GET /v1/sessions/{id}/events":                s.events,
		"GET /v1/sessions/{id}/stream":                s.stream,
		"POST /v1/sessions/{id}/approvals/{call}":     s.decide,
		"GET /v1/nodes":                               s.listNodes,
		"POST /v1/sessions/{id}/artifacts":            s.uploadArtifact,
		"GET /v1/sessions/{id}/artifacts":             s.listArtifacts,
		"GET /v1/artifacts/{id}":                      s.downloadArtifact,
		"GET /v1/artifacts/{id}/meta":                 s.artifactMeta,
	}
}

// Routes 返回全部需要鉴权的接口模式。
func (s *Server) Routes() []string {
	var out []string
	for p := range s.routes() {
		out = append(out, p)
	}
	return out
}

func (s *Server) Handler() http.Handler {
	v := s.Auth
	if v == nil {
		v = auth.Insecure{}
	}
	api := http.NewServeMux()
	for p, h := range s.routes() {
		api.Handle(p, instrument(p, h))
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", auth.Middleware(v, api))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// statusWriter 记录响应码；保留 Flush 以支持 SSE。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// instrument 按路由模式记录请求数与耗时（模式而非路径，避免标签基数随 ID 增长）。
func instrument(route string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h(sw, r)
		metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
		metrics.HTTPDuration.WithLabelValues(route).Observe(metrics.Since(start))
	})
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// errForbidden 用于调用方自报的身份与令牌不符；访问别人的资源则返回 404，不泄露其存在。
var errForbidden = errors.New("forbidden")

// session 读取 Session 并要求它属于调用方；否则按不存在处理。
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*session.State, bool) {
	id := r.PathValue("id")
	st, err := s.Service.Load(r.Context(), id)
	if err == nil && !principal(r).Allows(st.Created.GetBusinessLine(), st.Created.GetEndUser()) {
		err = fmt.Errorf("%w: session %s", service.ErrNotFound, id)
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return st, true
}

// artifact 读取工件元数据并要求其所属 Session 属于调用方。
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) (*artifact.Meta, bool) {
	if !s.artifacts(w) {
		return nil, false
	}
	id := r.PathValue("id")
	m, err := s.Artifacts.Stat(r.Context(), id)
	if err == nil {
		st, lerr := s.Service.Load(r.Context(), m.SessionID)
		if lerr != nil || !principal(r).Allows(st.Created.GetBusinessLine(), st.Created.GetEndUser()) {
			err = fmt.Errorf("%w: artifact %s", artifact.ErrNotFound, id)
		}
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return m, true
}

// claim 返回调用方以 (businessLine, endUser) 身份行事时实际生效的值：令牌中有的以令牌为准，
// 请求中自报的值只能为空或与令牌一致。服务令牌须由请求给出 endUser。
func claim(p auth.Principal, businessLine, endUser string) (string, string, error) {
	if p.Unrestricted {
		return businessLine, endUser, nil
	}
	if businessLine != "" && businessLine != p.BusinessLine {
		return "", "", fmt.Errorf("%w: business_line does not match token", errForbidden)
	}
	if p.EndUser != "" {
		if endUser != "" && endUser != p.EndUser {
			return "", "", fmt.Errorf("%w: end_user does not match token", errForbidden)
		}
		endUser = p.EndUser
	}
	if endUser == "" {
		return "", "", fmt.Errorf("%w: end_user is required for service tokens", service.ErrInvalid)
	}
	return p.BusinessLine, endUser, nil
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
	case errors.Is(err, errForbidden):
		code = http.StatusForbidden
	case errors.Is(err, service.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, service.ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, artifact.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, artifact.ErrTooLarge):
		code = http.StatusRequestEntityTooLarge
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
	bl, eu, err := claim(principal(r), req.BusinessLine, req.EndUser)
	if err != nil {
		s.fail(w, err)
		return
	}
	id, err := s.Service.Create(r.Context(), service.CreateRequest{
		BusinessLine: bl, EndUser: eu, Agent: req.Agent, AgentVersion: req.AgentVersion,
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
	st, ok := s.session(w, r)
	if !ok {
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
	if _, ok := s.session(w, r); !ok {
		return
	}
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
	if _, ok := s.session(w, r); !ok {
		return
	}
	if err := s.Service.Interrupt(r.Context(), r.PathValue("id"), r.PathValue("run")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
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
	// 审批记录的决定者取自令牌，而不是请求中自报的值。
	if p := principal(r); !p.Unrestricted || req.By == "" {
		req.By = actor(p)
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
	bl, eu, err := claim(principal(r), q.Get("business_line"), q.Get("end_user"))
	if err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Nodes.List(r.Context(), node.Scope{BusinessLine: bl, EndUser: eu})
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

type artifactView struct {
	ID        string    `json:"id"`
	URI       string    `json:"uri"`
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

func viewArtifact(m *artifact.Meta) artifactView {
	return artifactView{ID: m.ID, URI: artifact.URI(m.ID), SessionID: m.SessionID, Name: m.Name,
		MimeType: m.MimeType, Size: m.Size, SHA256: m.SHA256, CreatedAt: m.CreatedAt}
}

func (s *Server) artifacts(w http.ResponseWriter) bool {
	if s.Artifacts == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "artifacts are not configured"})
		return false
	}
	return true
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.artifacts(w) {
		return
	}
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	sid := st.SessionID
	mimeType := r.Header.Get("Content-Type")
	if mimeType == "application/octet-stream" {
		mimeType = "" // 交给扩展名与内容推断
	}
	m, err := s.Artifacts.Put(r.Context(), sid, r.URL.Query().Get("name"), mimeType, r.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewArtifact(m))
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.artifacts(w) {
		return
	}
	if _, ok := s.session(w, r); !ok {
		return
	}
	list, err := s.Artifacts.List(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []artifactView{}
	for _, m := range list {
		out = append(out, viewArtifact(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

func (s *Server) artifactMeta(w http.ResponseWriter, r *http.Request) {
	m, ok := s.artifact(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, viewArtifact(m))
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.artifact(w, r); !ok {
		return
	}
	m, rc, err := s.Artifacts.Open(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", m.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Name}))
	w.Header().Set("X-Yanshi-Session", m.SessionID)
	w.Header().Set("X-Yanshi-Artifact-Name", url.QueryEscape(m.Name))
	_, _ = io.Copy(w, rc)
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
		b, err := pj.Marshal(public(e))
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
	if _, ok := s.session(w, r); !ok {
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
	// 令牌到期时断开，客户端带新令牌与 Last-Event-ID 重连（docs/design/auth.md §4）。
	var expired <-chan time.Time
	if exp := principal(r).Expires; !exp.IsZero() {
		t := time.NewTimer(time.Until(exp))
		defer t.Stop()
		expired = t.C
	}

	for {
		events, err := eventlog.ReadAll(ctx, s.Service.Store.Log, id, after)
		if err != nil {
			return
		}
		for _, e := range events {
			b, err := pj.Marshal(public(e))
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
		case <-expired:
			fmt.Fprint(w, "event: token_expired\ndata: {}\n\n")
			return
		case <-ping.C:
			// 删除后日志不再增长：借心跳复查删除记录，及时结束流（ADR-0015）。
			if s.Service.Deletions != nil {
				if gone, _ := lifecycle.Deleted(ctx, s.Service.Deletions, id); gone {
					fmt.Fprint(w, "event: session_deleted\ndata: {}\n\n")
					return
				}
			}
			fmt.Fprint(w, ": ping\n\n")
		case <-heads:
		case d := <-deltas:
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "event: delta\ndata: %s\n\n", b)
		}
	}
}

// public 返回可以发给客户端的事件：清除进程内部地址（ADR-0013）。事件不可修改，因此按需复制。
func public(e *v1.Event) *v1.Event {
	a := e.GetAttemptStarted()
	if a.GetLiveEndpoint() == "" {
		return e
	}
	cp := proto.Clone(e).(*v1.Event)
	cp.GetAttemptStarted().LiveEndpoint = ""
	return cp
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

// actor 描述调用方，记入关闭、审批等事件。
func actor(p auth.Principal) string {
	switch {
	case p.Unrestricted:
		return "api"
	case p.Service():
		return "service:" + p.BusinessLine
	}
	return "end_user:" + p.EndUser
}

type sessionSummary struct {
	SessionID   string     `json:"session_id"`
	EndUser     string     `json:"end_user"`
	Agent       string     `json:"agent"`
	CreatedAt   time.Time  `json:"created_at"`
	LastInputAt time.Time  `json:"last_input_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	if s.Service.Index == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "session index is not configured"})
		return
	}
	q := r.URL.Query()
	p := principal(r)
	bl, eu := p.BusinessLine, p.EndUser
	switch {
	case p.Unrestricted:
		bl, eu = q.Get("business_line"), q.Get("end_user")
	case p.Service():
		eu = q.Get("end_user") // 为空表示整个业务线
	case q.Get("end_user") != "" && q.Get("end_user") != eu:
		s.fail(w, fmt.Errorf("%w: end_user does not match token", errForbidden))
		return
	}
	limit, err := parseUint(q.Get("limit"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if limit == 0 || limit > 200 {
		limit = 50
	}
	page, next, err := s.Service.Index.List(r.Context(), bl, eu, q.Get("cursor"), int(limit))
	if err != nil {
		s.fail(w, fmt.Errorf("%w: %v", service.ErrInvalid, err))
		return
	}
	out := []sessionSummary{}
	for _, x := range page {
		v := sessionSummary{SessionID: x.ID, EndUser: x.EndUser, Agent: x.Agent, CreatedAt: x.CreatedAt, LastInputAt: x.LastInputAt}
		if !x.ClosedAt.IsZero() {
			v.ClosedAt = &x.ClosedAt
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "next_cursor": next})
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.Service.Close(r.Context(), st.SessionID, actor(principal(r)), req.Reason); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	reason := "end_user"
	if principal(r).Service() {
		reason = "service"
	}
	if err := s.Service.Delete(r.Context(), st.SessionID, reason, ""); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// nodeRemover 让注销账号同时删除 EndUser 的 Node 登记。
type nodeRemover struct{ dir node.Directory }

func (n nodeRemover) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	_, err := n.dir.DeleteScope(ctx, node.Scope{BusinessLine: businessLine, EndUser: endUser})
	return err
}

// businessLineOf 返回只接受服务令牌的接口所作用的业务线：开发模式下取自查询参数。
func businessLineOf(w http.ResponseWriter, r *http.Request, s *Server) (string, bool) {
	p := principal(r)
	switch {
	case p.Unrestricted:
		if bl := r.URL.Query().Get("business_line"); bl != "" {
			return bl, true
		}
		s.fail(w, fmt.Errorf("%w: business_line is required", service.ErrInvalid))
		return "", false
	case p.Service():
		return p.BusinessLine, true
	}
	s.fail(w, fmt.Errorf("%w: a service token is required", errForbidden))
	return "", false
}

func (s *Server) deleteEndUser(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	var nodes service.NodeRemover
	if s.Nodes != nil {
		nodes = nodeRemover{s.Nodes}
	}
	id, err := s.Service.DeleteEndUser(r.Context(), bl, r.PathValue("id"), nodes)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": id})
}

func (s *Server) deletion(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	if s.Service.Deletions == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "deletion is not configured"})
		return
	}
	req, err := s.Service.Deletions.Request(r.Context(), r.PathValue("id"))
	if err == nil && req.BusinessLine != bl {
		err = lifecycle.ErrNotFound
	}
	if errors.Is(err, lifecycle.ErrNotFound) {
		err = fmt.Errorf("%w: deletion %s", service.ErrNotFound, r.PathValue("id"))
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": req.ID, "created_at": req.CreatedAt, "sessions": req.Sessions, "completed": req.Completed,
		"done": req.Completed == req.Sessions,
	})
}
