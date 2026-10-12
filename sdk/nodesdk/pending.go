package nodesdk

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

// 等待用户的提问与审批，含进行中的委托里的（docs/design/m5-multi-agent.md §4、docs/design/m3-client-sdks.md §2）。
// 与网页 SDK 的 PendingTracker（sdk/web/src/view.ts）规则相同。

// AskUser 是 ask_user 的能力名（ADR-0025）。
const AskUser = "ask_user"

// agentNodePrefix 是委托调用的 ToolCallStarted.node_id 前缀：其后是子 Session 的 ID。
const agentNodePrefix = "@agent/"

// userNode 是 ask_user 调用派发时 ToolCallStarted 的 node_id：路由到用户本人（ADR-0025）。
const userNode = "@user"

// 子 Session 的订阅以 not_found 或 error 结束、而委托仍在进行时，退避后重订：Worker 先提交 ToolCallStarted、再创建子
// Session，发现委托立即订阅时子 Session 可能还不存在；error 是网关一侧的暂时故障。deleted 等其他原因不重订。
var (
	retryMin = 200 * time.Millisecond
	retryMax = 5 * time.Second
)

func retryable(reason string) bool { return reason == "not_found" || reason == "error" }

// Question 是 Agent 经 ask_user 提出的问题。
type Question struct {
	CallID   string           `json:"-"`
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple"`
	Fields   []QuestionField  `json:"fields"`
}

type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type QuestionField struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Type 为 "text"（默认）、"number"、"date"（YYYY-MM-DD）或 "time"（HH:MM）。
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// QuestionOf 从 AssistantMessage 的工具调用中读出 ask_user 提问；不是提问或参数无法解析时返回 nil。
// 参数不合法的提问不会到达用户（Worker 直接把错误交还模型），所以这里不再校验。
func QuestionOf(c *v1.ToolCall) *Question {
	if c.GetCapability() != AskUser {
		return nil
	}
	q := &Question{}
	if err := json.Unmarshal([]byte(c.GetArgumentsJson()), q); err != nil {
		return nil
	}
	q.CallID = c.GetCallId()
	return q
}

type PendingKind string

const (
	PendingQuestion PendingKind = "question"
	PendingApproval PendingKind = "approval"
)

// PendingItem 是一项等待用户的提问或审批。
type PendingItem struct {
	Kind PendingKind
	// SessionID 是回答或审批发往的 Session：顶层 Session，或进行中的委托派生的子 Session。
	SessionID string
	CallID    string
	RunID     string
	// Agent 是提出它的子 Agent（AgentDef 名，去掉 agent__ 前缀）；顶层 Session 自己的为空。
	Agent string
	// Via 是经由顶层 Session 的哪个直接委托的子 Session（孙 Session 里的也归到它）；顶层 Session 自己的为空。
	Via string
	// Question 在 Kind 为 PendingQuestion 时是提问内容。
	Question *Question
	// Summary 与 Capability 在 Kind 为 PendingApproval 时是给用户看的说明与要执行的能力。
	Summary    string
	Capability string
}

type trackedCall struct{ capability, runID string }

type trackedQuestion struct {
	q     *Question
	runID string
}

type trackedApproval struct{ callID, runID, summary, capability string }

type trackedDelegation struct {
	callID, runID string
	child         *tracked
}

