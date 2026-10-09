package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
)

// script 是脚本化的被评测模型：输入以 "call <工具> <参数>" 开头时调用该工具，之后回复 "done"；否则复述输入。
type script struct{}

func (script) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == model.RoleTool {
		return &model.Response{Content: model.TextBlocks("done: " + model.Text(last.Content))}, nil
	}
	in := model.Text(last.Content)
	if rest, ok := strings.CutPrefix(in, "call "); ok {
		name, args, _ := strings.Cut(rest, " ")
		if args == "" {
			args = "{}"
		}
		return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "x", Capability: name, ArgumentsJson: args}}}, nil
	}
	return &model.Response{Content: model.TextBlocks("echo: " + in)}, nil
}

// fixedJudge 总是给出 3 分（低于通过线）或 5 分（回复包含 "good"）。
type fixedJudge struct{}

func (fixedJudge) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	if strings.Contains(model.Text(req.Messages[0].Content), "good") {
		return &model.Response{Content: model.TextBlocks(`好的 {"score": 5, "reason": "ok"}`)}, nil
	}
	return &model.Response{Content: model.TextBlocks(`{"score": 3, "reason": "partial"}`)}, nil
}

func writeCase(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, suite string) *Report {
	t.Helper()
	cases, err := LoadSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "assistant", Version: "1", Model: "script/any",
		Capabilities: []string{"device:*", "memory_save", "clock_now"}})
	gw := model.NewGateway()
	gw.Register("script", script{})
	gw.Register("judge", fixedJudge{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := Run(ctx, Config{Agents: agents, Model: gw, Judge: "judge/any", TurnTimeout: 20 * time.Second}, cases)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestAssertionsAndApprovals(t *testing.T) {
	dir := t.TempDir()
	writeCase(t, dir, "write-approved", `name: write-approved
trials: 2
setup:
  device: {label: pc, files: {"a.txt": "hello"}}
turns:
  - input: 'call pc__write_file {"path":"out.md","content":"周五前提交"}'
    expect:
      calls: ["pc__write_file"]
      approvals: {min: 1, max: 1}
      device_writes: {"out.md": "周五"}
      reply_contains: ["wrote"]
`)
	writeCase(t, dir, "send-denied", `name: send-denied
trials: 1
setup:
  device: {label: pc}
turns:
  - input: 'call pc__send_message {"to":"a","text":"b"}'
    approve: false
    expect:
      device_sent: 0
      reply_contains: ["denied"]
`)
	writeCase(t, dir, "memory-and-judge", `name: memory-and-judge
trials: 1
setup:
  memories: [{category: plan, content: "下周搬家"}]
turns:
  - input: 'call memory_save {"category":"preference","content":"喜欢 good 咖啡"}'
    expect:
      memories: [{category: preference, contains: "咖啡"}, {category: plan, contains: "搬家"}]
      no_memories: [{contains: "身份证"}]
      judge: "回复是否 good？"
  - input: "随便聊聊"
    new_session: true
    expect:
      no_calls: ["*"]
      reply_max_chars: 5
      judge: "回复是否合适？"
`)
	writeCase(t, dir, "needs-sandbox", "name: needs-sandbox\nrequires: [sandbox]\nturns: [{input: x}]\n")
	rep := run(t, dir)
	by := map[string]CaseResult{}
	for _, c := range rep.Cases {
		by[c.Name] = c
	}
	if c := by["write-approved"]; c.Passed != 2 {
		t.Fatalf("write-approved: %+v", c)
	}
	if c := by["send-denied"]; c.Passed != 1 {
		t.Fatalf("send-denied: %+v", c)
	}
	m := by["memory-and-judge"]
	// 第一轮全部通过（评分 5）；第二轮：回复过长且评分 3 → 失败。
	if m.Passed != 0 || m.Failures["turn 2: reply_max_chars"] != 1 || m.Failures["turn 2: judge"] != 1 || m.Failures["turn 1: judge"] != 0 || m.JudgeAvg != 4 {
		t.Fatalf("memory-and-judge: %+v", m)
	}
	if by["needs-sandbox"].Skipped == "" {
		t.Fatal("sandbox case not skipped")
	}
	md := rep.Markdown(nil, nil)
	if !strings.Contains(md, "turn 2: reply_max_chars") || !strings.Contains(md, "跳过") {
		t.Fatalf("markdown:\n%s", md)
	}
}

func TestCompareFlagsRegressions(t *testing.T) {
	base := &Report{Cases: []CaseResult{{Name: "a", Trials: 3, Passed: 3, PassRate: 1, JudgeAvg: 4.6}, {Name: "b", Trials: 3, Passed: 2, PassRate: 0.67}}}
	cur := &Report{Cases: []CaseResult{
		{Name: "a", Trials: 3, Passed: 2, PassRate: 0.67, JudgeAvg: 4.0}, // 少过 1 次：不算回归；评分降 0.6：回归
		{Name: "b", Trials: 3, Passed: 0, PassRate: 0},                   // 少过 2 次：回归
		{Name: "new", Trials: 3, Passed: 0},                              // 基线没有：不算
	}}
	got := Compare(base, cur)
	if len(got) != 2 || !strings.Contains(got[0], "a: judge") || !strings.Contains(got[1], "b: pass rate") {
		t.Fatalf("regressions = %v", got)
	}
}
