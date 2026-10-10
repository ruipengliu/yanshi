package moderation_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"yanshi/internal/moderation"
)

type failing struct{}

func (failing) Check(context.Context, moderation.Request) (moderation.Verdict, error) {
	return moderation.Verdict{}, errors.New("timeout")
}

func TestMockAndCheck(t *testing.T) {
	ctx := context.Background()
	m := moderation.Mock{}
	if v, err := moderation.Check(ctx, m, moderation.Request{Stage: moderation.Input, Text: "你好【违规测试】"}); err != nil || !v.Block || v.Labels[0] != "mock" {
		t.Fatalf("marker not blocked: %+v %v", v, err)
	}
	if v, _ := moderation.Check(ctx, m, moderation.Request{Stage: moderation.Output, Text: "今天天气不错"}); v.Block {
		t.Fatal("ordinary text blocked")
	}
	if v, err := moderation.Check(ctx, nil, moderation.Request{Text: "【违规测试】"}); err != nil || v.Block {
		t.Fatal("nil moderator must allow")
	}
	if _, err := moderation.Check(ctx, failing{}, moderation.Request{Stage: moderation.Input}); !errors.Is(err, moderation.ErrUnavailable) {
		t.Fatalf("provider error not reported as unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "terms.txt")
	_ = os.WriteFile(path, []byte("# 注释\n\n禁词\n"), 0o644)
	terms, err := moderation.LoadTerms(path)
	if err != nil || len(terms) != 1 {
		t.Fatalf("terms %v %v", terms, err)
	}
	if v, _ := moderation.Check(ctx, moderation.Mock{Terms: terms}, moderation.Request{Text: "含禁词"}); !v.Block {
		t.Fatal("custom term not blocked")
	}
}
