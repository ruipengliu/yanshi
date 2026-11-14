// Package recognition 只定义识别适配契约；音频由宿主持有，fake 不读取、不保存音频。
package recognition

import (
	"context"
	"errors"
	"time"

	"xinli/capture"
)

// ChunkRef 是宿主内音频分片的临时引用，不是文件内容或服务端工件。
type ChunkRef struct{ ID string }

type Recognizer interface {
	Recognize(context.Context, ChunkRef) ([]capture.Segment, error)
}

type Result struct {
	Segments []capture.Segment
	Delay    time.Duration
	Fail     bool
	// SpeakerOverrides 按段号注入标签错误，SpeakerRefs 模拟稳定实体赋值。
	SpeakerOverrides map[int]string
	SpeakerRefs      map[int]string
}

type Wait func(context.Context, time.Duration) error

type Fake struct {
	results map[string]Result
	wait    Wait
}

var ErrRecognition = errors.New("recognition failed")

// NewFake 复制脚本，使同一分片的结果只取决于配置，与调用顺序和并发无关。
func NewFake(results map[string]Result, wait Wait) *Fake {
	f := &Fake{results: map[string]Result{}, wait: wait}
	if f.wait == nil {
		f.wait = waitTimer
	}
	for id, result := range results {
		result.Segments = capture.CloneSegments(result.Segments)
		for i, speaker := range result.SpeakerOverrides {
			if i >= 0 && i < len(result.Segments) {
				result.Segments[i].Speaker = speaker
			}
		}
		for i, ref := range result.SpeakerRefs {
			if i >= 0 && i < len(result.Segments) {
				result.Segments[i].SpeakerRef = ref
			}
		}
		result.SpeakerOverrides, result.SpeakerRefs = nil, nil
		f.results[id] = result
	}
	return f
}

func (f *Fake) Recognize(ctx context.Context, ref ChunkRef) ([]capture.Segment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, ok := f.results[ref.ID]
	if !ok {
		return nil, ErrRecognition
	}
	if err := f.wait(ctx, result.Delay); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Fail {
		return nil, ErrRecognition
	}
	return capture.CloneSegments(result.Segments), nil
}

func waitTimer(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
