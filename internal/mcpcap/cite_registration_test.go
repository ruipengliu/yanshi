package mcpcap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// ReadAll 使用 limit=0；同时记录范围，防止普通引用退化为分页扫描。
type citationReadLog struct {
	eventlog.Log
	after  []uint64
	limits []int
}

func (l *citationReadLog) Read(ctx context.Context, sid string, after uint64, limit int) ([]*v1.Event, error) {
	l.after = append(l.after, after)
	l.limits = append(l.limits, limit)
	return l.Log.Read(ctx, sid, after, limit)
}

func TestCitationNamespaceFromRegistration(t *testing.T) {
	ctx := context.Background()
	log := citationNamespaceLog(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "cites", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "save", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			raw, err := json.Marshal(req.Params.Meta["yanshi/captures"])
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
		})
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer hs.Close()
	for _, tc := range []struct {
		name     string
		declared bool
		line     string
		state    *session.State
		attested bool
		latest   bool
		missing  bool
	}{
		{name: "ordinary with attested history", line: "bl"},
		{name: "cross business line", declared: true, line: "other"},
		{name: "attested latest", declared: true, line: "bl", state: &session.State{Seq: 4}, attested: true, latest: true},
		{name: "attested prefix", declared: true, line: "bl", state: &session.State{Seq: 3}, attested: true},
		{name: "attested before first page", declared: true, line: "bl", state: &session.State{Seq: 2}, attested: true, missing: true},
		{name: "attested nil state", declared: true, line: "bl", attested: true, latest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := &citationReadLog{Log: log}
			conn := &mcpcap.Connector{Log: reads, Servers: []mcpcap.Server{{Name: "cites", BusinessLine: "bl", URL: hs.URL, Timeout: time.Second, CiteCaptures: true, AttestsSpeakers: tc.declared}}}
			defer conn.Close()
			tools, err := conn.Tools(ctx, "bl", []string{"cites/*"})
			if err != nil || len(tools) != 1 {
				t.Fatalf("tools: %d, %v", len(tools), err)
			}
			res, err := tools[0].Local.(capability.ResultInvoker).InvokeResult(ctx, capability.Invocation{
				BusinessLine: tc.line, SessionID: "s", State: tc.state,
				Arguments: `{"refs":["#2.0","#2.1","#999.0","#2.0"]}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got []mcpcap.Citation
			if err := json.Unmarshal([]byte(model.Text(res.Content)), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 || got[0].Missing != tc.missing || got[1].Missing != tc.attested || !got[2].Missing {
				t.Fatalf("citations = %+v", got)
			}
			if !tc.missing {
				want := "wearer"
				if tc.latest {
					want = "other"
				}
				if got[0].Speaker != want || got[0].Redacted != tc.latest || (tc.latest && got[0].SpeakerKey != "other-1") {
					t.Fatalf("wrong namespace or prefix: %+v", got[0])
				}
			}
			if tc.attested {
				if len(reads.limits) != 1 || reads.limits[0] != 0 || reads.after[0] != 0 {
					t.Fatalf("attested reads: %v, %v", reads.after, reads.limits)
				}
			} else {
				if len(reads.limits) != 3 {
					t.Fatalf("ordinary reads: %v", reads.limits)
				}
				for i, after := range []uint64{1, 1, 998} {
					if reads.limits[i] != 1 || reads.after[i] != after {
						t.Fatalf("ordinary path scanned the log: %v, %v", reads.after, reads.limits)
					}
				}
			}
		})
	}
}
