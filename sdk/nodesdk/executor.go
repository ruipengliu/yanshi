// Package nodesdk 是嵌入 HostApp 的设备端 SDK：把本地能力作为 Node 接入 yanshi。
//
// HostApp 只需声明 Capability 并提供处理函数；SDK 负责连接、断线重连，
// 以及以 call_id 为键的去重执行，保证重复投递不会重复执行（docs/design/m1-device-nodes.md §3）。
package nodesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

// Handler 执行一次调用；返回的 error 作为错误结果交给 Agent。
type Handler func(ctx context.Context, argumentsJSON string) ([]*v1.ContentBlock, error)

type Capability struct {
	Spec    *v1.CapabilitySpec
	Handler Handler
}

type invocationKey struct{}

// Invocation 返回当前正在执行的调用（Session、Run、call_id 等），供能力处理函数使用，
// 例如把上传的工件归属到正确的 Session。
func Invocation(ctx context.Context) *v1.Invoke {
	inv, _ := ctx.Value(invocationKey{}).(*v1.Invoke)
	return inv
}

type State int

const (
	StateNew State = iota
	// StateStarted 已开始执行、尚无结果。崩溃后重启看到此状态，说明执行可能已产生副作用。
	StateStarted
	StateDone
)

type Record struct {
	State  State
	Result *v1.InvokeResult
	// SessionID 与 UpdatedAt 用于按 Session 清除与按保留期清理（docs/design/m2-session-lifecycle.md §3.5）。
	SessionID string
	UpdatedAt time.Time
}

// Ledger 持久记录每个 call_id 的执行状态。实现必须在 Put 返回前落盘，
// 否则 App 被杀后可能重复执行有副作用的调用。
//
// 调用结果可能含有个人数据：Session 删除时由 Forget 清除，其余记录由 Prune 按保留期清理。
type Ledger interface {
	// Get 返回记录；不存在时返回 State 为 StateNew 的记录。
	Get(callID string) (*Record, error)
	Put(callID string, r *Record) error
	// Forget 删除属于该 Session 的全部记录。
	Forget(sessionID string) error
	// Prune 删除最后更新早于 before 的记录。
	Prune(before time.Time) error
}

// ForgetCapability 是保留的能力名：平台以它投递"清除某 Session 的本地记录"，
// 复用 Inbox 的离线投递与重试（docs/design/m2-session-lifecycle.md §3.5）。
const ForgetCapability = "__forget_session"

// DefaultLedgerRetention 是设备端账本记录的默认保留期。
const DefaultLedgerRetention = 7 * 24 * time.Hour

// Executor 以 call_id 去重地执行调用。并发安全。
type Executor struct {
	caps   map[string]Capability
	ledger Ledger

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

func NewExecutor(ledger Ledger, caps ...Capability) *Executor {
	e := &Executor{caps: map[string]Capability{}, ledger: ledger, inflight: map[string]context.CancelFunc{}}
	for _, c := range caps {
		e.caps[c.Spec.GetName()] = c
	}
	return e
}

func (e *Executor) Specs() []*v1.CapabilitySpec {
	out := make([]*v1.CapabilitySpec, 0, len(e.caps))
	for _, c := range e.caps {
		out = append(out, c.Spec)
	}
	return out
}

func errorResult(callID, msg string) *v1.InvokeResult {
	return &v1.InvokeResult{CallId: callID, IsError: true,
		Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: msg}}}}}
}

// Execute 执行或复用一次调用的结果。同一 call_id 正在执行时返回 nil（结果会由正在执行的那次返回）。
func (e *Executor) Execute(ctx context.Context, inv *v1.Invoke) (*v1.InvokeResult, error) {
	id := inv.GetCallId()
	e.mu.Lock()
	if _, busy := e.inflight[id]; busy {
		e.mu.Unlock()
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	e.inflight[id] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.inflight, id)
		e.mu.Unlock()
		cancel()
	}()

	if inv.GetCapability() == ForgetCapability {
		// 目标 Session 在参数中：该调用本身不属于任何 Session，不会随 Session 的清理被撤回。
		var args struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(inv.GetArgumentsJson()), &args); err != nil || args.SessionID == "" {
			return errorResult(id, "forget: session_id is required"), nil
		}
		if err := e.ledger.Forget(args.SessionID); err != nil {
			return nil, err
		}
		return &v1.InvokeResult{CallId: id, Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "forgotten"}}}}}, nil
	}
	rec, err := e.ledger.Get(id)
	if err != nil {
		return nil, err
	}
	sid := inv.GetSessionId()
	if rec.State == StateDone {
		return rec.Result, nil
	}
	c, ok := e.caps[inv.GetCapability()]
	var res *v1.InvokeResult
	switch {
	case !ok:
		res = errorResult(id, fmt.Sprintf("capability %q is not provided by this node", inv.GetCapability()))
	case rec.State == StateStarted && !c.Spec.GetIdempotent():
		res = errorResult(id, "outcome unknown: the app stopped while executing this call; it was not retried")
	default:
		if err := e.ledger.Put(id, &Record{State: StateStarted, SessionID: sid, UpdatedAt: time.Now()}); err != nil {
			return nil, err
		}
		content, err := c.Handler(context.WithValue(ctx, invocationKey{}, inv), inv.GetArgumentsJson())
		if err != nil {
			if ctx.Err() != nil {
				// 被取消或进程退出：保持 started，下次投递按"结果未知"处理。
				return nil, ctx.Err()
			}
			res = errorResult(id, err.Error())
		} else {
			res = &v1.InvokeResult{CallId: id, Content: content}
		}
	}
	if err := e.ledger.Put(id, &Record{State: StateDone, Result: res, SessionID: sid, UpdatedAt: time.Now()}); err != nil {
		return nil, err
	}
	return res, nil
}

// Cancel 取消正在执行的调用（尽力而为）。
func (e *Executor) Cancel(callID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel := e.inflight[callID]; cancel != nil {
		cancel()
	}
}

// Prune 按保留期清理账本。
func (e *Executor) Prune(before time.Time) error { return e.ledger.Prune(before) }
