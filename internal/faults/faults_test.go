package faults

import (
	"context"
	"errors"
	"testing"

	"yanshi/internal/model"
	"yanshi/internal/moderation"
)

type textModel struct{ text string }

func (m textModel) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	for _, r := range m.text {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		onDelta(model.Delta{Text: string(r)})
	}
	return &model.Response{Content: model.TextBlocks(m.text)}, nil
}

func TestRules(t *testing.T) {
	in := &Injector{}
	if _, err := in.Add(Rule{Kind: "disk_full"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := in.Add(Rule{Kind: ModelDelay, Params: map[string]string{"delay_ms": "x"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad param: %v", err)
	}
	r, _ := in.Add(Rule{Kind: ModelError, SessionID: "s1", Count: 2})
	ctx1, ctx2 := WithSession(context.Background(), "s1"), WithSession(context.Background(), "s2")
	if _, ok := in.Take(ctx2, ModelError); ok {
		t.Fatal("rule for s1 fired for s2")
	}
	for i := range 2 {
		if _, ok := in.Take(ctx1, ModelError); !ok {
			t.Fatalf("take %d did not fire", i)
		}
	}
	if _, ok := in.Take(ctx1, ModelError); ok {
		t.Fatal("rule fired after its count")
	}
	rules, recent := in.Snapshot()
	if len(rules) != 0 || len(recent) != 2 || recent[0].RuleID != r.ID || recent[0].SessionID != "s1" {
		t.Fatalf("rules %v recent %v", rules, recent)
	}
	in.Add(Rule{Kind: CatalogError})
	in.Delete("")
	if rules, _ := in.Snapshot(); len(rules) != 0 {
		t.Fatal("delete all")
	}
	var nilInjector *Injector
	if _, ok := nilInjector.Take(ctx1, ModelError); ok {
		t.Fatal("nil injector fired")
	}
}

func TestModelFaults(t *testing.T) {
	in := &Injector{}
	m := Model{Next: textModel{"一二三四五六七八九十"}, Injector: in}
	ctx := context.Background()
	in.Add(Rule{Kind: ModelRateLimit, Count: 1, Params: map[string]string{"retry_after": "7"}})
	var pe *model.ProviderError
	if _, err := m.Generate(ctx, &model.Request{Model: "m"}, nil); !errors.As(err, &pe) || pe.Status != 429 || pe.RetryAfter.Seconds() != 7 || !model.Retryable(err) {
		t.Fatalf("rate limit: %v", err)
	}
	in.Add(Rule{Kind: ModelStreamCut, Count: 1, Params: map[string]string{"after_chars": "3"}})
	got := ""
	_, err := m.Generate(ctx, &model.Request{Model: "m"}, func(d model.Delta) { got += d.Text })
	if !errors.Is(err, model.ErrStalled) || got != "一二三" {
		t.Fatalf("stream cut: %q %v", got, err)
	}
	if resp, err := m.Generate(ctx, &model.Request{Model: "m"}, func(model.Delta) {}); err != nil || model.Text(resp.Content) != "一二三四五六七八九十" {
		t.Fatalf("no rule: %v", err)
	}
	in.Add(Rule{Kind: ModerationDown, Count: 1})
	if _, err := moderation.Check(ctx, Moderator{Injector: in}, moderation.Request{}); !errors.Is(err, moderation.ErrUnavailable) {
		t.Fatalf("moderation: %v", err)
	}
}
