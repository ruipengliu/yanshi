// Package tracing 记录 Run 的执行耗时，供部署按 Run ID 查询。
package tracing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"slices"
	"time"
)

const (
	FieldTraceID      = "trace_id"
	FieldSpanID       = "span_id"
	FieldParentSpanID = "parent_span_id"
	FieldLinkTraceID  = "link_trace_id"
	FieldName         = "name"
	FieldStart        = "start"
	FieldDuration     = "duration_ms"
	FieldOutcome      = "outcome"
	FieldSession      = "session"
	FieldRun          = "run"
	FieldAttempt      = "attempt"
	FieldCall         = "call"
	FieldWorker       = "worker"
	FieldRetries      = "retries"
	FieldFirstToken   = "first_token_ms"
	FieldQueueWait    = "queue_wait_ms"
	FieldKind         = "kind"
	FieldTurn         = "turn"
	FieldDeltas       = "deltas"
	FieldFirstDelta   = "first_delta_ms"
)

type Name uint8

const (
	RunSubmit Name = iota
	QueueClaim
	RunAttempt
	ModelCall
	ModelAdmission
	RunEnd
	ToolCall
	NodeDeliver
	ToolResult
	SandboxExec
	SandboxPrepare
	SandboxRun
	LoopStep
	LoopModel
	LiveStream
)

var names = [...]string{
	"run.submit", "queue.claim", "run.attempt", "model.call", "model.admission", "run.end",
	"tool.call", "node.deliver", "tool.result", "sandbox.exec", "sandbox.prepare", "sandbox.run",
	"loop.step", "loop.model", "live.stream",
}

type Outcome uint8

const (
	OK Outcome = iota
	Done
	Failed
	Handoff
	Suspended
	Lost
	Interrupted
	Error
	Canceled
	Timeout
	Dispatched
	Backgrounded
)

var outcomes = [...]string{
	"ok", "done", "failed", "handoff", "suspended", "lost", "interrupted", "error", "canceled", "timeout",
	"dispatched", "backgrounded",
}

type ToolKind uint8

const (
	Local ToolKind = iota
	MCP
	Node
	Sandbox
	Delegation
	AskUser
)

var kinds = [...]string{"local", "mcp", "node", "sandbox", "delegation", "ask_user"}

type CountName uint8

const (
	Retries CountName = iota
	Turn
	Deltas
)

var counts = [...]string{FieldRetries, FieldTurn, FieldDeltas}

// Attribute 只经本包的构造函数生成，调用方只能在指定字段传入对应的 ID 或数值。
type Attribute struct{ attr slog.Attr }

func Session(id string) Attribute { return Attribute{slog.String(FieldSession, id)} }
func Run(id string) Attribute     { return Attribute{slog.String(FieldRun, id)} }
func Call(id string) Attribute    { return Attribute{slog.String(FieldCall, id)} }
func Worker(id string) Attribute  { return Attribute{slog.String(FieldWorker, id)} }
func Attempt(n uint32) Attribute  { return Attribute{slog.Uint64(FieldAttempt, uint64(n))} }

func Count(name CountName, n int) Attribute {
	if int(name) < len(counts) {
		return Attribute{slog.Int(counts[name], n)}
	}
	return Attribute{}
}

func Kind(kind ToolKind) Attribute {
	if int(kind) < len(kinds) {
		return Attribute{slog.String(FieldKind, kinds[kind])}
	}
	return Attribute{}
}

func QueueWait(d time.Duration) Attribute {
	return Attribute{slog.Float64(FieldQueueWait, milliseconds(d))}
}

func LinkRun(id string) Attribute {
	if id == "" {
		return Attribute{}
	}
	return Attribute{slog.String(FieldLinkTraceID, TraceID(id))}
}

func milliseconds(d time.Duration) float64 { return float64(max(0, d)) / float64(time.Millisecond) }

// TraceID 只依赖 Run ID，使各进程可用已有的 ID 关联追踪记录。
func TraceID(runID string) string {
	if runID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("yanshi/trace/v1\x00" + runID))
	return nonzeroHex(sum[:16])
}

func nonzeroHex(id []byte) string {
	for _, b := range id {
		if b != 0 {
			return hex.EncodeToString(id)
		}
	}
	// W3C Trace Context 保留全零 ID。
	id[len(id)-1] = 1
	return hex.EncodeToString(id)
}

// Tracer 与其零值均可并发使用。nil 与零值关闭追踪。
type Tracer struct{ logger *slog.Logger }

func New(logger *slog.Logger) *Tracer { return &Tracer{logger: logger} }

