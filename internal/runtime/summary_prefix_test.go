package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// TestTranscriptThroughIsAPrefix：在每个闭合边界处截取的前缀都恰好是完整上下文的前缀（含召回），
// 摘要请求因此能命中主请求留下的缓存。
func TestTranscriptThroughIsAPrefix(t *testing.T) {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{BusinessLine: "bl"}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.MemoryRecalled{RunId: "r1", Attempt: 1, Items: []*v1.RecalledMemory{{Id: "m", BusinessLine: "bl", Category: "preference", Content: "简短"}}})
	for i := range 6 {
		id := "c" + string(rune('a'+i))
		b.add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls(id)}).
			add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: id, Content: model.TextBlocks(strings.Repeat("x", 90))})
	}
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	full := Transcript(st, 0)
	for _, e := range st.History {
		if !st.CanCut(e.GetSeq()) {
			continue
		}
		prefix := transcriptThrough(st, 0, e.GetSeq())
		if len(prefix) > len(full) || !reflect.DeepEqual(prefix, full[:len(prefix)]) {
			t.Fatalf("transcript through %d is not a prefix of the full transcript", e.GetSeq())
		}
	}
	// 压缩之后（当前 Run 的输入已被压缩、召回紧跟摘要）同样成立。
	b.add(&v1.ContextCompacted{RunId: "r1", Attempt: 1, ThroughSeq: 8, Summary: model.TextBlocks("S")})
	st, _ = session.Reduce(b.events)
	full = Transcript(st, 0)
	prefix := transcriptThrough(st, 0, st.History[1].GetSeq())
	if !reflect.DeepEqual(prefix, full[:len(prefix)]) || !strings.Contains(model.Text(prefix[1].Content), "简短") {
		t.Fatalf("prefix after compaction: %s vs %s", shape(prefix), shape(full))
	}
}

// fakeSummarizer 记录收到的请求；toolOnly 为 true 时在前缀模式下只返回调用、不给文本。
type fakeSummarizer struct {
	toolOnly bool
	reqs     []*model.Request
}

func (f *fakeSummarizer) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	f.reqs = append(f.reqs, req)
	if f.toolOnly && len(req.Tools) > 0 {
		return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "x", Capability: "read_file"}}}, nil
	}
	return &model.Response{Content: model.TextBlocks("摘要")}, nil
}

func TestModelCompactorUsesPrefixAndFallsBack(t *testing.T) {
	prefix := &model.Request{System: "你是助手", Tools: []model.ToolSpec{{Name: "read_file"}},
		Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks("a")}}}
	f := &fakeSummarizer{}
	resp, err := ModelCompactor{Model: f}.Summarize(context.Background(), SummaryInput{Prefix: prefix, TargetTokens: 100})
	if err != nil || model.Text(resp.Content) != "摘要" || len(f.reqs) != 1 {
		t.Fatalf("prefix mode: %v %v %d requests", resp, err, len(f.reqs))
	}
	req := f.reqs[0]
	if req.System != prefix.System || len(req.Tools) != 1 || !reflect.DeepEqual(req.Messages[:1], prefix.Messages) ||
		!IsSummaryRequest(req) {
		t.Fatalf("summary request does not extend the main prefix: %+v", req)
	}
	// 模型只给出调用时退回文本模式（不带工具）。
	f = &fakeSummarizer{toolOnly: true}
	resp, err = ModelCompactor{Model: f}.Summarize(context.Background(), SummaryInput{Prefix: prefix, TargetTokens: 100})
	if err != nil || model.Text(resp.Content) != "摘要" || len(f.reqs) != 2 || len(f.reqs[1].Tools) != 0 || !IsSummaryRequest(f.reqs[1]) {
		t.Fatalf("fallback: %v %v %d requests", resp, err, len(f.reqs))
	}
}