// tracked 是被跟踪的一个 Session：顶层 Session，或进行中的委托派生的子 Session。
type tracked struct {
	sessionID, agent, via string
	calls                 map[string]trackedCall
	// asked 是 AssistantMessage 中尚未派发的提问：Worker 按顺序一次推进一个调用，模型同一轮返回"委托 + ask_user"时，
	// 提问要等委托结束才派发（ToolCallStarted{node_id: "@user"}）。在那之前它不能回答，也不算待回答。
	asked       []trackedQuestion
	questions   []trackedQuestion
	approvals   []trackedApproval
	delegations []trackedDelegation
	// off 取消对子 Session 的订阅；为 nil 时尚未订阅或在等待重订（由跟踪器的 goroutine 订阅）。
	off func()
	// gen 在每次订阅结束时加一：之前那次订阅迟到的回调据此忽略。subscribing：跟踪器的 goroutine 正在订阅它。
	gen         int
	subscribing bool
	// after 是子 Session 已收到的最后一个 seq：重订时从这里续传，已有的待处理项不必重建。
	after uint64
	// backoff 是重订的退避；retrying：在等退避的定时器。
	backoff  time.Duration
	retrying bool
	// dropped：委托已结束，不再跟踪；ended：网关以不可重订的原因结束了订阅（子 Session 被删除等），不再订阅。
	dropped, ended bool
}

func newTracked(sid, agent, via string) *tracked {
	return &tracked{sessionID: sid, agent: agent, via: via, calls: map[string]trackedCall{}}
}

// PendingTracker 跟踪一个顶层 Session 及其进行中的委托（递归）里等待用户的提问与审批。
//
// 委托出去的子 Run 的 ask_user 与审批路由到用户本人，却写在子 Session 的日志里：只看顶层 Session 的客户端看不到，
// 父 Run 就一直等着委托直到超时。所以：
//   - 顶层 Session 的事件由 HostApp 交给 OnEvent（在它自己的 SessionHandler 中，从 seq 0 开始订阅）；
//   - 被跟踪的 Session 出现委托（ToolCallStarted 的 node_id 为 "@agent/<子 Session>"）时，跟踪器经 Client 订阅子 Session
//     的完整历史；该委托调用出现 ToolResult、或它所在的 Run 结束时，取消订阅并收起子 Session（及其后代）的待处理项；
//   - 子 Session 的订阅以 not_found 或 error 结束而委托仍在进行时（子 Session 可能还没创建），退避后从已收到的位置重订
//     （200ms 起翻倍，至多 5 秒）；以其他原因（deleted 等）结束时收起它的待处理项，不再订阅；
//   - 提问随 AssistantMessage 出现、派发给用户（ToolCallStarted 的 node_id 为 "@user"）后才算待回答，在该调用出现
//     ToolResult 或 Run 结束时收起；审批随 ApprovalRequested 出现，在 ApprovalDecided、该调用的 ToolResult 或 Run 结束时收起。
//
// 跟踪器对子 Session 的订阅与 HostApp 自己的订阅（Subscribe）并存。OnEvent 只更新状态、不阻塞；订阅与 onChange 在跟踪器
// 自己的 goroutine 中进行，onChange 串行调用，连续的变化可能合并为一次。方法可并发调用。
type PendingTracker struct {
	c        *Client
	onChange func(items []PendingItem)

	mu   sync.Mutex
	root *tracked
	// 树中的全部 Session：防止委托链成环时重复订阅。
	sessions map[string]bool
	// version 在待处理项每次变化时加一；notified 是上次 onChange 时的 version。
	version, notified uint64
	// cancels 是待取消的子订阅（委托已结束），由跟踪器的 goroutine 调用。
	cancels []func()
	closed  bool
	wake    chan struct{}
}

// NewPendingTracker 开始跟踪 sessionID；onChange 可为 nil。不再需要时调用 Close。
func NewPendingTracker(c *Client, sessionID string, onChange func(items []PendingItem)) *PendingTracker {
	t := &PendingTracker{
		c: c, onChange: onChange, root: newTracked(sessionID, "", ""),
		sessions: map[string]bool{sessionID: true}, wake: make(chan struct{}, 1),
	}
	go t.loop()
	return t
}

// OnEvent 应用顶层 Session 的一条已提交事件；其他 Session 的事件忽略。不阻塞，可在 SessionHandler.OnEvent 中调用。
func (t *PendingTracker) OnEvent(e *v1.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || e.GetSessionId() != t.root.sessionID {
		return
	}
	t.update(t.root, e)
}

