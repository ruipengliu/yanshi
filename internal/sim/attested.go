package sim

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
)

const simAttestedName = "mcp_sim__pages"

type simAttestedTool struct{ w *World }

func (t simAttestedTool) Spec() capability.Spec {
	return capability.Spec{Name: simAttestedName, Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t simAttestedTool) Invoke(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
	r, e := t.InvokeResult(ctx, inv)
	return r.Content, e
}
func (t simAttestedTool) InvokeResult(_ context.Context, inv capability.Invocation) (capability.Result, error) {
	if t.w.faults && t.w.chance(0.1) {
		return capability.Result{}, fmt.Errorf("simulated page failure")
	}
	p := capture.StructuredPage{Kind: "page", Span: "57.0", Done: true}
	for i, g := range t.w.captureSegments() {
		who := "wearer"
		if g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER {
			who = "other"
		}
		p.Segments = append(p.Segments, capture.PageSegment{Ref: fmt.Sprintf("57.%d", i), Speaker: who, SpeakerKey: g.GetSpeakerKey(), Text: g.GetText(), SpokenAt: g.GetSpokenAt().AsTime().Format("2006-01-02T15:04:05Z07:00"), TimeZone: g.GetTimeZone()})
	}
	raw, err := json.Marshal(p)
	return capability.Result{Page: raw}, err
}
