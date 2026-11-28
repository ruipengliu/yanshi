package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"yanshi/internal/askuser"
	"yanshi/internal/delegation"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

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

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BusinessLine   string          `json:"business_line"`
		EndUser        string          `json:"end_user"`
		Agent          string          `json:"agent"`
		AgentVersion   string          `json:"agent_version"`
		DeleteAt       json.RawMessage `json:"delete_at"`
		TimeZone       string          `json:"time_zone"`
		IdempotencyKey json.RawMessage `json:"idempotency_key"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var deleteAt time.Time
	if len(req.DeleteAt) > 0 {
		if p := principal(r); !p.Service() && !p.Unrestricted {
			s.fail(w, fmt.Errorf("%w: delete_at requires a service token", errForbidden))
			return
		}
		if err := json.Unmarshal(req.DeleteAt, &deleteAt); err != nil || deleteAt.IsZero() {
			s.fail(w, fmt.Errorf("%w: delete_at must be an RFC3339 timestamp", service.ErrInvalid))
			return
		}
	}
	bl, eu, err := claim(principal(r), req.BusinessLine, req.EndUser)
	if err != nil {
		s.fail(w, err)
		return
	}
	var key string
	if len(req.IdempotencyKey) > 0 {
		if err := json.Unmarshal(req.IdempotencyKey, &key); err != nil || key == "" {
			s.fail(w, fmt.Errorf("%w: invalid idempotency_key", service.ErrInvalid))
			return
		}
	}
	id, replay, err := s.Service.CreateWithReplay(r.Context(), service.CreateRequest{
		BusinessLine: bl, EndUser: eu, Agent: req.Agent, AgentVersion: req.AgentVersion, DeleteAt: deleteAt, TimeZone: req.TimeZone,
		IdempotencyKey: key,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]string{"session_id": id})
}

type runView struct {
	RunID   string       `json:"run_id"`
	Status  string       `json:"status"`
	Attempt uint32       `json:"attempt"`
	Turns   int          `json:"turns"`
	Waiting *waitingView `json:"waiting,omitempty"`
	// WaitingAll 在同时等待多个已派发的调用时列出全部（并行委托，ADR-0055）；Waiting 仍是第一个未完成的调用。
	WaitingAll []*waitingView `json:"waiting_all,omitempty"`
	// FromBackground 非空表示 Run 由该后台调用的了结（结果或到期）开启，而不是用户的输入（ADR-0035）。
	FromBackground string `json:"from_background,omitempty"`
}

// backgroundView 是一个未了结的后台调用（ADR-0035）：设备离线超过宽限期、Run 不再等待它；设备上线后执行，
// 结果作为一条输入送达，到 Deadline 仍未执行则撤回。
type backgroundView struct {
	CallID     string    `json:"call_id"`
	Capability string    `json:"capability"`
	NodeID     string    `json:"node_id"`
	NodeOnline *bool     `json:"node_online,omitempty"`
	Deadline   time.Time `json:"deadline"`
	RunID      string    `json:"run_id"`
}

// waitingView 说明挂起中的 Run 在等什么。
type waitingView struct {
	// Kind 为 "approval"、"node"、"question"（等待用户回答 ask_user，ADR-0025）、"agent"（等待委托的子 Session，
	// SessionID 是它）或 "quota"（配额用尽，Deadline 为重置时间）。
	Kind       string    `json:"kind"`
	SessionID  string    `json:"session_id,omitempty"`
	CallID     string    `json:"call_id,omitempty"`
	Capability string    `json:"capability,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeOnline *bool     `json:"node_online,omitempty"`
	Deadline   time.Time `json:"deadline"`
}

func (s *Server) waiting(r *http.Request, endUser string, run *session.Run) *waitingView {
	c := run.PendingCall()
	if run.QuotaSuspended() {
		return &waitingView{Kind: "quota", Reason: run.SuspendReason, Deadline: run.SuspendedUntil}
	}
	// 移交（handoff）只是换一个 Worker 继续，不是在等什么，不显示。
	if run.Status.Terminal() || c == nil {
		return nil
	}
	return s.waitingCall(r, endUser, c)
}

