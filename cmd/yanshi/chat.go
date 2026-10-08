package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

type client struct {
	base string
	mu   sync.Mutex
	// active 是最近一个未结束的 Run，用于 /stop。
	active string
	// approvals 是待审批的调用 ID，按到达顺序。
	approvals []string
}

func (c *client) post(path string, body any, out any) error {
	b, _ := json.Marshal(body)
	resp, err := http.Post(c.base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func chat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "yanshi 服务地址")
	agent := fs.String("agent", "assistant", "AgentDef 名称")
	user := fs.String("user", "dev", "终端用户 ID")
	bl := fs.String("business-line", "dev", "业务线")
	_ = fs.Parse(args)

	c := &client{base: strings.TrimRight(*server, "/")}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := c.post("/v1/sessions", map[string]string{"business_line": *bl, "end_user": *user, "agent": *agent}, &created); err != nil {
		return err
	}
	sid := created.SessionID
	fmt.Printf("session %s · 直接输入即可对话；运行中输入会作为插话；/stop 中断；/approve、/deny 审批；Ctrl-D 退出\n", sid)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.follow(ctx, sid)

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		switch {
		case line == "":
		case line == "/stop":
			c.mu.Lock()
			run := c.active
			c.mu.Unlock()
			if run == "" {
				fmt.Println("[没有进行中的 Run]")
				continue
			}
			if err := c.post("/v1/sessions/"+sid+"/runs/"+run+"/interrupt", struct{}{}, nil); err != nil {
				fmt.Println("[中断失败]", err)
			}
		case line == "/approve" || line == "/deny":
			c.mu.Lock()
			var call string
			if len(c.approvals) > 0 {
				call, c.approvals = c.approvals[0], c.approvals[1:]
			}
			c.mu.Unlock()
			if call == "" {
				fmt.Println("[没有待审批的调用]")
				continue
			}
			body := map[string]any{"approve": line == "/approve", "by": "chat"}
			if err := c.post("/v1/sessions/"+sid+"/approvals/"+call, body, nil); err != nil {
				fmt.Println("[审批失败]", err)
			}
		default:
			var res struct {
				RunID   string `json:"run_id"`
				Steered bool   `json:"steered"`
			}
			if err := c.post("/v1/sessions/"+sid+"/inputs", map[string]string{"text": line}, &res); err != nil {
				fmt.Println("[发送失败]", err)
				continue
			}
			c.mu.Lock()
			c.active = res.RunID
			c.mu.Unlock()
		}
	}
	return in.Err()
}

// follow 订阅 SSE：打印增量，并以已提交事件为准标注工具调用与 Run 结束。
func (c *client) follow(ctx context.Context, sid string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/sessions/"+sid+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("[订阅失败]", err)
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var kind string
	for sc.Scan() {
		line := sc.Text()
		if k, ok := strings.CutPrefix(line, "event: "); ok {
			kind = k
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		switch kind {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(data), &d)
			fmt.Print(d.Text)
		case "event":
			e := &v1.Event{}
			if protojson.Unmarshal([]byte(data), e) == nil {
				c.render(e)
			}
		}
	}
}

func (c *client) render(e *v1.Event) {
	switch p := e.GetPayload().(type) {
	case *v1.Event_AttemptStarted:
		if p.AttemptStarted.GetAttempt() > 1 {
			fmt.Printf("\n[第 %d 次尝试]\n", p.AttemptStarted.GetAttempt())
		}
	case *v1.Event_AssistantMessage:
		if len(p.AssistantMessage.GetContent()) > 0 {
			fmt.Println()
		}
		for _, tc := range p.AssistantMessage.GetToolCalls() {
			fmt.Printf("[调用 %s %s]\n", tc.GetCapability(), tc.GetArgumentsJson())
		}
	case *v1.Event_ToolResult:
		out := model.Text(p.ToolResult.GetContent())
		if len(out) > 300 {
			out = out[:300] + "…"
		}
		fmt.Printf("[结果 %s]\n", out)
	case *v1.Event_ApprovalRequested:
		c.mu.Lock()
		c.approvals = append(c.approvals, p.ApprovalRequested.GetCallId())
		c.mu.Unlock()
		fmt.Printf("[需要审批] %s —— 输入 /approve 或 /deny\n", p.ApprovalRequested.GetSummary())
	case *v1.Event_ApprovalDecided:
		c.mu.Lock()
		for i, id := range c.approvals {
			if id == p.ApprovalDecided.GetCallId() {
				c.approvals = append(c.approvals[:i], c.approvals[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	case *v1.Event_RunSuspended:
		fmt.Println("[等待中：审批或设备结果]")
	case *v1.Event_RunCompleted, *v1.Event_RunFailed, *v1.Event_RunInterrupted:
		c.mu.Lock()
		c.active = ""
		c.mu.Unlock()
		switch p := p.(type) {
		case *v1.Event_RunFailed:
			fmt.Printf("[失败] %s\n", p.RunFailed.GetReason())
		case *v1.Event_RunInterrupted:
			fmt.Println("\n[已中断]")
		}
	}
}
