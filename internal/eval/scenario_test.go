package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/eval/xinlifixture"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/service"
)

func scenarioConfig(t *testing.T) Config {
	t.Helper()
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "day", Version: "1", Model: "script/any", Capabilities: []string{"mcp:xinli/*", "memory_save", "clock_now"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("script", script{})
	cfg := Config{Agents: agents, Model: gw, Parallel: 2, TurnTimeout: 10 * time.Second}
	cfg.defaults()
	return cfg
}

func TestScenarioTimeLoad(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"invalid now", "now: yesterday\nturns: [{input: hi}]", "now must be RFC3339"},
		{"missing now", "turns: [{input: hi, business_day: '2026-10-23'}]", "business_day requires now"},
		{"invalid day", "now: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi, business_day: '2026-02-30'}]", "business_day must be YYYY-MM-DD"},
		{"backwards", "now: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi, business_day: '2026-10-22'}]", "cannot go backwards"},
		{"same session", "now: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi}, {input: hi, business_day: '2026-10-24'}]", "requires new_session"},
		{"redo", "now: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi}, {redo: true, business_day: '2026-10-23'}]", "cannot accompany redo"},
		{"oneshot", "oneshot: test\nnow: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi}]", "cannot configure Session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCase(t, dir, "invalid", "name: invalid\n"+tc.body+"\n")
			if _, err := LoadSuite(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load error = %v; want %q", err, tc.want)
			}
		})
	}
	dir := t.TempDir()
	writeCase(t, dir, "valid", "name: valid\nnow: '2026-10-23T18:30:00+08:00'\nturns: [{input: hi, business_day: '2026-10-23'}, {input: hi, business_day: '2026-10-23'}, {input: hi, new_session: true, business_day: '2026-10-24'}]\n")
	cases, err := LoadSuite(dir)
	if err != nil || cases[0].Now == "" || cases[0].Turns[2].BusinessDay != "2026-10-24" {
		t.Fatalf("scenario load = %+v, %v", cases, err)
	}
}

func TestScenarioBusinessDaysAndReplacement(t *testing.T) {
	c := &Case{Name: "four-days", Agent: "day", Now: "2026-10-12T18:30:00+08:00", Trials: 2}
	c.Setup.Xinli = &XinliSetup{}
	for i, day := range []string{"2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15"} {
		content := fmt.Sprintf("第%d天的清单", i+1)
		c.Turns = append(c.Turns,
			Turn{BusinessDay: day, NewSession: i > 0, Captures: []CaptureSegment{{Speaker: "wearer", Text: "我来发清单。", At: day + " 09:00"}}, Input: `call mcp_xinli__captures {"all":true}`, Expect: Expect{ReplyContains: []string{day, "#1.0"}}},
			Turn{Input: `call mcp_xinli__file_changes {"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"清单","summary":"发清单","projects":[{"match":"none"}]}]}}`, Expect: Expect{ReplyExcludes: []string{"没有被接受"}}},
			Turn{Input: fmt.Sprintf(`call mcp_xinli__file_changes {"source":{"day":true},"commitments":{"create":[{"exchange":"x1.0","kind":"mine","content":%q,"refs":["#1.0"]},{"exchange":"x1.0","kind":"mine","content":"重交时去掉的事项","refs":["#1.0"]}]}}`, content), Expect: Expect{ReplyContains: []string{"均已核实"}}},
		)
		if i == 1 {
			// 同一业务日换绑后重交，也应取代当天的整组操作，不能留下旧清单。
			c.Turns = append(c.Turns, Turn{BusinessDay: day, NewSession: true, Input: `call mcp_xinli__captures {"all":true}`, Expect: Expect{ReplyContains: []string{"#1.0", day}}})
		}
		n := i + 1
		c.Turns = append(c.Turns,
			Turn{BusinessDay: day, Input: fmt.Sprintf(`call mcp_xinli__file_changes {"source":{"day":true},"commitments":{"create":[{"exchange":"x1.0","kind":"mine","content":%q,"refs":["#1.0"]}]}}`, content), Expect: Expect{ReplyContains: []string{"均已核实"}, Xinli: &XinliExpect{CommitmentsCount: &Range{Min: &n, Max: &n}, NoCommitments: []CommitmentMatch{{Contains: "重交时去掉"}}}}},
			Turn{Input: `call mcp_xinli__file_day {"day":{"headline":"当天总览"}}`, Expect: Expect{ReplyContains: []string{"已收到 " + day + " 的整理"}}},
		)
	}
	final := &XinliExpect{}
	for i := range 4 {
		final.Commitments = append(final.Commitments, CommitmentMatch{Kind: "mine", Contains: fmt.Sprintf("第%d天的清单", i+1)})
	}
	c.Turns = append(c.Turns, Turn{Input: "检查最终账本", Expect: Expect{Xinli: final}})
	// 混跑现实时间用例，防止初始负偏移与跨日前拨污染其他 trial。
	plain := &Case{Name: "real-day", Agent: "day", TimeZone: "Asia/Shanghai", Trials: 1, Turns: []Turn{{Input: `call clock_now {}`, Expect: Expect{ReplyContains: []string{captureBusinessDay(time.Now())}}}}}
	rep, err := Run(t.Context(), scenarioConfig(t), []*Case{c, plain})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range rep.Cases {
		if result.Passed != result.Trials {
			t.Fatalf("scenario trial: %+v", result)
		}
	}
}

func TestScenarioMemoryAndPersonaExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &Case{Name: "expiry", Agent: "day", Now: "2026-10-23T18:30:00+08:00"}
	in, err := start(ctx, scenarioConfig(t), []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	if in.svc.Store.Clock != in.clock || in.mems.Clock != in.clock {
		t.Fatal("kernel and Memory do not share the scenario clock")
	}
	req := memory.SaveRequest{BusinessLine: BusinessLine, EndUser: "expiry", CallID: "birthday", Category: memory.Plan, Content: "下个月八号过生日", ValidUntil: "2026-11-08"}
	if _, err := in.mems.Save(ctx, req); err != nil {
		t.Fatalf("scenario future plan rejected: %v", err)
	}
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "expiry", Agent: "day"})
	if err != nil {
		t.Fatal(err)
	}
	const day = "2026-10-23"
	if err := in.xinli.BindDay(ctx, BusinessLine, "expiry", day, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := in.submitCaptures(ctx, sid, day, 1, []CaptureSegment{{Speaker: "wearer", Text: "我来发清单。", At: "now-15m"}}); err != nil {
		t.Fatalf("scenario captures rejected: %v", err)
	}
	obs, err := in.turn(ctx, sid, Turn{Input: `call mcp_xinli__captures {"all":true}`}, 10*time.Second)
	if err != nil || obs == nil || !strings.Contains(obs.reply, "18:15") || !strings.Contains(obs.reply, day) {
		t.Fatalf("scenario relative capture time: %+v, %v", obs, err)
	}
	if got := relativeDate("today-2", in.clock.Now()); got != "2026-10-21" {
		t.Fatalf("scenario relative seed date = %s", got)
	}
	if err := in.advanceBusinessDay(ctx, c.Now, "2026-10-30"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.xinli.AppendCaptures(ctx, BusinessLine, "expiry", day, "still-valid", []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "期限前仍可补传"}}); err != nil {
		t.Fatalf("captures expired before the scenario deadline: %v", err)
	}
	if err := in.advanceBusinessDay(ctx, c.Now, "2026-10-31"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.xinli.AppendCaptures(ctx, BusinessLine, "expiry", day, "expired", []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "过期补传"}}); err == nil {
		t.Fatal("Persona ignored the advanced scenario clock")
	}
	if err := in.advanceBusinessDay(ctx, c.Now, "2026-11-09"); err != nil {
		t.Fatal(err)
	}
	req.CallID = "expired-plan"
	if _, err := in.mems.Save(ctx, req); err == nil || !strings.Contains(err.Error(), "already passed") {
		t.Fatalf("expired scenario plan: %v", err)
	}
	mems, err := in.mems.Store.List(ctx, "expiry", []memory.Scope{{BusinessLine: BusinessLine}}, in.clock.Now(), memory.DefaultMaxPerUser)
	if err != nil || len(mems) != 0 {
		t.Fatalf("scenario expired memories = %v, %v", mems, err)
	}
}

func TestLegacyScenarioClockDefaults(t *testing.T) {
	dir := t.TempDir()
	writeCase(t, dir, "legacy", "name: legacy\nturns: [{input: hi, captures: [{speaker: wearer, text: 原话, at: '2026-10-23 09:00'}]}]\n")
	cases, err := LoadSuite(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]
	if c.Now != "" || c.Turns[0].BusinessDay != "" || isolatedClock(c) || c.Turns[0].JudgeState || c.Turns[0].Captures[0].At != "2026-10-23 09:00" {
		t.Fatalf("legacy case changed: %+v", c)
	}
	before := time.Now()
	clk, scenario, err := scenarioClock(cases)
	if err != nil || scenario || clk.Offset() != 0 || clk.Now().Before(before) || clk.Now().After(time.Now()) {
		t.Fatalf("legacy clock changed: %v, %v", scenario, err)
	}
}
