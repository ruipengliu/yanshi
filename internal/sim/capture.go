package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

// 旁听转写的模拟（docs/design/captures.md §3、§11）：HostApp 批量提交，与输入、Run、关闭和删除交错；写入偶尔遇到存储故障，
// HostApp 以同一 capture_id 重试。检查：旁听不开启 Run；结果未知的重试恰好生效一次；响应丢失后的重试不写入；日志中没有
// 本应拦截的旁听（checkCaptures）。

// flakyCaptureLog 只向旁听转写的写入注入存储故障（一半在写入之前，一半在写入之后、结果未知）。其余写入不经过它的故障：
// 它们的故障由 flakyLog 与 flakyWaking 注入。
type flakyCaptureLog struct {
	eventlog.Log
	w *World
}

func (l flakyCaptureLog) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	capture := slices.ContainsFunc(events, func(e *v1.Event) bool { return e.GetCaptureAppended() != nil })
	if !capture || !l.w.faults || !l.w.chance(0.08) {
		return l.Log.Append(ctx, sid, expected, events...)
	}
	l.w.Stats.CaptureStoreFaults++
	if l.w.chance(0.5) {
		return 0, &storeFault{}
	}
	if _, err := l.Log.Append(ctx, sid, expected, events...); err != nil {
		return 0, err
	}
	return 0, &storeFault{written: true}
}

var captureWords = []string{"报价", "周五", "张三", "合同", "预算", "会议", "好的", "下周", "确认", "发邮件"}

// captureSegments 生成一批 1～4 段的旁听转写，说话人随机。
func (w *World) captureSegments() []*v1.CaptureSegment {
	segs := make([]*v1.CaptureSegment, 1+w.rng.IntN(4))
	for i := range segs {
		text := fmt.Sprintf("%s%s %d", captureWords[w.rng.IntN(len(captureWords))], captureWords[w.rng.IntN(len(captureWords))], w.rng.IntN(1000))
		if w.chance(0.5) {
			segs[i] = &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text}
		} else {
			segs[i] = &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: fmt.Sprintf("other-%d", 1+w.rng.IntN(2)), Text: text}
		}
		// 带时间才有时段与续页；同批的片段属于同一时段，时间来自模拟时钟。
		segs[i].SpokenAt = timestamppb.New(w.clock.Now())
		segs[i].TimeZone = "UTC"
	}
	return segs
}

func (w *World) captureStep() error {
	ctx := context.Background()
	i := w.rng.IntN(len(w.sessions))
	sid := w.sessions[i]
	w.nextCapture++
	id := fmt.Sprintf("cap-%d", w.nextCapture)
	segs := w.captureSegments()
	before, err := w.svc.Load(ctx, sid)
	if err != nil {
		return err
	}
	res, err := w.svc.SubmitCaptures(ctx, sid, session.StreamTranscript, id, segs)
	var sf *storeFault
	switch {
	case errors.As(err, &sf):
		return w.retryCapture(sid, id, segs, sf.written)
	case errors.Is(err, moderation.ErrUnavailable):
		w.Stats.CapturesRejected++
		w.tracef("capture %s %s: %v", sid, id, err)
		return nil
	case errors.Is(err, service.ErrConflict) && before.Closed != nil:
		// 关闭的结果未知、重试也没有成功时（closeAt），模拟仍把 Session 当作打开的：客户端由此得知它已关闭。
		w.tracef("capture %s: closed by an uncertain close", sid)
		return w.closeAt(i, w.svc.Close(ctx, sid, "sim", "test"))
	case err != nil:
		return fmt.Errorf("capture %s: %w", id, err)
	}
	after, err := w.svc.Load(ctx, sid)
	if err != nil {
		return err
	}
	if res.Duplicate || after.Captures.Count != before.Captures.Count+1 || len(after.Summaries()) != len(before.Summaries()) {
		return fmt.Errorf("invariant: capture %s in %s: %+v, captures %d→%d, runs %d→%d", id, sid, res,
			before.Captures.Count, after.Captures.Count, len(before.Summaries()), len(after.Summaries()))
	}
	w.Stats.CapturesAppended++
	w.Stats.CapturesRedacted += res.Redacted
	w.tracef("capture %s %s → %d segments, %d redacted", sid, id, len(segs), res.Redacted)
	if !w.chance(0.2) {
		return nil
	}
	// 响应丢失后重试：返回 duplicate，不写入任何事件。
	again, err := w.svc.SubmitCaptures(ctx, sid, session.StreamTranscript, id, segs)
	if err != nil {
		return fmt.Errorf("retry of capture %s: %w", id, err)
	}
	last, err := w.svc.Load(ctx, sid)
	if err != nil {
		return err
	}
	if !again.Duplicate || last.Seq != after.Seq {
		return fmt.Errorf("invariant: retry of capture %s returned %+v, log %d→%d", id, again, after.Seq, last.Seq)
	}
	w.Stats.CaptureDuplicates++
	return nil
}

