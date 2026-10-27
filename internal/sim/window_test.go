package sim

import (
	"context"
	"errors"
	"strings"
	"testing"

	"yanshi/internal/model"
)

// 加载用的窗口变大后，主请求、摘要请求与模型超长错误仍必须以原来的 3200 为界。
func TestSimulationWindowRemainsSmall(t *testing.T) {
	if simWindow != 3200 || simContext.Threshold() != 2400 || simContext.Window <= simWindow {
		t.Fatalf("unexpected window configuration: %+v", simContext)
	}
	exact := &model.Request{System: strings.Repeat("p", simWindow*3)}
	if model.EstimateRequest(exact) != simWindow || checkRequest(exact) != nil {
		t.Fatal("exact window rejected")
	}
	for _, prefix := range []string{"", "【上下文压缩】"} {
		req := &model.Request{System: prefix + strings.Repeat("p", simWindow*3+1)}
		if model.EstimateRequest(req) >= simContext.Window {
			t.Fatal("test must fit the larger configuration window")
		}
		w := &World{}
		_, err := (&simModel{w: w}).Generate(context.Background(), req, nil)
		if !errors.Is(err, model.ErrContextOverflow) || w.Stats.Overflows != 1 || len(w.violations) != 1 {
			t.Fatalf("oversize request accepted: %v, %+v", err, w.Stats)
		}
	}
}
