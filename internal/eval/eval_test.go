package eval

import (
	"context"
	"fmt"
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

func (script) Generate(ctx context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	// "loop N"：连续调用 clock_now，直到上下文中有 N 个结果（模拟长 Run）。
	var n, results int
	for _, m := range req.Messages {
		if m.Role == model.RoleUser {
			_, _ = fmt.Sscanf(model.Text(m.Content), "loop %d", &n)
		}
		if m.Role == model.RoleTool {
			results++
		}
	}
	if n > 0 && results < n {
		// 每次调用耗时 150ms，使评测有机会在 Run 进行中杀掉 Worker；被取消时像真实调用一样返回错误。
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "x", Capability: "clock_now", ArgumentsJson: "{}"}}}, nil
	}
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
	rep, err := Run(ctx, Config{Agents: agents, Model: gw, Judge: "judge/any", TurnTimeout: 20 * time.Second, LeaseTTL: time.Second}, cases)
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

// TestCrashLeadsToTakeover：本轮发起 2 次调用后杀掉持有 Run 的 Worker，Run 由其他 Worker 接管并完成。
func TestCrashLeadsToTakeover(t *testing.T) {
	dir := t.TempDir()
	writeCase(t, dir, "crash", `
name: crash
trials: 1
turns:
  - input: loop 5
    crash_after_calls: 2
    expect:
      calls: [clock_now]
      takeovers: {min: 1}
      compactions: {max: 0}
`)
	rep := run(t, dir)
	if c := rep.Cases[0]; c.Passed != 1 {
		t.Fatalf("crash case failed: %+v", c)
	}
}

func TestMergeBaselineKeepsOtherCases(t *testing.T) {
	base := &Report{Cases: []CaseResult{{Name: "a", Passed: 1}, {Name: "b", Passed: 1}}}
	cur := &Report{Cases: []CaseResult{{Name: "b", Passed: 3}, {Name: "c", Passed: 2}}}
	m := MergeBaseline(base, cur)
	got := fmt.Sprintln(len(m.Cases), m.Cases[0].Name, m.Cases[1].Passed, m.Cases[2].Name)
	if got != "3 a 3 c\n" {
		t.Fatalf("merged baseline %s", got)
	}
}

func TestPlainNumbers(t *testing.T) {
	if got := plainNumbers("总额 1,234,567 元，另 277，050，又 277 050 与 277\u202f050"); got != "总额 1234567 元，另 277050，又 277050 与 277050" {
		t.Fatalf("plainNumbers = %q", got)
	}
}

// TestDeviceReadTruncatesWithNotice：超过上限的文件被截断，并附上说明（与真实 Node 一致）。
func TestDeviceReadTruncatesWithNotice(t *testing.T) {
	d := &device{files: map[string]string{"big.log": strings.Repeat("x", 70<<10), "small.txt": "hi"}, writes: map[string]string{}}
	var read func(context.Context, string) ([]*v1.ContentBlock, error)
	for _, c := range d.capabilities() {
		if c.Spec.GetName() == "read_file" {
			read = c.Handler
		}
	}
	big, _ := read(context.Background(), `{"path":"big.log"}`)
	if s := model.Text(big); !strings.Contains(s, "已截断：文件共 71680 字节") || len(s) > 70<<10 {
		t.Fatalf("big file not truncated with a notice (%d bytes)", len(s))
	}
	small, _ := read(context.Background(), `{"path":"small.txt"}`)
	if model.Text(small) != "hi" {
		t.Fatal("small file altered")
	}
}