// waitingAll 列出同时在等的全部已派发调用；不多于一个时为空，由 waiting 说明。
func (s *Server) waitingAll(r *http.Request, endUser string, run *session.Run) []*waitingView {
	if run.Status.Terminal() || run.QuotaSuspended() {
		return nil
	}
	var out []*waitingView
	for _, c := range run.Calls {
		if c.Dispatched() {
			if v := s.waitingCall(r, endUser, c); v != nil {
				out = append(out, v)
			}
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

func (s *Server) waitingCall(r *http.Request, endUser string, c *session.Call) *waitingView {
	switch {
	case c.AwaitingApproval():
		return &waitingView{Kind: "approval", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			Summary: c.Approval.Summary, Deadline: c.Approval.Deadline}
	case c.Dispatched() && delegation.IsNode(c.NodeID):
		return &waitingView{Kind: "agent", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			SessionID: delegation.ChildSession(c.NodeID), Deadline: c.Deadline}
	case c.Dispatched() && c.NodeID == askuser.NodeID:
		return &waitingView{Kind: "question", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(), Deadline: c.Deadline}
	case c.Dispatched():
		v := &waitingView{Kind: "node", CallID: c.Call.GetCallId(), Capability: c.Call.GetCapability(),
			NodeID: c.NodeID, Deadline: c.Deadline}
		if s.Nodes != nil {
			if info, err := s.Nodes.Get(r.Context(), endUser, c.NodeID); err == nil {
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
	for _, p := range st.Past {
		runs = append(runs, runView{RunID: p.ID, Status: p.Status.String(), Attempt: p.Attempt, Turns: p.Turns, FromBackground: p.FromBackground})
	}
	for _, run := range st.Runs {
		runs = append(runs, runView{RunID: run.ID, Status: run.Status.String(), Attempt: run.Attempt, Turns: run.Turns,
			Waiting: s.waiting(r, st.Created.GetEndUser(), run), WaitingAll: s.waitingAll(r, st.Created.GetEndUser(), run),
			FromBackground: run.FromBackground})
	}
	background := []backgroundView{}
	for _, b := range st.OpenBackground() {
		v := backgroundView{CallID: b.CallID, Capability: b.Capability, NodeID: b.NodeID, Deadline: b.Deadline, RunID: b.RunID}
		if s.Nodes != nil {
			if info, err := s.Nodes.Get(r.Context(), st.Created.GetEndUser(), b.NodeID); err == nil {
				v.NodeOnline = &info.Online
			}
		}
		background = append(background, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    st.SessionID,
		"seq":           st.Seq,
		"business_line": st.Created.GetBusinessLine(),
		"end_user":      st.Created.GetEndUser(),
		"agent":         map[string]string{"name": st.Agent.GetName(), "version": st.Agent.GetVersion()},
		"runs":          runs,
		"background":    background,
		"time_zone":     st.Created.GetTimeZone(),
		"parent":        parentView(st),
	})
}

// parentView 是委托派生的子 Session 的来处（docs/design/m5-multi-agent.md）：客户端据此折叠展示。不是子 Session 时为 nil。
func parentView(st *session.State) map[string]string {
	p := st.Created.GetParent()
	if p == nil {
		return nil
	}
	return map[string]string{"session_id": p.GetSessionId(), "run_id": p.GetRunId(), "call_id": p.GetCallId()}
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
		writeJSON(w, http.StatusNotImplemented, errorResponse{Error: "session index is not configured", Code: codeNotImplemented})
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
	list := s.Service.Index.List
	switch q.Get("order") {
	case "", "recent":
	case "session_id":
		list = s.Service.Index.ListStable
	default:
		s.fail(w, fmt.Errorf("%w: unknown session order", service.ErrInvalid))
		return
	}
	page, next, err := list(r.Context(), bl, eu, q.Get("cursor"), int(limit))
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
