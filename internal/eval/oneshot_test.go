package eval

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/oneshot"
)

type oneshotEvalModel struct{ invalid bool }

func (p oneshotEvalModel) Generate(_ context.Context, r *model.Request, _ func(model.Delta)) (*model.Response, error) {
	text := `{"brief":"good ` + r.Model + `"}`
	if p.invalid {
		text = "invalid JSON"
	}
	return &model.Response{Content: model.TextBlocks(text), Usage: &v1.Usage{InputTokens: 5, OutputTokens: 3}}, nil
}

func oneshotEvalConfig(t *testing.T) Config {
	t.Helper()
	agents, err := agentdef.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var defs []*oneshot.Def
	for _, v := range []string{"1", "2"} {
		defs = append(defs, &oneshot.Def{Name: "brief", Version: v, Model: "fake/v" + v, Instructions: "概括", Input: oneshot.Input{MaxChars: 100}, Concurrency: 2, MaxOutputTokens: 20,
			Output: map[string]any{"type": "object", "properties": map[string]any{"brief": map[string]any{"type": "string"}}, "required": []string{"brief"}}})
	}
	shots, err := oneshot.NewRegistry(defs...)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := shots.ParseReleases([]byte("brief: {stable: \"1\"}"))
	if err != nil {
		t.Fatal(err)
	}
	shots.SetReleases(rel)
	gw := model.NewGateway()
	gw.Register("fake", oneshotEvalModel{})
	gw.Register("judge", fixedJudge{})
	return Config{Agents: agents, Oneshots: shots, Model: gw, Judge: "judge/any", AgentVersion: "does-not-exist", TurnTimeout: time.Second, Trials: 1, Parallel: 1}
}

func TestOneshotEvalShapeAndVersion(t *testing.T) {
	dir := t.TempDir()
	writeCase(t, dir, "brief", "name: brief\noneshot: brief\ntrials: 1\nturns:\n  - input: 第一段\n    expect: {reply_contains: [good, v2], reply_excludes: [v1], judge: 判断 good, output_schema_valid: true}\n  - input: 第二段\n    expect: {reply_contains: [good, v2]}\n")
	cases, err := LoadSuite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cases[0].Agent != "" {
		t.Fatal("oneshot got implicit agent")
	}
	cfg := oneshotEvalConfig(t)
	cfg.OneshotVersion = "2"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := Run(ctx, cfg, cases)
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Cases[0]
	if c.Passed != 1 || c.Completed == nil || *c.Completed != 1 || c.AvgTokens != 16 || c.AvgCalls != 0 || c.AvgCompactions != 0 || c.CacheHit != 0 {
		t.Fatalf("case: %+v", c)
	}
	if c.JudgeAvg == nil || *c.JudgeAvg != 5 {
		t.Fatalf("judge: %+v", c)
	}
}

func TestOneshotEvalFailureKeepsBothCallsCost(t *testing.T) {
	cfg := oneshotEvalConfig(t)
	cfg.Model.Register("fake", oneshotEvalModel{invalid: true})
	c := &Case{Name: "brief", Oneshot: "brief", Trials: 1, Turns: []Turn{{Input: "原文"}}}
	rep, err := Run(context.Background(), cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Cases[0]
	if got.Passed != 0 || got.Completed == nil || *got.Completed != 0 || got.AvgTokens != 16 || got.IncompleteCost == nil || got.IncompleteCost.Tokens != 16 {
		t.Fatalf("cost: %+v", got)
	}
}

func TestOneshotEvalHasNoSession(t *testing.T) {
	cfg := oneshotEvalConfig(t)
	cfg.defaults()
	c := &Case{Name: "brief", Oneshot: "brief", Turns: []Turn{{Input: "原文"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	tr := in.oneshotTrial(ctx, c, BusinessLine, "test-user")
	if !tr.Completed || tr.Tokens != 8 {
		t.Fatalf("trial: %+v", tr)
	}
	// 无 Session 的 trial 不会写入 Session 索引。
	page, _, err := in.svc.Index.List(ctx, BusinessLine, "test-user", "", 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("oneshot created sessions: %+v %v", page, err)
	}
}

func TestOneshotCaseRejectsSessionFields(t *testing.T) {
	for _, extra := range []string{"agent: assistant\n", "setup: {device: {label: pc}}\n", "call: true\n"} {
		dir := t.TempDir()
		writeCase(t, dir, "bad", "name: bad\noneshot: brief\n"+extra+"turns: [{input: hi}]\n")
		if _, err := LoadSuite(dir); err == nil {
			t.Fatal("accepted", extra)
		}
	}
	for _, extra := range []string{"new_session: true", "captures: [{speaker: wearer, text: hi}]", "expect: {calls: [clock_now]}"} {
		dir := t.TempDir()
		writeCase(t, dir, "bad", "name: bad\noneshot: brief\nturns:\n  - input: hi\n    "+extra+"\n")
		if _, err := LoadSuite(dir); err == nil {
			t.Fatal("accepted", extra)
		}
	}
	cfg := oneshotEvalConfig(t)
	cfg.OneshotVersion = "missing"
	if _, err := deriveOneshots(cfg, []*Case{{Name: "brief", Oneshot: "brief"}}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
}