// Items 返回全部待处理项，按委托树深度优先排列：每个 Session 先审批、后提问，然后是它的子 Session。
func (t *PendingTracker) Items() []PendingItem {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.items()
}

// TypedAnswerTarget 返回输入框里的文字应回答的提问（以 Client.Answer 的 text 发往它的 SessionID）：顶层 Session 自己没有
// 待回答的提问、整棵树里恰好一个待回答的提问、且它在子 Session 中时返回它，否则返回 nil，文字照旧提交给顶层 Session
// （顶层自己的提问由服务端把文字当作回答）。尚未派发的提问不算待回答。
//
// 不由服务端沿委托链把父 Session 的输入转给子 Session：一次输入跨两个 Session 不能在一个事务中提交（ADR-0028），父日志里
// 也没有可以去重的输入。回答按 call_id 去重：别的设备已经回答时得到 conflict，此时应保留文字、提示用户，不要自动改投顶层
// Session。
func (t *PendingTracker) TypedAnswerTarget() *PendingItem {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.root.questions) > 0 {
		return nil
	}
	var found *PendingItem
	for _, it := range t.items() {
		if it.Kind != PendingQuestion {
			continue
		}
		if found != nil {
			return nil
		}
		found = &it
	}
	return found
}

// Close 停止跟踪并取消全部子 Session 的订阅与重订（随后在跟踪器的 goroutine 中完成）；之后不再调用 onChange
// （正在进行的那一次除外）。
func (t *PendingTracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	t.clear(t.root)
	t.signal()
}

func (t *PendingTracker) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// loop 订阅新出现的子 Session、取消已结束的委托的订阅，并在待处理项变化后调用 onChange。订阅与回调都不在接收 goroutine
// 中：订阅要写网络，回调由 HostApp 实现，二者都可能阻塞。
func (t *PendingTracker) loop() {
	for range t.wake {
		t.mu.Lock()
		closed := t.closed
		cancels := t.cancels
		t.cancels = nil
		type pick struct {
			n     *tracked
			gen   int
			after uint64
		}
		var subscribe []pick
		if !closed {
			t.walk(t.root, func(n *tracked) {
				if n != t.root && n.off == nil && !n.ended && !n.retrying && !n.subscribing {
					n.subscribing = true
					subscribe = append(subscribe, pick{n, n.gen, n.after})
				}
			})
		}
		var items []PendingItem
		notify := !closed && t.version != t.notified
		if notify {
			items, t.notified = t.items(), t.version
		}
		t.mu.Unlock()

		for _, off := range cancels {
			off()
		}
		for _, p := range subscribe {
			// 第一次从头订阅（子 Session 的提问与审批可能早于这次订阅），重订时从已收到的位置续传。
			n := p.n
			off := t.c.observe(n.sessionID, p.after, childHandler{t, n, p.gen})
			t.mu.Lock()
			n.subscribing = false
			gone := n.dropped || t.closed
			// 订阅在 observe 返回之前就已结束（gen 变了）：OnEnded 已经处理，这个 off 不再有用。
			if !gone && n.gen == p.gen {
				n.off = off
			}
			t.mu.Unlock()
			if gone {
				off()
			}
		}
		if notify && t.onChange != nil {
			t.onChange(items)
		}
		if closed {
			return
		}
	}
}

// childHandler 接收一个子 Session 的一次订阅（在连接的接收 goroutine 中）：只更新状态，订阅与回调交给跟踪器的 goroutine。
type childHandler struct {
	t   *PendingTracker
	n   *tracked
	gen int
}

func (h childHandler) OnEvent(e *v1.Event) {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	if !h.t.closed && !h.n.dropped && h.n.gen == h.gen {
		h.n.after, h.n.backoff = e.GetSeq(), 0
		h.t.update(h.n, e)
	}
}

func (childHandler) OnDelta(*v1.LiveDelta) {}

