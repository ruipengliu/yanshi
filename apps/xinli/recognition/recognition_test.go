package recognition_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"persona/domain"
	"xinli/capture"
	"xinli/recognition"
)

func TestDeterministicFake(t *testing.T) {
	var waited time.Duration
	script := map[string]recognition.Result{
		"chunk":   {Segments: []capture.Segment{{Speaker: "wearer", Text: "转写", At: "2026-10-28T12:00:00Z"}}, Delay: 3 * time.Second, SpeakerOverrides: map[int]string{0: "other-1"}, SpeakerRefs: map[int]string{0: "P-1"}},
		"failure": {Fail: true},
	}
	fake := recognition.NewFake(script, func(ctx context.Context, d time.Duration) error { waited = d; return ctx.Err() })
	script["chunk"].Segments[0].Text = "外部改动"
	first, err := fake.Recognize(context.Background(), recognition.ChunkRef{ID: "chunk"})
	if err != nil || waited != 3*time.Second || first[0].Speaker != "other-1" || first[0].SpeakerRef != "P-1" || first[0].Text != "转写" {
		t.Fatal(first, err)
	}
	second, err := fake.Recognize(context.Background(), recognition.ChunkRef{ID: "chunk"})
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("not deterministic", err)
	}
	first[0].Text = "修改输出"
	if second[0].Text != "转写" {
		t.Fatal("alias")
	}
	for _, id := range []string{"failure", "unknown"} {
		if _, err := fake.Recognize(context.Background(), recognition.ChunkRef{ID: id}); !errors.Is(err, recognition.ErrRecognition) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fake.Recognize(ctx, recognition.ChunkRef{ID: "chunk"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDelayCancellation(t *testing.T) {
	fake := recognition.NewFake(map[string]recognition.Result{"slow": {Delay: time.Hour}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fake.Recognize(ctx, recognition.ChunkRef{ID: "slow"}); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delay ignored cancellation")
	}
}

func TestMetadataCopies(t *testing.T) {
	segments := []capture.Segment{
		{Speaker: "wearer", SpeakerGrade: "timeline", Disposition: "accept", Text: "甲🙂", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}},
		{Speaker: "unknown", Text: "背景原文", Disposition: "quarantine", DispositionReason: "playback", Background: true},
	}
	fake := recognition.NewFake(map[string]recognition.Result{"chunk": {Segments: segments}}, nil)
	segments[0].LowSupportSpans[0].CriticalKinds[0] = "date"
	segments[0].LowSupportSpans[0].End = 2
	first, err := fake.Recognize(context.Background(), recognition.ChunkRef{ID: "chunk"})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].LowSupportSpans[0].CriticalKinds[0] != "person" || first[0].LowSupportSpans[0].End != 1 {
		t.Fatal("script aliased")
	}
	first[0].LowSupportSpans[0].CriticalKinds[0] = "number"
	first[0].LowSupportSpans[0].End = 2
	second, err := fake.Recognize(context.Background(), recognition.ChunkRef{ID: "chunk"})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].LowSupportSpans[0].CriticalKinds[0] != "person" || second[0].LowSupportSpans[0].End != 1 {
		t.Fatal("result aliased")
	}
	if second[0].SpeakerGrade != "timeline" || second[0].Disposition != "accept" || second[1].Speaker != "unknown" ||
		second[1].Disposition != "quarantine" || second[1].DispositionReason != "playback" || !second[1].Background {
		t.Fatal("scripted metadata lost")
	}
}
