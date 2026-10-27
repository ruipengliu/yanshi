package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/eval/xinlifixture"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

// 在真实 Worker、captures 与 HTTP MCP 上走整理流程，故意制造提前结束与拒收，检验调度不会跳过未交出的原话。
type spanScript struct{ mode string }

func (s spanScript) Generate(ctx context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	var input, text string
	var calls []*v1.ToolCall
	users := 0
	for _, m := range req.Messages {
		if m.Role == model.RoleUser {
			input = model.Text(m.Content)
			users++
		}
		calls = append(calls, m.ToolCalls...)
		if m.Role == model.RoleTool {
			text += model.Text(m.Content) + "\n"
		}
	}
	if users != 1 {
		return nil, fmt.Errorf("new run carried %d user messages, want 1", users)
	}
	response := func(name, args string) (*model.Response, error) {
		r := &model.Response{Usage: &v1.Usage{InputTokens: 10, OutputTokens: 2, CachedInputTokens: 3}}
		if name != "" {
			r.ToolCalls = []*v1.ToolCall{{CallId: "x", Capability: name, ArgumentsJson: args}}
		} else {
			r.Content = model.TextBlocks("done")
		}
		return r, nil
	}
	if !strings.Contains(req.System, xinliSpanInstructions) {
		return response("", "")
	}
	if s.mode == "timeout" && len(calls) > 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if input == xinliDayInput {
		switch len(calls) {
		case 0:
			return response("mcp_xinli__day_ledger", "{}")
		case 1:
			return response("mcp_xinli__file_day", `{"day":{"headline":"整理完成"}}`)
		default:
			return response("", "")
		}
	}
	if s.mode == "stalled" {
		return response("", "")
	}
	if s.mode == "ask" {
		return response("ask_user", `{"question":"要整理哪一段？"}`)
	}
	if len(calls) == 0 {
		return response("captures", fmt.Sprintf(`{"after":"new","all":%v}`, input == xinliSpanAllInput))
	}
	refs := regexp.MustCompile(`#([0-9]+\.[0-9]+) [0-9]{2}:`).FindAllStringSubmatch(text, -1)
	if len(refs) == 0 {
		return response("", "")
	}
	last := calls[len(calls)-1]
	if last.GetCapability() == "captures" {
		positions := regexp.MustCompile(`after="([^"]+)"`).FindAllStringSubmatch(text, -1)
		if len(positions) > 0 {
			pos := positions[len(positions)-1][1]
			if strings.Contains(pos, "~") {
				if s.mode == "partial" {
					return response("", "")
				}
				return response("captures", fmt.Sprintf(`{"after":%q,"all":true}`, pos))
			}
			if s.mode == "overread" && len(calls) == 1 {
				return response("captures", fmt.Sprintf(`{"after":%q,"all":true}`, pos))
			}
		}
		if s.mode == "missing-receipt" {
			return response("", "")
		}
	}
	files := 0
	for _, c := range calls {
		if c.GetCapability() == "mcp_xinli__file_changes" {
			files++
		}
	}
	if files == 0 || s.mode == "retry" && files == 1 {
		to := refs[len(refs)-1][1]
		if s.mode == "retry" && files == 0 {
			to = "999999.0"
		}
		args, _ := json.Marshal(map[string]any{"source": map[string]string{"from": "#" + refs[0][1], "to": "#" + to},
			"exchanges": map[string]any{}, "commitments": map[string]any{}})
		return response("mcp_xinli__file_changes", string(args))
	}
	return response("", "")
}

func runSpanScript(t *testing.T, mode string, segments []CaptureSegment, turns []Turn) TrialResult {
	t.Helper()
	gw := model.NewGateway()
	gw.Register("script", spanScript{mode})
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "xinli-day", Version: "1", Model: "script/any",
		Streams: []agentdef.Stream{{Name: session.StreamTranscript}}, Capabilities: []string{"captures", "mcp:xinli/*"},
		Context: agentdef.Context{Scope: agentdef.ScopeRun}},
		&agentdef.Def{Name: "xinli-assistant", Version: "1", Model: "script/any"})
	if err != nil {
		t.Fatal(err)
	}
	c := &Case{Name: "spans", Agent: "xinli-day", XinliSpanRuns: true, Trials: 1}
	c.Setup.Xinli = &XinliSetup{}
	if turns == nil {
		turns = []Turn{{Input: "整理今天", Captures: segments}}
	}
	c.Turns = turns
	// 窗口覆盖也必须保留 scope: run；否则第二个时段的请求会带上第一个 Run。
	c.Context.Window = 24000
	c.Context.MaxToolResult = 6000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Agents: agents, Model: gw, TurnTimeout: 10 * time.Second, Parallel: 1}
	cfg.defaults()
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	return in.trial(ctx, c, 0)
}