// OnEnded：not_found 或 error 时委托仍在进行（委托结束时 dropped 已置位），退避后重订，已有的待处理项保留。其他原因
// （子 Session 被删除等）时它的待处理项已无法处理，收起；委托本身仍等父 Session 的结果。
func (h childHandler) OnEnded(reason string) {
	t, n := h.t, h.n
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || n.dropped || n.gen != h.gen {
		return
	}
	n.gen++
	n.off = nil
	if retryable(reason) {
		n.backoff = min(max(n.backoff*2, retryMin), retryMax)
		n.retrying = true
		time.AfterFunc(n.backoff, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			n.retrying = false
			if !t.closed && !n.dropped {
				t.signal() // 由跟踪器的 goroutine 重订
			}
		})
		return
	}
	n.ended = true
	if t.clear(n) {
		t.changed()
	}
}

// changed 记下一次变化并唤醒跟踪器的 goroutine（持有 mu）。
func (t *PendingTracker) changed() {
	t.version++
	t.signal()
}

// update 把一条事件应用到被跟踪的 Session（持有 mu）。
func (t *PendingTracker) update(n *tracked, e *v1.Event) {
	if t.apply(n, e) {
		t.changed()
	}
}

// apply 返回待处理项是否变化（持有 mu）。
func (t *PendingTracker) apply(n *tracked, e *v1.Event) bool {
	switch p := e.GetPayload().(type) {
	case *v1.Event_AssistantMessage:
		m := p.AssistantMessage
		changed := false
		for _, c := range m.GetToolCalls() {
			n.calls[c.GetCallId()] = trackedCall{capability: c.GetCapability(), runID: m.GetRunId()}
			if q := QuestionOf(c); q != nil {
				n.asked = slices.DeleteFunc(n.asked, func(x trackedQuestion) bool { return x.q.CallID == q.CallID })
				n.asked = append(n.asked, trackedQuestion{q: q, runID: m.GetRunId()})
			}
		}
		return changed
	case *v1.Event_ApprovalRequested:
		a := p.ApprovalRequested
		n.approvals = slices.DeleteFunc(n.approvals, func(x trackedApproval) bool { return x.callID == a.GetCallId() })
		n.approvals = append(n.approvals, trackedApproval{callID: a.GetCallId(), runID: a.GetRunId(), summary: a.GetSummary(),
			capability: n.calls[a.GetCallId()].capability})
		return true
	case *v1.Event_ApprovalDecided:
		return n.removeApproval(func(x trackedApproval) bool { return x.callID == p.ApprovalDecided.GetCallId() })
	case *v1.Event_ToolCallStarted:
		s := p.ToolCallStarted
		if s.GetNodeId() == userNode {
			// 提问派发给用户：此后才待回答。
			i := slices.IndexFunc(n.asked, func(x trackedQuestion) bool { return x.q.CallID == s.GetCallId() })
			if i < 0 {
				return false
			}
			q := n.asked[i]
			n.asked = slices.Delete(n.asked, i, i+1)
			n.questions = append(n.questions, q)
			return true
		}
		sid, ok := strings.CutPrefix(s.GetNodeId(), agentNodePrefix)
		if !ok || t.sessions[sid] || slices.ContainsFunc(n.delegations, func(d trackedDelegation) bool { return d.callID == s.GetCallId() }) {
			return false
		}
		via := n.via
		if via == "" {
			via = sid
		}
		agent := strings.TrimPrefix(n.calls[s.GetCallId()].capability, "agent__")
		n.delegations = append(n.delegations, trackedDelegation{callID: s.GetCallId(), runID: s.GetRunId(), child: newTracked(sid, agent, via)})
		t.sessions[sid] = true
		t.signal() // 由跟踪器的 goroutine 订阅
		return false
	case *v1.Event_ToolResult:
		id := p.ToolResult.GetCallId()
		n.asked = slices.DeleteFunc(n.asked, func(x trackedQuestion) bool { return x.q.CallID == id })
		changed := n.removeQuestion(func(x trackedQuestion) bool { return x.q.CallID == id })
		changed = n.removeApproval(func(x trackedApproval) bool { return x.callID == id }) || changed
		return t.endDelegations(n, func(d trackedDelegation) bool { return d.callID == id }) || changed
	case *v1.Event_RunCompleted, *v1.Event_RunFailed, *v1.Event_RunInterrupted:
		runID := runIDOf(e)
		n.asked = slices.DeleteFunc(n.asked, func(x trackedQuestion) bool { return x.runID == runID })
		changed := n.removeQuestion(func(x trackedQuestion) bool { return x.runID == runID })
		changed = n.removeApproval(func(x trackedApproval) bool { return x.runID == runID }) || changed
		changed = t.endDelegations(n, func(d trackedDelegation) bool { return d.runID == runID }) || changed
		maps.DeleteFunc(n.calls, func(_ string, c trackedCall) bool { return c.runID == runID })
		return changed
	}
	return false
}

