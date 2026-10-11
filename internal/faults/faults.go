// Package faults 是服务端故障注入（docs/design/console.md §7，ADR-0033）：只在 serve -faults 开启时装配，
// 供调试控制台在真实进程上观察执行语义——模型调用失败与重试、流中途中断、Worker 崩溃后租约过期被接管、
// 内容安全服务不可用时不放行、设备目录查询失败。故障类型与 internal/sim 中注入的同名故障对应；sim 在确定性的
// 模拟中注入它们，这里在真实进程中按规则注入。
//
// 规则在进程内存中，只影响本进程；注入的每一次都记录在 Recent 中（只有 ID 与类型，ADR-0015）。
package faults

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"yanshi/internal/metrics"
)

// Kind 是故障类型。
type Kind string

const (
	// ModelError：模型调用返回可重试的服务端错误（503）。经 model.Guard 的提供商先在提供商层重试。
	ModelError Kind = "model_error"
	// ModelRateLimit：模型调用被限流（429），参数 retry_after（秒，默认 2）。
	ModelRateLimit Kind = "model_rate_limit"
	// ModelStreamCut：流式输出到一半中断（参数 after_chars，默认 20）。已输出过增量，提供商层不重试，由 Worker 重新调用。
	ModelStreamCut Kind = "model_stream_cut"
	// ModelDelay：模型调用前等待（参数 delay_ms，默认 3000），便于观察插话、中断与晚加入者。
	ModelDelay Kind = "model_delay"
	// WorkerCrash：Worker 写完这一步后"崩溃"——不释放租约、不移交，Run 等租约过期后被接管（新的 Attempt）。
	WorkerCrash Kind = "worker_crash"
	// ModerationDown：内容安全服务不可用。输入被拒绝（不写日志），模型输出的这一步重试（ADR-0021）。
	ModerationDown Kind = "moderation_down"
	// CatalogError：解析工具时查询设备目录失败（暂时性），这一步重试。
	CatalogError Kind = "catalog_error"
)

// Kinds 是全部故障类型。
var Kinds = []Kind{ModelError, ModelRateLimit, ModelStreamCut, ModelDelay, WorkerCrash, ModerationDown, CatalogError}

// Rule 是一条注入规则：对匹配的调用注入 Count 次（0 表示一直注入，直到删除）。
type Rule struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// SessionID 非空时只对该 Session 注入；为空时对所有 Session。
	SessionID string            `json:"session_id,omitempty"`
	Count     int               `json:"count"`
	Params    map[string]string `json:"params,omitempty"`
	Fired     int               `json:"fired"`
	Created   time.Time         `json:"created"`
}

// Firing 是一次注入。
type Firing struct {
	RuleID    string    `json:"rule_id"`
	Kind      Kind      `json:"kind"`
	SessionID string    `json:"session_id,omitempty"`
	Time      time.Time `json:"time"`
}

// ErrInvalid 表示规则不合法。
var ErrInvalid = errors.New("faults: invalid rule")

// maxRecent 是保留的最近注入次数。
const maxRecent = 200

// Injector 保存规则并决定每次调用是否注入。nil 的 Injector 从不注入。
type Injector struct {
	Now func() time.Time

	mu     sync.Mutex
	next   int
	rules  []*Rule
	recent []Firing
}

func (in *Injector) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// Add 增加一条规则并返回它（带上分配的 ID）。
func (in *Injector) Add(r Rule) (Rule, error) {
	if !slices.Contains(Kinds, r.Kind) {
		return Rule{}, fmt.Errorf("%w: unknown kind %q", ErrInvalid, r.Kind)
	}
	if r.Count < 0 {
		return Rule{}, fmt.Errorf("%w: count must be ≥ 0", ErrInvalid)
	}
	for k, v := range r.Params {
		if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 600_000 {
			return Rule{}, fmt.Errorf("%w: param %s must be an integer in [0, 600000]", ErrInvalid, k)
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.next++
	r.ID, r.Fired, r.Created = "flt_"+strconv.Itoa(in.next), 0, in.now()
	in.rules = append(in.rules, &r)
	return r, nil
}

// Delete 删除规则；id 为空时删除全部。
func (in *Injector) Delete(id string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rules = slices.DeleteFunc(in.rules, func(r *Rule) bool { return id == "" || r.ID == id })
}

// Snapshot 返回当前规则与最近的注入（新的在前）。
func (in *Injector) Snapshot() ([]Rule, []Firing) {
	in.mu.Lock()
	defer in.mu.Unlock()
	rules := make([]Rule, len(in.rules))
	for i, r := range in.rules {
		rules[i] = *r
	}
	recent := slices.Clone(in.recent)
	slices.Reverse(recent)
	return rules, recent
}

// Take 报告这次调用是否注入 kind，注入时返回规则的参数。计数用尽的规则随之删除。
func (in *Injector) Take(ctx context.Context, kind Kind) (map[string]string, bool) {
	if in == nil {
		return nil, false
	}
	sid := SessionFrom(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, r := range in.rules {
		if r.Kind != kind || (r.SessionID != "" && r.SessionID != sid) {
			continue
		}
		r.Fired++
		in.recent = append(in.recent, Firing{RuleID: r.ID, Kind: kind, SessionID: sid, Time: in.now()})
		if len(in.recent) > maxRecent {
			in.recent = in.recent[len(in.recent)-maxRecent:]
		}
		metrics.FaultsInjected.WithLabelValues(string(kind)).Inc()
		params := r.Params
		if r.Count > 0 && r.Fired >= r.Count {
			in.rules = slices.Delete(in.rules, i, i+1)
		}
		return params, true
	}
	return nil, false
}

// Param 读取整数参数，缺省或不合法时取 def。
func Param(params map[string]string, name string, def int) int {
	if n, err := strconv.Atoi(params[name]); err == nil {
		return n
	}
	return def
}

type sessionKey struct{}

// WithSession 在 ctx 中记下正在处理的 Session（Worker 的每一步、提交的检查），按 Session 注入的规则据此匹配。
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey{}, sessionID)
}

// SessionFrom 返回 WithSession 记下的 Session。
func SessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(sessionKey{}).(string)
	return s
}
