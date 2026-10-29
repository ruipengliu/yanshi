package sender_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"xinli/capture"
	"xinli/capture/sender"
)

type transport func(context.Context, capture.Batch) (capture.Receipt, error)

func (f transport) Send(ctx context.Context, b capture.Batch) (capture.Receipt, error) {
	return f(ctx, b)
}

func TestGenerationStopSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	id := sender.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "phone", SessionID: "s"}
	s, err := sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := sender.Open(path, id); err == nil {
		other.Close()
		t.Fatal("two writers")
	}
	batch, err := s.Enqueue("2026-10-28", "UTC", 7, []capture.Segment{{Speaker: "wearer", Text: "旧世代正文", At: "2026-10-28T12:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Flush(context.Background(), transport(func(_ context.Context, in capture.Batch) (capture.Receipt, error) {
		if in.Generation != 7 || in.CaptureID != batch.CaptureID {
			t.Fatal("rebased request")
		}
		return capture.Receipt{Generation: 8, Status: "resync_required"}, capture.ErrResync
	}))
	if !errors.Is(err, capture.ErrResync) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Flush(context.Background(), transport(func(context.Context, capture.Batch) (capture.Receipt, error) {
		t.Fatal("old generation sent")
		return capture.Receipt{}, nil
	}))
	if !errors.Is(err, capture.ErrResync) {
		t.Fatal(err)
	}
	state := s.State()
	if state.Stopped != "resync_required" || state.Entries[0].Batch.Generation != 7 || state.Entries[0].Status != "resync_required" {
		t.Fatal("stop lost")
	}
	if _, err := s.Enqueue("2026-10-28", "UTC", 8, batch.Segments); !errors.Is(err, sender.ErrStopped) {
		t.Fatal("automatic new generation", err)
	}
}

func TestStagedIsNotConfirmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	id := sender.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "phone", SessionID: "s"}
	s, err := sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	batch, err := s.Enqueue("", "UTC", 0, []capture.Segment{{Speaker: "wearer", Text: "暂存正文", At: "2026-10-28T12:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := transport(func(_ context.Context, in capture.Batch) (capture.Receipt, error) {
		if in.BusinessDay != "2026-10-28" || in.CaptureID != batch.CaptureID {
			t.Fatal("day or identity changed")
		}
		return capture.Receipt{Status: "staged", BusinessDay: in.BusinessDay, NextSeq: 1}, nil
	})
	if err := s.Flush(context.Background(), staged); !errors.Is(err, sender.ErrGap) {
		t.Fatal(err)
	}
	if s.State().Entries[0].Status != "staged" || len(s.State().Entries[0].Batch.Segments) != 1 {
		t.Fatal("staged acknowledged")
	}
	// 交出的状态只能观察，不能把文件中的原请求改成异文。
	snapshot := s.State()
	snapshot.Entries[0].Batch.Segments[0].Text = "改动"
	if s.State().Entries[0].Batch.Segments[0].Text != "暂存正文" {
		t.Fatal("alias")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wrong := id
	wrong.EndUser = "another"
	if other, err := sender.Open(path, wrong); err == nil {
		other.Close()
		t.Fatal("owner mismatch")
	}
	s, err = sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Flush(context.Background(), staged); !errors.Is(err, sender.ErrGap) {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file permissions", err)
	}
}

func TestSenderProcessRestart(t *testing.T) {
	id := sender.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "phone", SessionID: "process"}
	if path := os.Getenv("YANSHI_CAPTURE_SENDER_CHILD"); path != "" {
		s, err := sender.Open(path, id)
		if err != nil {
			t.Fatal(err)
		}
		batch, err := s.Enqueue("2026-10-28", "UTC", 0, []capture.Segment{{Speaker: "wearer", Text: "重启前入队", At: "2026-10-28T12:00:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Print(batch.CaptureID)
		// 不调用 Close，验证锁由进程退出释放且已 fsync 的批次仍可补传。
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "process.json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSenderProcessRestart$")
	child.Env = append(os.Environ(), "YANSHI_CAPTURE_SENDER_CHILD="+path)
	output, err := child.Output()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Flush(ctx, transport(func(_ context.Context, in capture.Batch) (capture.Receipt, error) {
		if in.CaptureID != string(output) || in.Seq != 1 {
			t.Fatal("process restart changed capture identity")
		}
		return capture.Receipt{Status: "accepted", BusinessDay: in.BusinessDay, NextSeq: 2, Refs: []string{"1.0"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
}
