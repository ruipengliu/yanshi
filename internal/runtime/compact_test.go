package runtime

import (
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func TestTranscriptStartsWithLatestSummary(t *testing.T) {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).           // 2
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).                           // 3
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1")}). // 4
		add(&v1.RunInterrupted{RunId: "r1"}).                                       // 5：c1 永远不会有结果
		add(&v1.RunRequested{RunId: "r2", Input: model.TextBlocks("b")}).           // 6
		add(&v1.AttemptStarted{RunId: "r2", Attempt: 1}).                           // 7
		add(&v1.AssistantMessage{RunId: "r2", Attempt: 1, ToolCalls: calls("c2")}). // 8
		add(&v1.ToolResult{RunId: "r2", Attempt: 1, CallId: "c2"}).                 // 9
		add(&v1.ContextCompacted{RunId: "r2", Attempt: 1, ThroughSeq: 6, Summary: model.TextBlocks("S")})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	msgs := Transcript(st, 0)
	if got, want := shape(msgs), "user:"+summaryPrefix+"S assistant[c2] tool:c2"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTranscriptTruncatesLargeToolResult(t *testing.T) {
	big := strings.Repeat("数据", 1000) // 约 2000 tokens
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1")}).
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c1", Content: model.TextBlocks(big)})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	got := model.Text(Transcript(st, 100)[2].Content)
	if n := model.EstimateText(got); n > 120 || !strings.Contains(got, "已截断：原始约 2000 tokens") {
		t.Fatalf("truncated result (%d tokens): %q", n, got)
	}
	if model.Text(Transcript(st, 0)[2].Content) != big {
		t.Fatal("maxToolResult 0 must not truncate")
	}
}

// longRun 构造一个 Run：每轮一次调用，结果约 size tokens。
func longRun(turns, size int) *session.State {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("go")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1})
	for i := range turns {
		id := "c" + string(rune('a'+i))
		b.add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls(id)}).
			add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: id, Content: model.TextBlocks(strings.Repeat("x", size*3))})
	}
	st, err := session.Reduce(b.events)
	if err != nil {
		panic(err)
	}
	return st
}

func TestPlanCompaction(t *testing.T) {
	cfg := agentdef.Context{Window: 1000, CompactAt: 0.75, KeepRecent: 200, MaxToolResult: 200}
	req := func(st *session.State) *model.Request {
		return &model.Request{Messages: Transcript(st, cfg.MaxToolResult)}
	}

	st := longRun(3, 50)
	if got := planCompaction(st, req(st), cfg, false); got != 0 {
		t.Fatalf("small context compacted through %d", got)
	}
	if got := planCompaction(st, req(st), cfg, true); got == 0 {
		t.Fatal("forced compaction found no cut")
	}

	// 积压远超阈值：每次压缩受摘要请求自身的预算限制，需要多步，但必须收敛到阈值以下，
	// 且始终保留约 KeepRecent 的原文。
	st = longRun(20, 100)
	steps := 0
	for {
		through := planCompaction(st, req(st), cfg, false)
		if through == 0 {
			break
		}
		if !st.CanCut(through) || through >= st.History[len(st.History)-1].GetSeq() {
			t.Fatalf("invalid cut %d", through)
		}
		e := &v1.Event{SessionId: "s", Seq: st.Seq + 1, Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{
			RunId: "r1", Attempt: 1, ThroughSeq: through, Summary: model.TextBlocks(strings.Repeat("s", 60)),
		}}}
		if err := st.Apply(e); err != nil {
			t.Fatal(err)
		}
		if steps++; steps > 10 {
			t.Fatal("compaction does not converge")
		}
	}
	if steps < 2 {
		t.Fatalf("compacted in %d steps, want several", steps)
	}
	if n := estimate(st, req(st), cfg.MaxToolResult); n > cfg.Threshold() {
		t.Fatalf("context still %d tokens after compaction", n)
	}
	kept := 0
	for _, e := range st.History {
		kept += eventTokens(e, cfg.MaxToolResult)
	}
	if kept < cfg.KeepRecent {
		t.Fatalf("kept only %d tokens of recent history, want >= %d", kept, cfg.KeepRecent)
	}
}