// retryCapture：提交结果未知（存储故障）后以同一 capture_id 重试，已生效的必须返回 duplicate，没有生效的照常生效。
func (w *World) retryCapture(sid, id string, segs []*v1.CaptureSegment, written bool) error {
	w.Stats.UncertainRetries++
	res, err := w.svc.SubmitCaptures(context.Background(), sid, session.StreamTranscript, id, segs)
	var sf *storeFault
	if errors.As(err, &sf) || (!written && errors.Is(err, moderation.ErrUnavailable)) {
		w.tracef("retry capture %s: %v", id, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("retry of uncertain capture %s: %w", id, err)
	}
	if res.Duplicate != written {
		return fmt.Errorf("invariant: retry of capture %s (written=%v) returned duplicate=%v", id, written, res.Duplicate)
	}
	if written {
		w.Stats.CaptureDuplicates++
	} else {
		w.Stats.CapturesAppended++
	}
	w.tracef("retry capture %s → duplicate=%v", id, res.Duplicate)
	return nil
}

// checkCapture 检查日志中的一批旁听转写没有本应拦截的内容（ADR-0021）。送检的单位是整批，与一条输入相同：整批没有拦截时，
// 整批的送检文本不违规；有拦截时，保留下来的每一段单独看都不违规。与检查点用同一构造函数（service.CaptureText）。
func (w *World) checkCapture(sid string, e *v1.Event) error {
	segs := e.GetCaptureAppended().GetSegments()
	redacted := slices.ContainsFunc(segs, (*v1.CaptureSegment).GetRedacted)
	if !redacted && w.blocked(service.CaptureText(segs)) {
		return fmt.Errorf("invariant: blocked capture reached the log of %s at seq %d", sid, e.GetSeq())
	}
	for _, g := range segs {
		if redacted && !g.GetRedacted() && w.blocked(g.GetText()) {
			return fmt.Errorf("invariant: blocked capture segment reached the log of %s at seq %d", sid, e.GetSeq())
		}
	}
	return nil
}

// countCaptureResults 只计入已写入日志的结果；生成参数、随后被打断或拒绝的调用不能冒充成功读取。
func (w *World) countCaptureResults(events []*v1.Event) {
	calls := map[string]*v1.ToolCall{}
	st := &session.State{}
	for _, e := range events {
		if m := e.GetAssistantMessage(); m != nil {
			for _, c := range m.GetToolCalls() {
				calls[m.GetRunId()+"/"+c.GetCallId()] = c
			}
		}
		if r := e.GetToolResult(); r != nil {
			c := calls[r.GetRunId()+"/"+r.GetCallId()]
			if c.GetCapability() == session.CapturesCapability {
				w.Stats.CaptureResults++
				if r.GetIsError() {
					w.Stats.CaptureErrors++
				} else {
					w.Stats.CaptureSuccesses++
					var args struct {
						After string `json:"after"`
					}
					_ = json.Unmarshal([]byte(c.GetArgumentsJson()), &args)
					if args.After == "new" {
						w.Stats.CaptureNewReads++
						if st.Captures.Read[session.StreamTranscript] != nil && r.GetCaptureRead() != nil {
							w.Stats.CaptureNewResumes++
						}
					}
					if r.GetCaptureRead() != nil {
						w.Stats.CapturePages++
						text := model.Text(r.GetContent())
						if strings.Contains(text, `after="`) {
							w.Stats.CaptureMorePages++
						}
						if strings.Contains(text, "起（续）") {
							w.Stats.CaptureContinuations++
						}
					}
				}
			}
		}
		before := st.Captures.Read[session.StreamTranscript].GetAfter()
		var ackRun bool
		if done := e.GetRunCompleted(); done != nil {
			ackRun = st.Run(done.GetRunId()).CaptureReads[session.StreamTranscript].GetAck()
		}
		if err := st.Apply(e); err != nil {
			return
		}
		if ackRun {
			if st.Captures.Read[session.StreamTranscript].GetAfter() != before {
				w.Stats.AcksCommitted++
			} else {
				w.Stats.AcksMissing++
			}
		}
	}
}

// captureAfter 从模型看到的最近一页照抄位置，不读 Session 的隐藏状态。
func captureAfter(messages []model.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != model.RoleTool {
			continue
		}
		text := model.Text(m.Content)
		if !strings.HasPrefix(text, "【时段 #") {
			continue
		}
		_, rest, ok := strings.Cut(text, `after="`)
		if !ok {
			return "0"
		}
		after, _, ok := strings.Cut(rest, `"`)
		if ok {
			return after
		}
	}
	return "0"
}
