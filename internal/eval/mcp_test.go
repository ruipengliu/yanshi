package eval

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"yanshi/internal/capability"
	"yanshi/internal/eval/xinlifixture"
)

// 走真实 dev 子进程、就绪解析与运行器同一装配；单改直连分页测试无法覆盖凭据丢失或连接串线。
func TestXinliFixtureMCPHeadersAcrossBusinessLines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, servers, err := xinlifixture.Start(ctx, defaultXinliBin, BusinessLine, NoHealthBusinessLine)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	if len(servers) != 2 {
		t.Fatal("expected two business line registrations")
	}
	first := servers[0].Headers["Authorization"]
	second := servers[1].Headers["Authorization"]
	if !strings.HasPrefix(first, "Bearer ") || !strings.HasPrefix(second, "Bearer ") || first == second {
		t.Fatal("readiness lost distinct business line credentials")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, servers[0].URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("dev MCP accepted an unauthenticated request")
	}
	for i, line := range []string{BusinessLine, NoHealthBusinessLine} {
		if err := fixture.Seed(ctx, line, "u", xinlifixture.SeedData{Projects: []xinlifixture.Project{{ID: "P-1", Name: []string{"常规项目", "未开启健康信息的项目"}[i]}}}); err != nil {
			t.Fatal(err)
		}
	}
	connectors := newEvalMCP(servers, nil, nil, nil)
	defer connectors.Close()
	for _, line := range []string{BusinessLine, NoHealthBusinessLine, BusinessLine, NoHealthBusinessLine} {
		tools, err := connectors.Tools(ctx, line, []string{"xinli/list_projects"})
		if err != nil || len(tools) != 1 || tools[0].Spec.Name != "mcp_xinli__list_projects" {
			t.Fatalf("business line %s failed MCP discovery: %v", line, err)
		}
		blocks, err := tools[0].Local.Invoke(ctx, capability.Invocation{BusinessLine: line, EndUser: "u", SessionID: "test-session", CallID: "list-projects", Arguments: `{}`})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, block := range blocks {
			text.WriteString(block.GetText().GetText())
		}
		want, other := "常规项目", "未开启健康信息的项目"
		if line == NoHealthBusinessLine {
			want, other = other, want
		}
		if !strings.Contains(text.String(), want) || strings.Contains(text.String(), other) {
			t.Fatal("MCP credentials or results crossed business lines")
		}
	}
	if tools, err := connectors.Tools(ctx, "unknown", []string{"xinli/*"}); err != nil || len(tools) != 0 {
		t.Fatal("unknown business line inherited MCP registration")
	}
}
