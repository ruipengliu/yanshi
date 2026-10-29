package recognition_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
