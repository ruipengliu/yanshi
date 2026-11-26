package eval

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/model/openaicompat"
)

// usageModel 回显最后一条用户消息，带固定的用量，回复中写上自己的模型名。
type usageModel struct{ name string }

func (u usageModel) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	return &model.Response{Content: model.TextBlocks(u.name + " echo: " + model.Text(req.Messages[len(req.Messages)-1].Content)),
		Usage: &v1.Usage{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 5}}, nil
}

// endpointLoop 是最简外部服务：每步用请求 models 的第一个、以步令牌为 API key 经评测的模型端点调用模型。
func endpointLoop(t *testing.T, endpoint string, models *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req v1.LoopStepRequest
		if err := protojson.Unmarshal(b, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		*models = append(*models, req.GetModels()...)
		mu.Unlock()
		last := req.GetMessages()[len(req.GetMessages())-1]
		p := &openaicompat.Provider{BaseURL: "http://" + endpoint + "/v1/loop", APIKey: req.GetStepToken()}
		resp, err := p.Generate(r.Context(), &model.Request{Model: req.GetModels()[0], System: req.GetSystem(),
			Messages: []model.Message{{Role: model.RoleUser, Content: last.GetContent()}}}, nil)
		if err != nil {
			t.Logf("loop model call: %v", err)
			http.Error(w, "model failed", http.StatusBadGateway)
			return
		}
		out, _ := protojson.Marshal(&v1.LoopStepResponse{Content: resp.Content})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// 评测驱动外部循环（ADR-0063）：服务经评测的模型端点调用模型，经端点的用量计入报告的 token 与缓存命中；
// 评测换模型时服务从请求的 models 得知新模型；指向未接上的循环的用例跳过。
func TestEvalDrivesExternalLoop(t *testing.T) {
	for _, tc := range []struct {
		name, override, want string
	}{
		{name: "declared model", want: "usage/a"},
		{name: "model override", override: "usage/b", want: "usage/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCase(t, dir, "loop-echo", `name: loop-echo
agent: looper
turns:
  - input: 你好
    expect:
      reply_contains: ["echo: 你好"]
`)
			writeCase(t, dir, "loop-unplugged", `name: loop-unplugged
agent: unplugged
turns:
  - input: 你好
`)
			cases, err := LoadSuite(dir)
			if err != nil {
				t.Fatal(err)
			}
			agents, err := agentdef.NewRegistry(
				&agentdef.Def{Name: "looper", Version: "1", Loop: &agentdef.LoopRef{Server: "plan", Models: []string{"usage/a"}}, Context: agentdef.Context{Scope: agentdef.ScopeRun}},
				&agentdef.Def{Name: "unplugged", Version: "1", Loop: &agentdef.LoopRef{Server: "absent"}, Context: agentdef.Context{Scope: agentdef.ScopeRun}})
			if err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("usage", usageModel{name: "usage"})
			gw.Register("judge", fixedJudge{})
			addr := freeAddr(t)
			var mu sync.Mutex
			var seen []string
			loop := endpointLoop(t, addr, &seen, &mu)
			defer loop.Close()
			rep, err := Run(t.Context(), Config{Agents: agents, Model: gw, Judge: "judge/any", TurnTimeout: 20 * time.Second, LeaseTTL: time.Second,
				ModelOverride: tc.override, Loops: map[string]string{"plan": loop.URL}, LoopEndpointAddr: addr}, cases)
			if err != nil {
				t.Fatal(err)
			}
			byName := map[string]CaseResult{}
			for _, c := range rep.Cases {
				byName[c.Name] = c
			}
			echo := byName["loop-echo"]
			if echo.Passed != echo.Trials || echo.Trials == 0 {
				t.Fatalf("loop-echo: %+v", echo)
			}
			if echo.AvgTokens != 105 || echo.CacheHit < 0.39 || echo.CacheHit > 0.41 {
				t.Fatalf("endpoint usage not counted: tokens %d cache hit %.2f", echo.AvgTokens, echo.CacheHit)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range seen {
				if m != tc.want {
					t.Fatalf("service was offered %v, want only %s", seen, tc.want)
				}
			}
			if !strings.Contains(byName["loop-unplugged"].Skipped, "absent") {
				t.Fatalf("unplugged loop: %+v", byName["loop-unplugged"])
			}
		})
	}
}

// make eval-loop 的用例与 AgentDef（evals/loop）在 check 中就要能加载：真实运行要花 token。
func TestLoopSuiteLoads(t *testing.T) {
	cases, err := LoadSuite("../../evals/loop")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := agentdef.LoadDir("../../evals/loop/agents")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no loop cases")
	}
	for _, c := range cases {
		d, err := agents.Default(c.Agent)
		if err != nil {
			t.Fatalf("case %s: %v", c.Name, err)
		}
		if d.Loop == nil || len(d.Loop.Models) == 0 {
			t.Fatalf("case %s: agent %s is not a loop agent with loop.models", c.Name, c.Agent)
		}
	}
}
