package day

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(copy)
}

func (e *environment) lateFileChanges(sid string) {
	e.t.Helper()
	if sid == "" {
		e.t.Fatal("retired Session ID missing")
	}
	var day struct {
		Session  string
		Sessions []string
	}
	if !e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_line=$1 AND end_user=$2 AND business_day=$3`, &day, []byte(businessLine), []byte(e.endUser), []byte(businessDay)) || day.Session == sid || !slices.Contains(day.Sessions, sid) {
		e.t.Fatal("late request does not identify an inactive historical day Session")
	}
	var work struct {
		Value struct {
			Target, CommandID string
		}
	}
	if !e.jsonRow(e.personaPool, `SELECT payload FROM work_sessions WHERE business_line=$1 AND end_user=$2 AND session_id=$3`, &work, []byte(businessLine), []byte(e.endUser), []byte(sid)) || work.Value.Target != "day/"+businessDay || work.Value.CommandID == "" {
		e.t.Fatal("late request lost its registered work identity")
	}
	token := ""
	for _, value := range e.environ {
		if v, ok := strings.CutPrefix(value, "DAY_MCP_TOKEN="); ok {
			token = v
		}
	}
	if token == "" {
		e.t.Fatal("Persona service credential missing")
	}
	beforeDay, beforeCommitments := e.day(businessDay), e.commitments()
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "day-late-call", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.persona.server.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	must(e.t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "file_changes",
		Meta:      mcp.Meta{"yanshi/business_line": businessLine, "yanshi/end_user": e.endUser, "yanshi/session": sid},
		Arguments: map[string]any{"source": map[string]string{"from": "#2.0", "to": "#2.0"}}})
	must(e.t, err)
	if result == nil {
		e.t.Fatal("late file_changes returned no result")
	}
	var text strings.Builder
	for _, content := range result.Content {
		if content, ok := content.(*mcp.TextContent); ok {
			text.WriteString(content.Text)
		}
	}
	// 资格错误须有结构化标志与明确原因；参数错误、擦除墓碑或 busy 都不能充当旧绑定屏障。
	if !result.IsError || text.String() != "这个 Session 已不是当天的工作 Session" && text.String() != "这个 Session 已退役，不能读写" {
		e.t.Fatalf("late file_changes was not explicitly rejected: IsError=%t text=%q", result.IsError, text.String())
	}
	if !reflect.DeepEqual(e.day(businessDay), beforeDay) || !reflect.DeepEqual(e.commitments(), beforeCommitments) {
		e.t.Fatal("late file_changes changed day state or commitments")
	}
}