func spanSegments() []CaptureSegment {
	return []CaptureSegment{
		{Speaker: "wearer", Text: "先喝杯茶。", At: "2026-10-23 09:00"},
		{Speaker: "wearer", Text: "今天出太阳了。", At: "2026-10-23 10:00"},
	}
}

func TestXinliSpanRuns(t *testing.T) {
	got := runSpanScript(t, "", spanSegments(), nil)
	// 两个时段各 3 次模型调用、日终 3 次；每次用量 10+2，缓存 3，调用与结果不能因聚合重复计数。
	if !got.Passed || got.Tokens != 108 || got.InputTokens != 90 || got.CachedInputTokens != 27 || got.Calls != 6 {
		t.Fatalf("two spans and final run: %+v", got)
	}
}

func TestXinliSpanRunsContinueAndRetry(t *testing.T) {
	segs := spanSegments()
	// 同一个时段跨过 6000 字符，在同一 Run 续页；另一个时段仍由另一个 Run 整理。
	segs = append([]CaptureSegment{{Speaker: "wearer", Text: strings.Repeat("闲聊。", 500), At: "2026-10-23 08:59"},
		{Speaker: "wearer", Text: strings.Repeat("闲聊。", 500), At: "2026-10-23 08:59"},
		{Speaker: "wearer", Text: strings.Repeat("闲聊。", 500), At: "2026-10-23 08:59"},
		{Speaker: "wearer", Text: strings.Repeat("闲聊。", 500), At: "2026-10-23 08:59"}}, segs...)
	got := runSpanScript(t, "retry", segs, nil)
	// 两次拒收在原 Run 内改正，长时段多读一页：比普通情况多 3 次调用、3 次模型响应。
	if !got.Passed || got.Calls != 9 || got.Tokens != 144 {
		t.Fatalf("continuation and retry: %+v", got)
	}
}

func TestXinliSpanRunsStopOnUnfiledOrIncomplete(t *testing.T) {
	for _, mode := range []string{"stalled", "missing-receipt", "partial", "overread"} {
		t.Run(mode, func(t *testing.T) {
			segs := spanSegments()
			if mode == "partial" {
				for range 5 {
					segs = append([]CaptureSegment{{Speaker: "wearer", Text: strings.Repeat("闲聊。", 500), At: "2026-10-23 08:59"}}, segs...)
				}
			}
			got := runSpanScript(t, mode, segs, nil)
			if got.Passed || got.Err == "" || got.Tokens == 0 {
				t.Fatalf("unsafe completion: %+v", got)
			}
			if mode == "missing-receipt" && !strings.Contains(got.Err, "no accepted changeset") {
				t.Fatalf("missing receipt: %s", got.Err)
			}
			if got.Calls > 3 {
				t.Fatalf("continued after unsafe completion: %+v", got)
			}
		})
	}
}

func TestXinliSpanRunsEmptyAndOngoing(t *testing.T) {
	for name, segs := range map[string][]CaptureSegment{
		"empty":   nil,
		"ongoing": {{Speaker: "wearer", Text: "还在聊。", At: "now-0m"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := runSpanScript(t, "", nil, []Turn{{Input: "整理交谈", Captures: segs}, {Input: "整理交谈"}})
			if !got.Passed || got.Calls != 2 {
				t.Fatalf("empty or ongoing should stop: %+v", got)
			}
		})
	}
}

