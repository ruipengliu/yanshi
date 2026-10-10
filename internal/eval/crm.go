package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/artifact"
	"yanshi/internal/mcpcap"
)

// crmServer 是评测内置的 CRM MCP Server（docs/design/m5-mcp.md）：search_customer 只读，
// create_ticket 有副作用（未声明只读，需要审批）。工单按调用方的 EndUser 记录，供断言（expect.tickets）。
type crmServer struct {
	connector *mcpcap.Connector
	mu        sync.Mutex
	tickets   map[string][]string // EndUser → 工单内容
	next      int
}

// crmCustomers 是 CRM 中的客户；评测用例的正确答案依赖这些数据。
var crmCustomers = []struct{ ID, Name, Contact, Phone, Level string }{
	{"C-1001", "青禾超市", "李娜", "021-5550-1234", "VIP"},
	{"C-1002", "北辰物流", "赵强", "010-6620-8899", "普通"},
	{"C-1003", "王记餐饮", "王海", "0571-8877-3210", "VIP"},
	{"C-1004", "青禾农场", "刘洋", "0512-6688-0001", "普通"},
}

func startCRM(ctx context.Context, arts *artifact.Service, logger *slog.Logger) (*crmServer, error) {
	c := &crmServer{tickets: map[string][]string{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "crm", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "search_customer", Description: "按名称关键词查询客户，返回客户编号、联系人、电话与等级。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"客户名称关键词"}},"required":["q"]}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct{ Q string }
			_ = json.Unmarshal(req.Params.Arguments, &in)
			var b strings.Builder
			for _, x := range crmCustomers {
				if in.Q != "" && strings.Contains(x.Name, in.Q) {
					fmt.Fprintf(&b, "%s %s 联系人：%s 电话：%s 等级：%s\n", x.ID, x.Name, x.Contact, x.Phone, x.Level)
				}
			}
			if b.Len() == 0 {
				b.WriteString("未找到匹配的客户")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "create_ticket", Description: "为客户创建服务工单。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"customer_id":{"type":"string"},"summary":{"type":"string"}},"required":["customer_id","summary"]}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct {
				CustomerID string `json:"customer_id"`
				Summary    string
			}
			_ = json.Unmarshal(req.Params.Arguments, &in)
			eu, _ := req.Params.Meta["yanshi/end_user"].(string)
			c.mu.Lock()
			c.next++
			id := fmt.Sprintf("T-%04d", c.next)
			c.tickets[eu] = append(c.tickets[eu], fmt.Sprintf("%s %s %s", id, in.CustomerID, in.Summary))
			c.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "已创建工单 " + id}}}, nil
		})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	go func() { <-ctx.Done(); _ = hs.Close() }()
	c.connector = &mcpcap.Connector{Servers: []mcpcap.Server{{Name: "crm", BusinessLine: BusinessLine, URL: "http://" + ln.Addr().String(), Timeout: 30 * time.Second}},
		Artifacts: arts, Logger: logger}
	return c, nil
}

func (c *crmServer) ticketsOf(endUser string) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.tickets[endUser]...)
}
