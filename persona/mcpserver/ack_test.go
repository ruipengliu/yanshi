package mcpserver

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 处理进度由业务线事务推进，任何 file_changes 结果都不再向内核发送交付回执。
func TestFileChangesWithoutAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := newServer(nil)
	hs := httptest.NewServer(Handler(x.store, Options{InsecureDev: true, AllowUnmanaged: true, Now: x.now}))
	defer hs.Close()
	if XinliRegistration().Acks {
		t.Fatal("xinli registration still declares acks")
	}
	x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-9000", Kind: "mine", Content: "发报价"}}})
	if err := x.Edit("bl", "u", "C-9000", "done", ""); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, tc := range []struct {
		name     string
		args     M
		accepted bool
	}{
		{"accepted", changes(span("#2.0", "#2.1"), []M{exchange("#2.0", "#2.1", "报价", "约好发报价", M{"match": "none"})}, M{}), true},
		{"rejected", changes(span("#2.0", "#2.1"), []M{exchange("#2.0", "#2.1", "失眠", "失眠", M{"match": "none"})}, M{}), false},
		{"empty", changes(span("#2.0", "#2.1"), nil, M{}), true},
		{"day", changes(M{"day": true}, nil, M{}), false},
		{"user priority", changes(span("#2.0", "#2.1"), []M{exchange("#2.0", "#2.1", "报价", "约好发报价", M{"match": "none"})},
			M{"close": []M{{"id": "C-9000", "rev": 2, "status": "cancelled", "refs": []string{"#2.0"}}}}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "file_changes", Arguments: tc.args,
				Meta: mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": tc.name,
					"yanshi/captures": []any{said("#2.0", "wearer", "09:00"), said("#2.1", "wearer", "09:01")}}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "user priority" {
				text := res.Content[0].(*mcp.TextContent).Text
				if strings.Contains(text, "没有被接受") || !strings.Contains(text, "以用户的为准") {
					t.Fatalf("user priority result = %s", text)
				}
			}
			if _, ok := res.Meta["yanshi/ack"]; ok {
				t.Fatal("retired ack returned")
			}
			if tc.accepted {
				if res.IsError || strings.Contains(res.Content[0].(*mcp.TextContent).Text, "没有被接受") {
					t.Fatal("accepted changeset reported as rejected")
				}
			}
		})
	}
}
