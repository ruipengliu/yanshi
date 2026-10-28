package capture_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func TestRenderBudget(t *testing.T) {
	g := &v1.CaptureSegment{Ref: "18446744073709551614.199", Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: strings.Repeat("x", 32), Text: strings.Repeat("😀", session.MaxCaptureText), SpokenAt: timestamppb.New(time.Date(2026, 12, 28, 23, 59, 0, 0, time.UTC)), TimeZone: "UTC"}
	p := capture.RenderPage{Kind: "page", Span: capture.Ref{Seq: ^uint64(0) - 2, Index: 199}, Cont: true, Segments: []*v1.CaptureSegment{g}}
	out, _, err := capture.Render(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	budget := model.EstimateTokens(out)
	if _, _, err := capture.Render(p, budget); err != nil {
		t.Fatal(err)
	}
	if blocks, _, err := capture.Render(p, budget-1); err == nil || len(blocks) != 0 {
		t.Fatal("oversized page accepted")
	}
}
