package extloop

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/usage"
)

var testKeys = []TokenKey{{KID: "k2", Secret: strings.Repeat("b", 32)}, {KID: "k1", Secret: strings.Repeat("a", 32)}}

func testClaims() StepClaims {
	return StepClaims{BusinessLine: "bl", Loop: "plan", SessionID: "s", RunID: "r", Attempt: 1, Turn: 2, EndUser: "u",
		AgentName: "planner", AgentVersion: "1"}
}

func keysFor(bl, loop string) ([]TokenKey, time.Duration) {
	if bl == "bl" && loop == "plan" {
		return testKeys, time.Minute
	}
	return nil, 0
}

// 步令牌以第一把密钥签发，验证接受登记的任一把；过期、换了声明、别的循环的密钥、无签名算法都被拒绝。
func TestStepToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := SignStep(testKeys, testClaims(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyStep(tok, keysFor, now.Add(30*time.Second))
	if err != nil || c.SessionID != "s" || c.EndUser != "u" || c.Turn != 2 {
		t.Fatalf("claims %+v err %v", c, err)
	}
	// 轮换：旧密钥签发的令牌在过期之前仍有效。
	old, _ := SignStep(testKeys[1:], testClaims(), now, time.Minute)
	if _, err := VerifyStep(old, keysFor, now); err != nil {
		t.Fatalf("token from the second key rejected: %v", err)
	}
	if _, err := VerifyStep(tok, keysFor, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired token accepted")
	}
	// 签发方的时钟比验证方快几秒：令牌仍有效。
	if _, err := VerifyStep(tok, keysFor, now.Add(-3*time.Second)); err != nil {
		t.Fatalf("token from a slightly fast clock rejected: %v", err)
	}
	other := testClaims()
	other.Loop = "other"
	forged, _ := SignStep(testKeys, other, now, time.Minute)
	if _, err := VerifyStep(forged, keysFor, now); err == nil {
		t.Fatal("token for an unregistered loop accepted")
	}
	parts := strings.Split(tok, ".")
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + parts[1] + "."
	if _, err := VerifyStep(none, keysFor, now); err == nil {
		t.Fatal("unsigned token accepted")
	}
	if tok, _ := SignStep(nil, testClaims(), now, time.Minute); tok != "" {
		t.Fatal("token signed without keys")
	}
}

func TestParseTokenKeys(t *testing.T) {
	env := map[string]string{"LOOP_KEY": strings.Repeat("x", 32), "SHORT": "x"}
	getenv := func(k string) string { return env[k] }
	s, err := parse([]byte("name: plan\nbusiness_line: bl\nurl: https://x\ntoken_keys: [{kid: k1, secret: \"${LOOP_KEY}\"}]\n"), getenv)
	if err != nil || len(s.TokenKeys) != 1 || s.TokenKeys[0].Secret != env["LOOP_KEY"] {
		t.Fatalf("server %+v err %v", s, err)
	}
	for name, body := range map[string]string{
		"short secret": "token_keys: [{kid: k1, secret: \"${SHORT}\"}]",
		"unset":        "token_keys: [{kid: k1, secret: \"${UNSET_LOOP_KEY}\"}]",
		"no kid":       "token_keys: [{secret: \"${LOOP_KEY}\"}]",
		"duplicate":    "token_keys: [{kid: k1, secret: \"${LOOP_KEY}\"}, {kid: k1, secret: \"${LOOP_KEY}\"}]",
	} {
		if _, err := parse([]byte("name: plan\nbusiness_line: bl\nurl: https://x\n"+body+"\n"), getenv); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// 登记配置了密钥时，每步请求带步令牌，声明与这一步一致、有效期是这一步的时限。
func TestDecideSendsStepToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var got string
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req v1.LoopStepRequest
		_ = protojson.Unmarshal(b, &req)
		got, models = req.GetStepToken(), req.GetModels()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	r := NewRegistry([]Server{{Name: "plan", BusinessLine: "bl", URL: srv.URL, Timeout: 20 * time.Second, TokenKeys: testKeys}}, nil)
	l, _ := r.Loop("bl", "plan")
	l.(*Client).Now = func() time.Time { return now }
	st := step()
	st.EndUser, st.Agent, st.Models = "u", &v1.AgentRef{Name: "planner", Version: "1"}, []string{"p/good"}
	if _, err := l.Decide(model.WithBackground(context.Background()), st, nil); err != nil {
		t.Fatal(err)
	}
	c, err := VerifyStep(got, r.Keys, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionID != "s" || c.RunID != "r" || c.Attempt != 2 || c.Turn != 3 || c.AgentName != "planner" || !c.Background ||
		!c.ExpiresAt.Time.Equal(now.Add(20*time.Second)) {
		t.Fatalf("claims %+v", c)
	}
	if len(models) != 1 || models[0] != "p/good" {
		t.Fatalf("models %v", models)
	}
}

type providerFunc func(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error)

func (f providerFunc) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	return f(ctx, req, onDelta)
}

type endpointFixture struct {
	e     *Endpoint
	srv   *httptest.Server
	clock *clock.Fake
	uses  *usage.Mem
	del   *lifecycle.MemDeletions
	calls []*model.Request
}

func newEndpoint(t *testing.T) *endpointFixture {
	t.Helper()
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "planner", Version: "1", Context: agentdef.Context{Scope: agentdef.ScopeRun},
		Loop: &agentdef.LoopRef{Server: "plan", Models: []string{"p/good"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &endpointFixture{clock: clock.NewFake(time.Now()), uses: usage.NewMem(), del: lifecycle.NewMemDeletions()}
	f.e = &Endpoint{Keys: keysFor, Agents: agents, Clock: f.clock, Now: f.clock.Now, IDs: ids.Random(), Deletions: f.del,
		Meter: &usage.Meter{Store: f.uses, Prices: &usage.PriceList{Models: map[string]usage.Price{"p/good": {Input: 1, Output: 1}}}},
		Model: providerFunc(func(_ context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
			f.calls = append(f.calls, req)
			if onDelta != nil {
				onDelta(model.Delta{Text: "你"})
				onDelta(model.Delta{Text: "好"})
			}
			return &model.Response{Content: model.TextBlocks("你好"), Usage: &v1.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2},
				ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: "note", ArgumentsJson: `{"a":1}`}}}, nil
		})}
	mux := http.NewServeMux()
	mux.Handle("POST "+EndpointPath, f.e)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *endpointFixture) token(t *testing.T) string {
	t.Helper()
	tok, err := SignStep(testKeys, testClaims(), f.clock.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *endpointFixture) post(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+EndpointPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const chatBody = `{"model":"p/good","temperature":0.2,"max_completion_tokens":50,"messages":[
 {"role":"system","content":"指令"},
 {"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AQI="}}]},
 {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"note","arguments":"{}"}}]},
 {"role":"tool","tool_call_id":"call_1","content":"已记"}],
 "tools":[{"type":"function","function":{"name":"note","description":"记一笔","parameters":{"type":"object"}}}]}`

// 非流式：请求按 OpenAI 的形状转给网关，回答按 chat.completion 返回；用量按令牌中的业务线、EndUser 与 AgentDef 计量。
func TestEndpointJSON(t *testing.T) {
	f := newEndpoint(t)
	resp := f.post(t, f.token(t), chatBody)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ch := out.Choices[0]
	if out.Model != "p/good" || ch.Message.Content != "你好" || ch.FinishReason != "tool_calls" || ch.Message.ToolCalls[0].Function.Name != "note" ||
		out.Usage.PromptTokens != 10 || out.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("response %+v", out)
	}
	in := f.calls[0]
	if in.Model != "p/good" || in.System != "指令" || in.MaxTokens != 50 || len(in.Messages) != 3 || len(in.Tools) != 1 ||
		in.Messages[0].Content[1].GetMedia().GetMimeType() != "image/png" || in.Messages[1].ToolCalls[0].GetCapability() != "note" ||
		in.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("gateway request %+v", in)
	}
	rows, err := f.uses.Report(context.Background(), usage.Query{BusinessLine: "bl", EndUser: "u", From: f.clock.Now().Add(-time.Hour),
		To: f.clock.Now().Add(time.Hour), GroupBy: usage.ByAgent})
	if err != nil || len(rows) != 1 || rows[0].Key != "planner@1" || rows[0].InputTokens != 10 || rows[0].CachedInputTokens != 4 {
		t.Fatalf("usage %+v err %v", rows, err)
	}
}

// 流式：增量按 SSE 转发，最后给出调用、结束原因、用量（include_usage）与 [DONE]。
func TestEndpointStream(t *testing.T) {
	f := newEndpoint(t)
	body := strings.Replace(chatBody, `"temperature":0.2`, `"stream":true,"stream_options":{"include_usage":true}`, 1)
	resp := f.post(t, f.token(t), body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var text, finish string
	var calls, usageChunks int
	done := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   *string           `json:"content"`
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		if c.Usage != nil {
			usageChunks++
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
			calls += len(ch.Delta.ToolCalls)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text != "你好" || calls != 1 || finish != "tool_calls" || usageChunks != 1 || !done {
		t.Fatalf("text %q calls %d finish %q usage %d done %v", text, calls, finish, usageChunks, done)
	}
}

// 提供商不流式输出时（或只输出了一部分），流结束前补发其余的文字。
func TestEndpointStreamWithoutDeltas(t *testing.T) {
	f := newEndpoint(t)
	f.e.Model = providerFunc(func(_ context.Context, _ *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
		onDelta(model.Delta{Text: "你"})
		return &model.Response{Content: model.TextBlocks("你好"), Usage: &v1.Usage{}}, nil
	})
	resp := f.post(t, f.token(t), strings.Replace(chatBody, `"temperature":0.2`, `"stream":true`, 1))
	var text string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(data), &c)
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
	}
	if text != "你好" {
		t.Fatalf("streamed text %q", text)
	}
}

// 拒绝：没有或无效的令牌、不在 loop.models 中的模型、已删除的 Session、超出配额。都不调用模型、不计量。
func TestEndpointRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*endpointFixture)
		token  func(*testing.T, *endpointFixture) string
		body   string
		status int
		code   string
	}{
		{name: "no token", token: func(*testing.T, *endpointFixture) string { return "" }, status: 401, code: "invalid_api_key"},
		{name: "expired", setup: func(f *endpointFixture) {}, token: func(t *testing.T, f *endpointFixture) string {
			tok := f.token(t)
			f.clock.Advance(2 * time.Minute)
			return tok
		}, status: 401, code: "invalid_api_key"},
		{name: "model not declared", body: strings.Replace(chatBody, "p/good", "p/other", 1), status: 400, code: "model_not_allowed"},
		{name: "session deleted", setup: func(f *endpointFixture) {
			if _, err := f.del.Mark(context.Background(), &lifecycle.Tombstone{SessionID: "s", BusinessLine: "bl", Reason: "end_user", RequestedAt: f.clock.Now()}); err != nil {
				t.Fatal(err)
			}
		}, status: 404, code: "session_deleted"},
		{name: "end user deleted", setup: func(f *endpointFixture) {
			if _, err := f.del.MarkEndUser(context.Background(), &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "d", RequestedAt: f.clock.Now()}); err != nil {
				t.Fatal(err)
			}
		}, status: 404, code: "session_deleted"},
		{name: "quota", setup: func(f *endpointFixture) {
			if err := f.uses.Record(context.Background(), &usage.Entry{ID: "before", BusinessLine: "bl", EndUser: "u", Kind: usage.Model, Cost: 2_000_000, At: f.clock.Now()}); err != nil {
				t.Fatal(err)
			}
			f.e.Quotas = &usage.Quotas{Store: f.uses, Limits: map[string]usage.Limits{"bl": {EndUserDaily: 1}}}
		}, status: 429, code: "quota_exceeded"},
		{name: "remote image", body: strings.Replace(chatBody, "data:image/png;base64,AQI=", "https://example.invalid/a.png", 1), status: 400, code: "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndpoint(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			tok := ""
			if tc.token != nil {
				tok = tc.token(t, f)
			} else {
				tok = f.token(t)
			}
			body := tc.body
			if body == "" {
				body = chatBody
			}
			resp := f.post(t, tok, body)
			var e struct {
				Error struct{ Code string } `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			if resp.StatusCode != tc.status || e.Error.Code != tc.code {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, e.Error.Code, tc.status, tc.code)
			}
			if len(f.calls) != 0 {
				t.Fatal("model called after rejection")
			}
		})
	}
}

// 提供商的错误不把原文回给外部服务；上下文超长给出 OpenAI 的错误码，框架据此压缩。
func TestEndpointModelErrors(t *testing.T) {
	f := newEndpoint(t)
	f.e.Model = providerFunc(func(context.Context, *model.Request, func(model.Delta)) (*model.Response, error) {
		return nil, model.ErrContextOverflow
	})
	resp := f.post(t, f.token(t), chatBody)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "context_length_exceeded") {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

// 持有密钥的一方能签发任意声明：有效期长于登记的一步时限、没有 EndUser 的令牌一律拒绝。
func TestStepTokenBounds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	long, _ := SignStep(testKeys, testClaims(), now, time.Hour)
	if _, err := VerifyStep(long, keysFor, now); err == nil {
		t.Fatal("token outliving the step timeout accepted")
	}
	anon := testClaims()
	anon.EndUser = ""
	tok, _ := SignStep(testKeys, anon, now, time.Minute)
	if _, err := VerifyStep(tok, keysFor, now); err == nil {
		t.Fatal("token without an end user accepted")
	}
}

func TestParseRejectsLiteralSecret(t *testing.T) {
	_, err := parse([]byte("name: plan\nbusiness_line: bl\nurl: https://x\ntoken_keys: [{kid: k1, secret: \""+strings.Repeat("x", 40)+"\"}]\n"),
		func(string) string { return "" })
	if err == nil {
		t.Fatal("literal secret accepted")
	}
}

type denyAll struct{}

func (denyAll) AllowsAgent(string, string) bool { return false }

// 令牌里的 AgentDef 须属于令牌的循环、且业务线允许它；不支持的音频格式拒绝。都不调用模型。
func TestEndpointRejectsForeignAgent(t *testing.T) {
	for name, setup := range map[string]func(*endpointFixture) string{
		"agent of another loop": func(f *endpointFixture) string {
			c := testClaims()
			c.Loop = "plan"
			c.AgentName = "other-loop-agent"
			tok, _ := SignStep(testKeys, c, f.clock.Now(), time.Minute)
			return tok
		},
		"agent not allowed for the business line": func(f *endpointFixture) string {
			f.e.Access = denyAll{}
			tok, _ := SignStep(testKeys, testClaims(), f.clock.Now(), time.Minute)
			return tok
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEndpoint(t)
			agents, err := agentdef.NewRegistry(
				&agentdef.Def{Name: "planner", Version: "1", Context: agentdef.Context{Scope: agentdef.ScopeRun},
					Loop: &agentdef.LoopRef{Server: "plan", Models: []string{"p/good"}}},
				&agentdef.Def{Name: "other-loop-agent", Version: "1", Context: agentdef.Context{Scope: agentdef.ScopeRun},
					Loop: &agentdef.LoopRef{Server: "other", Models: []string{"p/good"}}})
			if err != nil {
				t.Fatal(err)
			}
			f.e.Agents = agents
			resp := f.post(t, setup(f), chatBody)
			if resp.StatusCode != http.StatusBadRequest || len(f.calls) != 0 {
				t.Fatalf("status %d calls %d", resp.StatusCode, len(f.calls))
			}
		})
	}
	f := newEndpoint(t)
	body := strings.Replace(chatBody, `{"type":"image_url","image_url":{"url":"data:image/png;base64,AQI="}}`,
		`{"type":"input_audio","input_audio":{"data":"AQI=","format":"x-unknown"}}`, 1)
	if resp := f.post(t, f.token(t), body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown audio format: %d", resp.StatusCode)
	}
}

// 合计并发上限：名额被占满时，下一次调用排队后返回 503 busy，而不是挤进提供商的准入。
func TestEndpointConcurrencyCap(t *testing.T) {
	f := newEndpoint(t)
	f.e.Max = 1
	entered, unblock := make(chan struct{}), make(chan struct{})
	f.e.Model = providerFunc(func(context.Context, *model.Request, func(model.Delta)) (*model.Response, error) {
		close(entered)
		<-unblock
		return &model.Response{Content: model.TextBlocks("ok"), Usage: &v1.Usage{}}, nil
	})
	tok := f.token(t)
	done := make(chan int)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+EndpointPath, strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-entered
	resp := f.post(t, tok, chatBody)
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	close(unblock)
	if resp.StatusCode != http.StatusServiceUnavailable || e.Error.Code != "busy" {
		t.Fatalf("second call: %d %q", resp.StatusCode, e.Error.Code)
	}
	if got := <-done; got != http.StatusOK {
		t.Fatalf("first call: %d", got)
	}
}

// 调用期间 Session 被删除：输出不交给外部服务。
func TestEndpointDeletedDuringCall(t *testing.T) {
	f := newEndpoint(t)
	f.e.Model = providerFunc(func(ctx context.Context, _ *model.Request, _ func(model.Delta)) (*model.Response, error) {
		if _, err := f.del.Mark(ctx, &lifecycle.Tombstone{SessionID: "s", BusinessLine: "bl", Reason: "end_user", RequestedAt: f.clock.Now()}); err != nil {
			t.Error(err)
		}
		return &model.Response{Content: model.TextBlocks("秘密"), Usage: &v1.Usage{}}, nil
	})
	resp := f.post(t, f.token(t), chatBody)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(b), "秘密") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}