func runIDOf(e *v1.Event) string {
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunCompleted:
		return p.RunCompleted.GetRunId()
	case *v1.Event_RunFailed:
		return p.RunFailed.GetRunId()
	case *v1.Event_RunInterrupted:
		return p.RunInterrupted.GetRunId()
	}
	return ""
}

func (n *tracked) removeQuestion(match func(trackedQuestion) bool) bool {
	before := len(n.questions)
	n.questions = slices.DeleteFunc(n.questions, match)
	return len(n.questions) != before
}

func (n *tracked) removeApproval(match func(trackedApproval) bool) bool {
	before := len(n.approvals)
	n.approvals = slices.DeleteFunc(n.approvals, match)
	return len(n.approvals) != before
}

// endDelegations 结束 n 中满足条件的委托，放下其子 Session；返回是否收起了待处理项（持有 mu）。
func (t *PendingTracker) endDelegations(n *tracked, match func(trackedDelegation) bool) bool {
	changed := false
	n.delegations = slices.DeleteFunc(n.delegations, func(d trackedDelegation) bool {
		if !match(d) {
			return false
		}
		changed = t.drop(d.child) || changed
		return true
	})
	return changed
}

// drop 放下一个子 Session（及其后代）：订阅交给跟踪器的 goroutine 取消；返回是否收起了待处理项（持有 mu）。
func (t *PendingTracker) drop(n *tracked) bool {
	n.dropped = true
	if n.off != nil {
		t.cancels = append(t.cancels, n.off)
		n.off = nil
		t.signal()
	}
	delete(t.sessions, n.sessionID)
	return t.clear(n)
}

// clear 收起一个 Session 的待处理项，并放下它的全部子 Session；返回是否收起了待处理项（持有 mu）。
func (t *PendingTracker) clear(n *tracked) bool {
	changed := len(n.questions) > 0 || len(n.approvals) > 0
	n.asked, n.questions, n.approvals = nil, nil, nil
	for _, d := range n.delegations {
		changed = t.drop(d.child) || changed
	}
	n.delegations = nil
	return changed
}

func (t *PendingTracker) walk(n *tracked, f func(*tracked)) {
	f(n)
	for _, d := range n.delegations {
		t.walk(d.child, f)
	}
}

// items 持有 mu。
func (t *PendingTracker) items() []PendingItem {
	var out []PendingItem
	t.walk(t.root, func(n *tracked) {
		for _, a := range n.approvals {
			out = append(out, PendingItem{Kind: PendingApproval, SessionID: n.sessionID, CallID: a.callID, RunID: a.runID,
				Agent: n.agent, Via: n.via, Summary: a.summary, Capability: a.capability})
		}
		for _, q := range n.questions {
			out = append(out, PendingItem{Kind: PendingQuestion, SessionID: n.sessionID, CallID: q.q.CallID, RunID: q.runID,
				Agent: n.agent, Via: n.via, Question: q.q})
		}
	})
	return out
}
