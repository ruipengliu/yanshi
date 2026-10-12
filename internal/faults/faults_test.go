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

type embedModel struct{ textModel }

func (embedModel) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

// TestEmbedPassesThrough：故障注入的包装转发嵌入请求（经 model.Guard 时同样可用），否则开启 -faults 后 Memory 召回失败。
func TestEmbedPassesThrough(t *testing.T) {
	gw := model.NewGateway()
	gw.Register("p", &model.Guard{Name: "p", Provider: Model{Next: embedModel{}, Injector: &Injector{}}})
	if vecs, err := gw.Embed(context.Background(), "p/e", []string{"a", "b"}); err != nil || len(vecs) != 2 {
		t.Fatalf("embed through the fault wrapper: %v, %v", vecs, err)
	}
	if _, err := (Model{Next: textModel{}}).Embed(context.Background(), "m", []string{"a"}); err == nil {
		t.Fatal("embed on a provider without embeddings succeeded")
	}
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

// TestModeratorSessionFromRequest：按 Session 的 ModerationDown 规则以送检请求自带的 Session 匹配，
// 检查点不必在 ctx 中记下 Session（Call 的转写原来就没有记，规则从不生效）。
func TestModeratorSessionFromRequest(t *testing.T) {
	in := &Injector{}
	in.Add(Rule{Kind: ModerationDown, SessionID: "s1"})
	m := Moderator{Next: moderation.Mock{}, Injector: in}
	ctx := context.Background()
	if _, err := moderation.Check(ctx, m, moderation.ForTranscript("bl", "s2", "user", "你好")); err != nil {
		t.Fatalf("rule for s1 fired for s2: %v", err)
	}
	if _, err := moderation.Check(ctx, m, moderation.ForTranscript("bl", "s1", "user", "你好")); !errors.Is(err, moderation.ErrUnavailable) {
		t.Fatalf("rule for s1 did not fire: %v", err)
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
