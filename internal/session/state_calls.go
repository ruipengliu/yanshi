package session

import (
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability/capname"
)

// Call 是 Run 中一次 Capability 调用的进度。
type Call struct {
	Call *v1.ToolCall
	// StartedAttempts 记录哪些 Attempt 追加过 ToolCallStarted；长度 > 1 表示被重新执行过。
	StartedAttempts []uint32
	// StartedReadOnly 与 StartedAttempts 一一对应，执行检查读本 Attempt，重答证据对全部 Attempt 取与。
	StartedReadOnly []bool
	// NodeID 非空表示路由调用，Deadline 是其结果截止时间。
	NodeID   string
	Deadline time.Time
	// DispatchedAt 是路由调用的派发时间（第一个带 node_id 的 ToolCallStarted 的事件时间）：设备离线的宽限期从它与设备
	// 下线时间中较晚者起算（ADR-0035）。
	DispatchedAt time.Time
	Done         bool
	// Background 表示调用已转入后台：Run 中的结果是占位说明，调用在 Session 级继续等待设备（State.Background）。
	Background bool

	Approval *Approval
}

// Approval 是一次调用的审批进度；nil 表示未请求审批。
type Approval struct {
	Summary  string
	Deadline time.Time
	Decided  bool
	Approved bool
}

// AwaitingApproval 表示已请求审批、尚无决定且调用未完成（未因超时等原因结束）。
func (c *Call) AwaitingApproval() bool { return c.Approval != nil && !c.Approval.Decided && !c.Done }

// Dispatched 表示路由调用已派发、尚无结果。
func (c *Call) Dispatched() bool { return c.NodeID != "" && c.Started() && !c.Done }

func (c *Call) Started() bool { return len(c.StartedAttempts) > 0 }

// PendingCall 返回第一个尚无结果的调用，没有则返回 nil。
func (r *Run) PendingCall() *Call {
	for _, c := range r.Calls {
		if !c.Done {
			return c
		}
	}
	return nil
}

// IsDelegation 报告调用是否是委托（ADR-0031）：工具名为 agent__<名称>，或已派发到子 Session。
func (c *Call) IsDelegation() bool {
	return capname.IsDelegation(c.Call.GetCapability()) || capname.IsDelegationNode(c.NodeID)
}

// DelegationFront 是从第一个未完成的调用起、按模型给出的顺序相邻的委托调用（ADR-0055）：Worker 一起派发它们，
// 等它们都有结果之后再推进后面的调用。第一个未完成的调用不是委托时为空。
func (r *Run) DelegationFront() []*Call {
	for i, c := range r.Calls {
		if c.Done {
			continue
		}
		var front []*Call
		for _, d := range r.Calls[i:] {
			if !d.IsDelegation() {
				break
			}
			front = append(front, d)
		}
		return front
	}
	return nil
}

func (r *Run) call(id string) *Call {
	for _, c := range r.Calls {
		if c.Call.GetCallId() == id {
			return c
		}
	}
	return nil
}

// callIDBatch 是 callIDs 的新层并入旧层的批量：克隆至多复制这么多个 ID。
const callIDBatch = 256

// hasCallID 报告 id 是否在 Session 中出现过。
func (s *State) hasCallID(id string) bool { return s.callIDs[id] || s.callIDsOld[id] }

func (s *State) addCallID(id string) {
	if s.callIDs == nil {
		s.callIDs = map[string]bool{}
	}
	s.callIDs[id] = true
	if len(s.callIDs) < callIDBatch {
		return
	}
	merged := make(map[string]bool, len(s.callIDsOld)+len(s.callIDs))
	for k := range s.callIDsOld {
		merged[k] = true
	}
	for k := range s.callIDs {
		merged[k] = true
	}
	s.callIDsOld, s.callIDs = merged, nil
}

// ShowCardCapability 是 show_card 的工具名（card.Capability；session 不依赖 card 包）。
const ShowCardCapability = capname.ShowCardCapability

func hasCard(blocks []*v1.ContentBlock) bool {
	for _, b := range blocks {
		if b.GetCard() != nil {
			return true
		}
	}
	return false
}
