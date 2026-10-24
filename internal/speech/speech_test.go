package speech_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/speech"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue/memqueue"
)

// fakeProvider 记录合成的文字。
type fakeProvider struct{ text string }

func (*fakeProvider) Recognize(_ context.Context, audio []byte) (string, time.Duration, error) {
	if string(audio) == "silence" {
		return "", 1500 * time.Millisecond, nil
	}
	return " 明早七点叫我起床。 ", 2100 * time.Millisecond, nil
}

func (p *fakeProvider) Synthesize(_ context.Context, text string) ([]byte, int, error) {
	p.text = text
	return make([]byte, 480), 24000, nil
}

func reply(run, text string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: run, Attempt: 1, Content: model.TextBlocks(text)}}}
}

// 识别返回去掉首尾空白的文字、按秒（向上取整）计量；播报读 Run 最后一条有文字的输出，去掉 Markdown 记号、按字计量。
// 配额用尽时两者都拒绝，且不调用提供商。
func TestTranscribeAndSpeak(t *testing.T) {
	ctx := context.Background()
	clk := clock.Real{}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	mlog, mq := memlog.New(), memqueue.New(clk)
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Random(), Clock: clk}
	svc := &service.Service{Store: store, Agents: agents, Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions(), Janitor: memqueue.New(clk)}
	uses := usage.NewMem()
	prov := &fakeProvider{}
	sp := &speech.Service{Sessions: svc, Provider: prov, ASRModel: "p/asr", TTSModel: "p/tts", IDs: ids.Random(),
		Meter: &usage.Meter{Store: uses, Prices: &usage.PriceList{Models: map[string]usage.Price{"p/asr": {Input: 1}, "p/tts": {Output: 1}}}}}

	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "demo", EndUser: "u1", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(ctx, sid, model.TextBlocks("hi"))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Load(ctx, sid)
	started := &v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: 1}}}
	if err := store.Commit(ctx, st, started, reply(res.RunID, "## 已设好\n- **明早 7:00** 的闹钟"), reply(res.RunID, "")); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.Load(ctx, sid)

	if text, err := sp.Transcribe(ctx, st, []byte("RIFF")); err != nil || text != "明早七点叫我起床。" {
		t.Fatalf("transcribe = %q, %v", text, err)
	}
	if text, err := sp.Transcribe(ctx, st, []byte("silence")); err != nil || text != "" {
		t.Fatalf("transcribe silence = %q, %v", text, err)
	}
	wav, err := sp.Speak(ctx, st, res.RunID)
	if err != nil || string(wav[:4]) != "RIFF" || len(wav) != 44+480 {
		t.Fatalf("speak = %d bytes, %v", len(wav), err)
	}
	if prov.text != "已设好\n明早 7:00 的闹钟" {
		t.Fatalf("synthesized %q", prov.text)
	}
	if _, err := sp.Speak(ctx, st, "nope"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("speak unknown run: %v", err)
	}
	sum, err := uses.Report(ctx, usage.Query{BusinessLine: "demo", GroupBy: usage.ByModel, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cost := map[string]int64{}
	for _, r := range sum {
		cost[r.Key] = r.Cost
	}
	// 2.1 秒与 1.5 秒各计 3、2 秒；播报 "已设好\n明早 7:00 的闹钟" 共 15 字。
	if cost["p/asr"] != 5 || cost["p/tts"] != 15 {
		t.Fatalf("usage = %v", cost)
	}

	limits := usage.Limits{EndUserDaily: 0.00001}
	if err := limits.Prepare(); err != nil {
		t.Fatal(err)
	}
	svc.Quotas = &usage.Quotas{Store: uses, Limits: map[string]usage.Limits{"demo": limits}}
	prov.text = ""
	if _, err := sp.Transcribe(ctx, st, []byte("RIFF")); !errors.Is(err, usage.ErrExceeded) {
		t.Fatalf("transcribe over quota: %v", err)
	}
	if _, err := sp.Speak(ctx, st, res.RunID); !errors.Is(err, usage.ErrExceeded) || prov.text != "" {
		t.Fatalf("speak over quota: %v", err)
	}
	if _, err := (&speech.Service{Sessions: svc}).Transcribe(ctx, st, []byte("x")); !errors.Is(err, speech.ErrNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
}

func TestSpeakable(t *testing.T) {
	if got := speech.Speakable("看[这里](https://x.y)。\n1. `a` | b\n> 引用"); got != "看这里。\na ， b\n引用" {
		t.Fatalf("speakable = %q", got)
	}
	long := strings.Repeat("一二三四五六七八九。", 100)
	if got := []rune(speech.Speakable(long)); len(got) != speech.MaxSpeakRunes || got[len(got)-1] != '。' {
		t.Fatalf("clipped to %d runes", len(got))
	}
}
