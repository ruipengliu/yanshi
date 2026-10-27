package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

const simAckName = "mcp_sim__save"

// simAckSource 沿用 Catalog 的 MCP 接缝，不另起一套对象图或 HTTP 服务；真实登记与元数据传递由 Worker 测试覆盖。
type simAckSource struct{ w *World }

func (s simAckSource) Tools(_ context.Context, _ string, _ []string) ([]capability.Tool, error) {
	if s.w.opts.Attested {
		t := simAttestedTool{s.w}
		return []capability.Tool{{Spec: t.Spec(), Local: t}}, nil
	}
	if !s.w.opts.CaptureAcks {
		return nil, nil
	}
	t := simAckTool{s.w}
	return []capability.Tool{{Spec: t.Spec(), Local: t}}, nil
}

type simAckTool struct{ w *World }

func (t simAckTool) Spec() capability.Spec {
	return capability.Spec{Name: simAckName, Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t simAckTool) Invoke(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
	out, err := t.InvokeResult(ctx, inv)
	return out.Content, err
}

func (t simAckTool) InvokeResult(_ context.Context, inv capability.Invocation) (capability.Result, error) {
	if t.w.faults {
		switch t.w.rng.IntN(4) {
		case 0:
			t.w.Stats.AckMCPFailures++
			return capability.Result{}, fmt.Errorf("simulated MCP failure")
		case 1:
			t.w.Stats.AckRefusals++
			return capability.Result{Content: model.TextBlocks("没有被接受")}, nil
		}
	}
	// 业务线保存的是模型提交的范围，绝不自行把回执补到整个 Run 的末尾。
	return capability.Result{Content: model.TextBlocks("已保存"), Ack: json.RawMessage(inv.Arguments)}, nil
}

var captureLineRef = regexp.MustCompile(`(?m)^#([0-9]+\.[0-9]+) `)

// simAckArgs 只从模型见到的最近一页构造范围，续页的第一段不等于页首的时段起点。
func simAckArgs(messages []model.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != model.RoleTool {
			continue
		}
		refs := captureLineRef.FindAllStringSubmatch(model.Text(messages[i].Content), -1)
		if len(refs) == 0 {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"stream": session.StreamTranscript, "from": refs[0][1], "through": refs[len(refs)-1][1]})
		return string(raw)
	}
	return ""
}

// checkCaptureAcks 独立枚举所有合法页链的可达位置，再检查投影的提交结果；不复用投影的贪心拼链实现。
func checkCaptureAcks(events []*v1.Event) error {
	st := &session.State{}
	for _, e := range events {
		var reachable map[string]bool
		if done := e.GetRunCompleted(); done != nil {
			r := st.Run(done.GetRunId())
			if r == nil {
				return fmt.Errorf("invariant: unknown completed run %s", done.GetRunId())
			}
			if last := r.CaptureReads[session.StreamTranscript]; last.GetAck() {
				cur := st.Captures.Read[session.StreamTranscript].GetAfter()
				if cur == "" {
					cur = "0"
				}
				reachable = map[string]bool{cur: true}
				for changed := true; changed; {
					changed = false
					for _, page := range r.CapturePages {
						if !reachable[page.GetStart()] || reachable[page.GetAfter()] {
							continue
						}
						first, _ := capture.ParseRef(page.GetFirst())
						after, _, _ := strings.Cut(page.GetAfter(), "~")
						last, _ := capture.ParseRef(after)
						for _, ack := range r.CaptureAcks {
							from, _ := capture.ParseRef(ack.GetFrom())
							through, _ := capture.ParseRef(ack.GetThrough())
							if ack.GetStream() == page.GetStream() && refCovered(from, first) && refCovered(last, through) {
								reachable[page.GetAfter()], changed = true, true
								break
							}
						}
					}
				}
			}
		}
		if err := st.Apply(e); err != nil {
			return err
		}
		if reachable != nil {
			got := st.Captures.Read[session.StreamTranscript].GetAfter()
			if got == "" {
				got = "0"
			}
			if !reachable[got] {
				return fmt.Errorf("invariant: unacknowledged capture page committed at event %d", e.GetSeq())
			}
		}
	}
	return nil
}

func refCovered(a, b capture.Ref) bool {
	return a.Seq < b.Seq || (a.Seq == b.Seq && a.Index <= b.Index)
}
