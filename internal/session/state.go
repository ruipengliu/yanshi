// Package session 把 Session 的 Event 日志投影为状态。
//
// 投影是纯函数，并在投影过程中校验 docs/design/m0-core-primitives.md 中定义的不变量：
// 任何违反状态机的事件都会使 Apply 返回错误。写入方在追加前用它校验，
// 模拟测试用它检查日志始终合法。
package session

import (
	"fmt"
	"maps"
	"slices"

	v1 "yanshi/gen/yanshi/v1"
)

// State 是 Session 在某个 seq 处的投影。
type State struct {
	SessionID string
	// Seq 是已应用的最后一个事件的 seq；0 表示空日志。
	Seq     uint64
	Created *v1.SessionCreated
	// Agent 是当前使用的 AgentDef 版本：创建时取自 SessionCreated，撤回版本时由 AgentSwitched 改变
	// （ADR-0020）。读取版本一律用它，而不是 Created.Agent。
	Agent *v1.AgentRef
	// Runs 是最近的 Run（完整），至多 KeepRuns+1 个：新 Run 请求时只留最近 KeepRuns 个旧 Run。更早的终态 Run 只留摘要（Past）：投影、克隆与快照的代价
	// 因此不随 Session 的历史增长（延展性评审 §8.3）。需要更早调用明细的少数路径读日志（HistoryCall）。
	Runs []*Run
	// Past 是更早的终态 Run 的摘要，按请求顺序。条目不再修改，在副本间共享。
	Past []RunSummary
	// 替代关系不随 Run 明细淘汰；只在接受重答时以新 map 替换，副本之间只读共享。
	SupersededRuns      map[string]bool
	SupersededCardCalls map[string]bool
	// History 是构成对话上下文的事件（用户输入、模型输出、调用结果），按日志顺序；
	// 压缩时裁剪。重答只过滤模型视图，仍保留旧事件供安全判断，连续重答可能延后下一次压缩。
	History []*v1.Event
	// Closed 非 nil 表示 Session 已关闭（终态）。
	Closed *v1.SessionClosed
	// Compaction 是最近一次上下文压缩，nil 表示从未压缩（ADR-0012）；CompactedAt 是该事件自身的 seq。
	Compaction  *v1.ContextCompacted
	CompactedAt uint64
	// 调用 ID 在 Session 内必须唯一：callIDs 是出现过的全部调用 ID，分两层使克隆不随历史增长——callIDsOld 不再
	// 修改、在副本间共享；callIDs 是最近加入的，克隆时复制，满 callIDBatch 个时连同旧的并入一个新的 callIDsOld。
	callIDsOld map[string]bool
	callIDs    map[string]bool
	// Inputs 是最近 MaxRecentInputs 条带 ID 的输入（旧的在前），用于去重重复提交。
	Inputs []InputRef
	// ActiveCall 是进行中的 Call（语音通话，docs/design/m3-call.md），没有时为 nil。
	ActiveCall *ActiveCall
	// Delegated 是委托派生的子 Session 中，委托的任务（输入 ID 为 DelegatedInputID）所在的 Run；不是子 Session 或任务
	// 尚未提交时为空。写回、撤回与重复派发都靠它找到委托 Run：去重窗口（Inputs）会被用户在子 Session 中的输入挤出
	// （评审 §11 问题 3）。
	Delegated string
	// Background 是未了结的后台调用（ADR-0035），键为调用 ID：设备离线超过宽限期、Worker 不再在 Run 中等待的路由调用。
	// 设备结果、到期或用户取消之后移出；Session 关闭后保留（不再有事件，关闭方据此撤回投递）。
	Background map[string]*BackgroundCall
	// 失效声明仍保留，以保证 ID 不复用及创建重试幂等。
	Schedules map[string]*Schedule
	// 只在同批 Fired 与紧随的 RunRequested 之间存在。
	pendingSchedule *v1.Event
}

// Reduce 从空状态依次应用 events。
func Reduce(events []*v1.Event) (*State, error) {
	s := &State{}
	for _, e := range events {
		if err := s.Apply(e); err != nil {
			return nil, err
		}
	}
	if s.pendingSchedule != nil {
		return nil, fmt.Errorf("unpaired ScheduleFired")
	}
	return s, nil
}

// Clone 返回可独立修改的副本。事件本身不可变，因此共享；终态的 Run 同样共享：Apply 只经 activeRun、fencedRun
// 与 Active 修改 Run，它们都拒绝终态的 Run。Check 每次提交都要克隆，共享使代价不随历史 Run 的数量增长。
func (s *State) Clone() *State {
	cp := *s
	cp.Runs = make([]*Run, len(s.Runs))
	for i, r := range s.Runs {
		if r.Status.Terminal() {
			cp.Runs[i] = r
		} else {
			cp.Runs[i] = r.clone()
		}
	}
	cp.History = slices.Clip(s.History)
	cp.Inputs = slices.Clone(s.Inputs)
	if s.ActiveCall != nil {
		c := *s.ActiveCall
		cp.ActiveCall = &c
	}
	cp.Past = slices.Clip(s.Past)
	cp.callIDs = make(map[string]bool, len(s.callIDs))
	for id := range s.callIDs {
		cp.callIDs[id] = true
	}
	// 条目不再修改（了结时整条移出），共享即可。
	cp.Background = maps.Clone(s.Background)
	cp.Schedules = maps.Clone(s.Schedules)
	return &cp
}

// Check 校验 events 能否依次应用到 s 之后，不修改 s。events 的 seq 可以留空，按顺序补齐后校验。
func (s *State) Check(events ...*v1.Event) error {
	_, err := s.after(events...)
	return err
}

// after 返回应用 events 之后的状态（副本），s 不变。
func (s *State) after(events ...*v1.Event) (*State, error) {
	if s.pendingSchedule != nil {
		return nil, fmt.Errorf("cannot append after an unpaired ScheduleFired")
	}
	cp := s.Clone()
	for i, e := range events {
		if e.GetSeq() == 0 {
			e = withSeq(e, s.Seq+uint64(i)+1)
		}
		if err := cp.Apply(e); err != nil {
			return nil, err
		}
	}
	if cp.pendingSchedule != nil {
		return nil, fmt.Errorf("ScheduleFired requires RunRequested in the same append")
	}
	return cp, nil
}

func withSeq(e *v1.Event, seq uint64) *v1.Event {
	cp := &v1.Event{Id: e.GetId(), SessionId: e.GetSessionId(), Seq: seq, Time: e.GetTime()}
	cp.Payload = e.Payload
	return cp
}

// Apply 把一个事件应用到状态上；事件违反状态机时返回错误且状态不变。
func (s *State) Apply(e *v1.Event) error {
	if err := s.apply(e); err != nil {
		return fmt.Errorf("session %s seq %d: %w", e.GetSessionId(), e.GetSeq(), err)
	}
	return nil
}
