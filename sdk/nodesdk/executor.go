// Package nodesdk 是嵌入 HostApp 的设备端 SDK：把本地能力作为 Node 接入 yanshi。
//
// HostApp 只需声明 Capability 并提供处理函数；SDK 负责连接、断线重连，
// 以及以 call_id 为键的去重执行，保证重复投递不会重复执行（docs/design/m1-device-nodes.md §3）。
package nodesdk

import (
	"context"
	"fmt"
	"sync"

	v1 "yanshi/gen/yanshi/v1"
)

// Handler 执行一次调用；返回的 error 作为错误结果交给 Agent。
type Handler func(ctx context.Context, argumentsJSON string) ([]*v1.ContentBlock, error)

type Capability struct {
	Spec    *v1.CapabilitySpec
	Handler Handler
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
}

// Ledger 持久记录每个 call_id 的执行状态。实现必须在 Put 返回前落盘，
// 否则 App 被杀后可能重复执行有副作用的调用。
type Ledger interface {
	// Get 返回记录；不存在时返回 State 为 StateNew 的记录。
	Get(callID string) (*Record, error)
	Put(callID string, r *Record) error
}

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

	rec, err := e.ledger.Get(id)
	if err != nil {
		return nil, err
	}
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
		if err := e.ledger.Put(id, &Record{State: StateStarted}); err != nil {
			return nil, err
		}
		content, err := c.Handler(ctx, inv.GetArgumentsJson())
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
	if err := e.ledger.Put(id, &Record{State: StateDone, Result: res}); err != nil {
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