func (t *Tracer) Enabled() bool { return t != nil && t.logger != nil }

// Span 由开始它的同步路径持有。
type Span struct {
	tracer  *Tracer
	name    Name
	start   time.Time
	traceID string
	id      [8]byte
	parent  string
	attrs   []slog.Attr
}

func (t *Tracer) Start(name Name, attrs ...Attribute) *Span {
	if !t.Enabled() {
		return nil
	}
	return t.start(name, time.Now(), attrs)
}

// StartAt 用于认领在前、取得 Run ID 在后的路径。
func (t *Tracer) StartAt(name Name, start time.Time, attrs ...Attribute) *Span {
	if !t.Enabled() {
		return nil
	}
	return t.start(name, start, attrs)
}

// Record 使用已提交 Event 的时间记录跨 Attempt 的 Run 总时长。
func (t *Tracer) Record(name Name, start, end time.Time, outcome Outcome, attrs ...Attribute) {
	if !t.Enabled() {
		return
	}
	t.start(name, start, attrs).end(end, outcome)
}

func (t *Tracer) start(name Name, start time.Time, attrs []Attribute) *Span {
	if int(name) >= len(names) {
		return nil
	}
	s := &Span{tracer: t, name: name, start: start}
	// 与 Event 的 ID 生成器分开，追踪开关不改变持久 ID 序列。
	rand.Read(s.id[:])
	s.Set(attrs...)
	return s
}

func (s *Span) Child(name Name) *Span {
	if s == nil {
		return nil
	}
	child := s.tracer.Start(name)
	if child == nil {
		return nil
	}
	child.traceID, child.parent = s.traceID, nonzeroHex(s.id[:])
	for _, attr := range s.attrs {
		switch attr.Key {
		case FieldSession, FieldRun, FieldAttempt, FieldCall, FieldWorker, FieldKind:
			child.attrs = append(child.attrs, attr)
		}
	}
	return child
}

// Set 补录开始时未知的值，例如提交后才确定的 Run ID。
func (s *Span) Set(attrs ...Attribute) {
	if s == nil {
		return
	}
	for _, a := range attrs {
		switch a.attr.Key {
		case "":
			continue
		case FieldRun:
			s.traceID = TraceID(a.attr.Value.String())
		}
		s.set(a.attr)
	}
}

func (s *Span) set(a slog.Attr) {
	i := slices.IndexFunc(s.attrs, func(old slog.Attr) bool { return old.Key == a.Key })
	if i >= 0 {
		s.attrs[i] = a
		return
	}
	s.attrs = append(s.attrs, a)
}

func (s *Span) FirstToken() {
	s.first(FieldFirstToken)
}

func (s *Span) FirstDelta() {
	s.first(FieldFirstDelta)
}

func (s *Span) first(key string) {
	if s == nil || slices.ContainsFunc(s.attrs, func(a slog.Attr) bool { return a.Key == key }) {
		return
	}
	s.attrs = append(s.attrs, slog.Float64(key, milliseconds(time.Since(s.start))))
}

// ExtendStart 补录已持久化的派发时间。缺失或时钟偏差时保留本地观察到的开始时间。
func (s *Span) ExtendStart(start time.Time) {
	if s != nil && !start.IsZero() && start.Before(s.start) {
		s.start = start
	}
}

func (s *Span) Is(name Name) bool { return s != nil && s.name == name }

func (s *Span) End(outcome Outcome) {
	if s != nil && s.tracer != nil {
		s.end(time.Now(), outcome)
	}
}

func (s *Span) end(end time.Time, outcome Outcome) {
	if s == nil || s.tracer == nil || int(outcome) >= len(outcomes) {
		return
	}
	attrs := []slog.Attr{
		slog.String(FieldSpanID, nonzeroHex(s.id[:])),
		slog.String(FieldParentSpanID, s.parent),
		slog.String(FieldName, names[s.name]),
		slog.String(FieldStart, s.start.UTC().Format(time.RFC3339Nano)),
		slog.Float64(FieldDuration, milliseconds(end.Sub(s.start))),
		slog.String(FieldOutcome, outcomes[outcome]),
	}
	if s.traceID != "" {
		attrs = append(attrs, slog.String(FieldTraceID, s.traceID))
	}
	s.tracer.logger.LogAttrs(context.Background(), slog.LevelInfo, "span", append(attrs, s.attrs...)...)
	s.tracer = nil
}

type spanKey struct{}

func (s *Span) Context(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, spanKey{}, s)
}

func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}
