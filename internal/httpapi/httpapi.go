// Package httpapi 以 HTTP/JSON + SSE 暴露 Session 操作。
//
//	POST /v1/sessions                              创建 Session
//	GET  /v1/sessions?end_user=&cursor=&limit=     列出 Session（按最近输入倒序）
//	GET  /v1/sessions/{id}                         Session 投影
//	POST /v1/sessions/{id}/close                   关闭 Session（不可逆）
//	DELETE /v1/sessions/{id}                       删除 Session（异步，202）
//	DELETE /v1/end_users/{id}                      删除 EndUser 的全部数据（仅服务令牌，异步，202）
//	GET  /v1/deletions/{id}                        删除请求的进度（仅服务令牌）
//	GET  /v1/memories?category=&q=&end_user=       本业务线的 Memory（docs/design/m4-memory-grant.md §6）
//	DELETE /v1/memories/{id}                       删除一条 Memory
//	DELETE /v1/sessions/{id}/memories              删除该 Session 写入的全部 Memory（仅服务令牌；按来源撤销投毒）
//	GET  /v1/memories/access?since=&end_user=      本业务线 Memory 的读取记录
//	GET  /v1/grants                                EndUser 涉及本业务线的授权（仅用户令牌）
//	POST /v1/grants                                创建授权 {"from","to","categories","expires_at"}（仅用户令牌）
//	DELETE /v1/grants/{id}                         撤销授权（仅用户令牌）
//	POST /v1/sessions/{id}/inputs                  提交输入（新 Run 或 Steer；内容安全拒绝 422、不可用 503）
//	POST /v1/sessions/{id}/runs/{run}/interrupt    中断 Run
//	GET  /v1/sessions/{id}/events?after=N&limit=M  已提交事件（JSON）
//	GET  /v1/sessions/{id}/stream?after=N          已提交事件 + 实时增量（SSE，支持 Last-Event-ID 续传）
//	POST /v1/sessions/{id}/approvals/{call}        审批决定 {"approve": bool}
//	POST /v1/sessions/{id}/answers/{call}          回答 ask_user 提问 {"selected": [...], "values": {...}, "text": ""}（ADR-0025）
//	GET  /v1/nodes?business_line=&end_user=        EndUser 的 Node 列表
//	POST /v1/sessions/{id}/artifacts?name=         上传工件（请求体为文件内容，Content-Type 为 MIME 类型）
//	GET  /v1/sessions/{id}/artifacts               Session 的工件列表
//	GET  /v1/artifacts/{id}                        下载工件
//	GET  /v1/artifacts/{id}/meta                   工件元数据
//	GET  /v1/quota?end_user=                       当前周期的配额与已用（docs/design/m4-quota-usage.md §5）
//	GET  /v1/usage?from=&to=&group_by=&end_user=   用量汇总（day | end_user | model | agent；金额单位：微元）
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
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/auth"
	"yanshi/internal/feed"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/memory"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
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
	// Memory 为 nil 时不提供 Memory 与 Grant 接口。
	Memory *memory.Service
	// Usage 为 nil 时不提供用量接口；配额取自 Service.Quotas（docs/design/m4-quota-usage.md §5）。
	Usage  usage.Store
	Logger *slog.Logger
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
		"GET /v1/memories":                            s.listMemories,
		"DELETE /v1/memories/{id}":                    s.forgetMemory,
		"DELETE /v1/sessions/{id}/memories":           s.forgetSessionMemories,
		"GET /v1/memories/access":                     s.memoryAccess,
		"GET /v1/grants":                              s.listGrants,
		"POST /v1/grants":                             s.createGrant,
		"DELETE /v1/grants/{id}":                      s.revokeGrant,
		"POST /v1/sessions/{id}/inputs":               s.submit,
		"POST /v1/sessions/{id}/runs/{run}/interrupt": s.interrupt,
		"GET /v1/sessions/{id}/events":                s.events,
		"GET /v1/sessions/{id}/stream":                s.stream,
		"POST /v1/sessions/{id}/approvals/{call}":     s.decide,
		"POST /v1/sessions/{id}/answers/{call}":       s.answer,
		"GET /v1/nodes":                               s.listNodes,
		"GET /v1/quota":                               s.quota,
		"GET /v1/usage":                               s.usage,
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
	case errors.Is(err, memory.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, memory.ErrRejected):
		code = http.StatusBadRequest
	case errors.Is(err, memory.ErrLimit):
		code = http.StatusConflict
	case errors.Is(err, artifact.ErrTooLarge):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, moderation.ErrRejected):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, moderation.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, usage.ErrExceeded):
		code = http.StatusTooManyRequests
		var ex *usage.ExceededError
		if errors.As(err, &ex) {
			secs := int(math.Ceil(time.Until(ex.Period.ResetAt).Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
			writeJSON(w, code, map[string]any{"error": err.Error(), "quota": ex.Period})
			return
		}
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
	// Kind 为 "approval"、"node"、"question"（等待用户回答 ask_user，ADR-0025）或 "quota"（配额用尽，Deadline 为重置时间）。
	Kind       string    `json:"kind"`
	CallID     string    `json:"call_id,omitempty"`
	Capability string    `json:"capability,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeOnline *bool     `json:"node_online,omitempty"`
	Deadline   time.Time `json:"deadline"`
}

func (s *Server) waiting(r *http.Request, run *session.Run) *waitingView {
	c := run.PendingCall()
	if run.Status.Terminal() || (c == nil && run.SuspendReason == "") {
		return nil
	}
	switch {
	case run.Status == session.RunSuspended && run.SuspendReason != "":
		return &waitingView{Kind: "quota", Reason: run.SuspendReason, Deadline: run.SuspendedUntil}
	case c.AwaitingApproval():
		return &waitingView{Kind: "approval", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			Summary: c.Approval.Summary, Deadline: c.Approval.Deadline}
	case c.Dispatched() && c.NodeID == askuser.NodeID:
		return &waitingView{Kind: "question", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(), Deadline: c.Deadline}
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
		"agent":         map[string]string{"name": st.Agent.GetName(), "version": st.Agent.GetVersion()},
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
	out := map[string]any{"run_id": res.RunID, "steered": res.Steered}
	if res.Answered != "" {
		out["answered"] = res.Answered
	}
	writeJSON(w, http.StatusAccepted, out)
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

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	var req askuser.Answer
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Service.Answer(r.Context(), r.PathValue("id"), r.PathValue("call"), &req); err != nil {
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
	src := feed.Source{Log: s.Service.Store.Log, Live: s.Live, Deletions: s.Service.Deletions}
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
	var removers []service.UserDataRemover
	if s.Nodes != nil {
		removers = append(removers, nodeRemover{s.Nodes})
	}
	if s.Memory != nil {
		removers = append(removers, s.Memory)
	}
	if s.Usage != nil {
		removers = append(removers, s.Usage) // 匿名化，不删除（ADR-0019）
	}
	id, err := s.Service.DeleteEndUser(r.Context(), bl, r.PathValue("id"), removers...)
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

func (s *Server) memoryEnabled(w http.ResponseWriter) bool {
	if s.Memory == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "memory is not configured"})
		return false
	}
	return true
}

// memoryOwner 返回 Memory 接口作用的 (业务线, EndUser)：用户令牌为本人；服务令牌须给出 end_user。
func memoryOwner(w http.ResponseWriter, r *http.Request, s *Server) (string, string, bool) {
	q := r.URL.Query()
	bl, eu, err := claim(principal(r), q.Get("business_line"), q.Get("end_user"))
	if err != nil {
		s.fail(w, err)
		return "", "", false
	}
	return bl, eu, true
}

// grantOwner 返回 Grant 接口的调用方：授权只能由 EndUser 本人管理，服务令牌一律拒绝。
func grantOwner(w http.ResponseWriter, r *http.Request, s *Server) (string, string, bool) {
	if principal(r).Service() {
		s.fail(w, fmt.Errorf("%w: grants are managed by the end user", errForbidden))
		return "", "", false
	}
	return memoryOwner(w, r, s)
}

type memoryView struct {
	ID            string    `json:"id"`
	Category      string    `json:"category"`
	Content       string    `json:"content"`
	SourceSession string    `json:"source_session"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, err := parseUint(q.Get("limit"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if limit == 0 || limit > 500 {
		limit = 100
	}
	hits, err := s.Memory.List(r.Context(), bl, eu, memory.Category(q.Get("category")), q.Get("q"), int(limit))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []memoryView{}
	for _, h := range hits {
		out = append(out, memoryView{ID: h.ID, Category: string(h.Category), Content: h.Content, SourceSession: h.SourceSession,
			CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": out})
}

func (s *Server) forgetMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	if err := s.Memory.Forget(r.Context(), bl, eu, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgetSessionMemories 按来源 Session 撤销 Memory（ADR-0023）：运维发现投毒后，删除由该 Session 写入的全部记忆。
// 只接受服务令牌：这是业务线的运维操作，不是 EndUser 的日常操作（EndUser 用 DELETE /v1/memories/{id}）。
func (s *Server) forgetSessionMemories(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	if _, ok := s.session(w, r); !ok {
		return
	}
	if p := principal(r); !p.Service() && !p.Unrestricted {
		s.fail(w, fmt.Errorf("%w: a service token is required", errForbidden))
		return
	}
	if err := s.Memory.ForgetSession(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) memoryAccess(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, fmt.Errorf("%w: since: %v", service.ErrInvalid, err))
			return
		}
		since = t
	}
	list, err := s.Memory.Accesses(r.Context(), eu, bl, since, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	type view struct {
		MemoryID string    `json:"memory_id"`
		Reader   string    `json:"reader_business_line"`
		At       time.Time `json:"at"`
	}
	out := []view{}
	for _, a := range list {
		out = append(out, view{MemoryID: a.MemoryID, Reader: a.Reader, At: a.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accesses": out})
}

type grantView struct {
	ID         string     `json:"id"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Categories []string   `json:"categories"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func viewGrant(g *memory.Grant) grantView {
	v := grantView{ID: g.ID, From: g.From, To: g.To, Categories: []string{}, CreatedAt: g.CreatedAt}
	for _, c := range g.Categories {
		v.Categories = append(v.Categories, string(c))
	}
	if !g.ExpiresAt.IsZero() {
		v.ExpiresAt = &g.ExpiresAt
	}
	if !g.RevokedAt.IsZero() {
		v.RevokedAt = &g.RevokedAt
	}
	return v
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	list, err := s.Memory.ListGrants(r.Context(), eu, bl)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []grantView{}
	for _, g := range list {
		out = append(out, viewGrant(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

func (s *Server) createGrant(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	var req struct {
		From       string     `json:"from"`
		To         string     `json:"to"`
		Categories []string   `json:"categories"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	// 授权界面可以由任一方的 App 发起，但调用方所在的业务线必须是授权的一方。
	if !principal(r).Unrestricted && req.From != bl && req.To != bl {
		s.fail(w, fmt.Errorf("%w: the caller's business line must be a party to the grant", errForbidden))
		return
	}
	cats := make([]memory.Category, len(req.Categories))
	for i, c := range req.Categories {
		cats[i] = memory.Category(c)
	}
	var expires time.Time
	if req.ExpiresAt != nil {
		expires = *req.ExpiresAt
	}
	g, err := s.Memory.CreateGrant(r.Context(), eu, req.From, req.To, cats, expires)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewGrant(g))
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	if err := s.Memory.RevokeGrant(r.Context(), eu, bl, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