func TestSpanReceiptMustBeNewAndMatchRange(t *testing.T) {
	c := xinlifixture.Changeset{ID: "old", Session: "s", Author: xinlifixture.AuthorDay, Source: xinlifixture.Source{From: "#2.0", To: "#3.1"}}
	if spanAccepted([]xinlifixture.Changeset{c}, []xinlifixture.Changeset{c}, "s", "2.0", "3.1") {
		t.Fatal("old receipt counted as a new accepted submission")
	}
	c.ID = "new"
	other := c
	other.ID, other.Source = "day", xinlifixture.Source{Day: true}
	if spanAccepted(nil, []xinlifixture.Changeset{c, other}, "s", "2.0", "3.1") {
		t.Fatal("span run also accepted an unrelated changeset")
	}
	if spanAccepted(nil, []xinlifixture.Changeset{c}, "s", "2.0", "3.0") || spanAccepted(nil, []xinlifixture.Changeset{c}, "other", "2.0", "3.1") {
		t.Fatal("receipt with wrong scope accepted")
	}
}

func TestXinliSpanRunsOnlyScheduleDayAgent(t *testing.T) {
	got := runSpanScript(t, "", nil, []Turn{{Input: "整理交谈"},
		{Input: "整理今天", NewSession: true, Agent: "xinli-assistant", Expect: Expect{NoCalls: []string{"*"}}}})
	if !got.Passed || got.Calls != 1 {
		t.Fatalf("assistant input became day orchestration: %+v", got)
	}
}

func TestFailedEvalTurnInterruptsAndCountsUsage(t *testing.T) {
	gw := model.NewGateway()
	gw.Register("script", spanScript{"timeout"})
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "xinli-day", Version: "1", Model: "script/any",
		Streams: []agentdef.Stream{{Name: session.StreamTranscript}}, Capabilities: []string{"captures"},
		Instructions: xinliSpanInstructions, Context: agentdef.Context{Scope: agentdef.ScopeRun}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Case{Name: "timeout", Agent: "xinli-day"}
	cfg := Config{Agents: agents, Model: gw, TurnTimeout: 500 * time.Millisecond, Parallel: 1}
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "timeout", Agent: c.Agent, AgentVersion: in.version[c.Name]})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := in.turn(ctx, sid, Turn{Input: xinliSpanInput}, cfg.TurnTimeout)
	if err == nil || obs == nil || obs.tokens != 12 || len(obs.calls) != 1 {
		t.Fatalf("partial usage on timeout: obs=%+v, err=%v", obs, err)
	}
	st, err := in.svc.Load(ctx, sid)
	if err != nil || st.Active() != nil {
		t.Fatalf("timed out run still active: state=%+v, err=%v", st, err)
	}
}

func TestXinliSpanRunsInterruptAskedRun(t *testing.T) {
	gw := model.NewGateway()
	gw.Register("script", spanScript{"ask"})
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "xinli-day", Version: "1", Model: "script/any",
		Streams: []agentdef.Stream{{Name: session.StreamTranscript}}, Capabilities: []string{"captures", "ask_user", "mcp:xinli/*"},
		Context: agentdef.Context{Scope: agentdef.ScopeRun}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Case{Name: "ask", Agent: "xinli-day", XinliSpanRuns: true, Trials: 1}
	c.Setup.Xinli = &XinliSetup{}
	c.Turns = []Turn{{Input: "整理交谈"}}
	cfg := Config{Agents: agents, Model: gw, TurnTimeout: 10 * time.Second, Parallel: 1}
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "ask", Agent: c.Agent, AgentVersion: in.version[c.Name]})
	if err != nil {
		t.Fatal(err)
	}
	// 没有回答脚本时 turn 停在提问处、不报错；拆 Run 的调度停止实验时不能留下等待回答的 Run。
	obs, err := in.spanTurns(ctx, sid, BusinessLine, "ask", c.Turns[0], cfg.TurnTimeout)
	if err == nil || !strings.Contains(err.Error(), "status asked") || obs == nil || obs.questions != 1 {
		t.Fatalf("asked span run: obs=%+v, err=%v", obs, err)
	}
	st, err := in.svc.Load(ctx, sid)
	if err != nil || st.Active() != nil {
		t.Fatalf("asked span run still active: state=%+v, err=%v", st, err)
	}
}

func TestXinliSpanRunsRejectOneShotScripts(t *testing.T) {
	in := &instance{}
	for _, turn := range []Turn{{Input: "整理今天", CrashAfterCalls: 1}, {Input: "整理今天", CardAction: &CardActionScript{}}} {
		if _, err := in.spanTurns(context.Background(), "s", BusinessLine, "u", turn, time.Second); err == nil {
			t.Fatalf("span runs accepted a one-shot script: %+v", turn)
		}
	}
}
