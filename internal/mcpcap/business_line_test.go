package mcpcap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/capability"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
)

func TestSameNameServersAcrossBusinessLines(t *testing.T) {
	dir := t.TempDir()
	register := func(line, tool string, attested bool) {
		srv := mcp.NewServer(&mcp.Implementation{Name: "shared", Version: "1"}, nil)
		srv.AddTool(&mcp.Tool{Name: tool, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				_, pageOptions := req.Params.Meta["yanshi/page_options"]
				cites, hasCitations := req.Params.Meta["yanshi/captures"]
				if pageOptions != attested || hasCitations != attested || req.Params.Meta["yanshi/business_line"] != line {
					return nil, fmt.Errorf("incorrect MCP metadata for %s", line)
				}
				if attested {
					raw, err := json.Marshal(cites)
					if err != nil || !strings.Contains(string(raw), "other-1") {
						return nil, fmt.Errorf("missing citation for %s: %v", line, err)
					}
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: line}},
					Meta:    mcp.Meta{"yanshi/page": map[string]string{"kind": "empty"}},
				}, nil
			})
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+line+"-token" {
				http.Error(w, "wrong business line credential", http.StatusUnauthorized)
				return
			}
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(hs.Close)
		config := fmt.Sprintf("name: shared\nbusiness_line: %s\nurl: %s\nheaders: {Authorization: 'Bearer %s-token'}\nattests_speakers: %t\ncite_captures: %t\n",
			line, hs.URL, line, attested, attested)
		if err := writeFile(filepath.Join(dir, line+".yaml"), config); err != nil {
			t.Fatal(err)
		}
	}
	register("bl", "attested", true)
	register("other", "ordinary", false)
	servers, err := mcpcap.LoadDir(dir)
	if err != nil || len(servers) != 2 {
		t.Fatalf("load same name across lines: %v", err)
	}
	if err := writeFile(filepath.Join(dir, "duplicate.yaml"), "name: shared\nbusiness_line: bl\nurl: http://example.invalid\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpcap.LoadDir(dir); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("duplicate registration in the same business line: %v", err)
	}

	connector := &mcpcap.Connector{Servers: servers, Log: citationNamespaceLog(t)}
	defer connector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, line := range []string{"bl", "other", "bl", "other"} {
		attested := line == "bl"
		wantName := "mcp_shared__ordinary"
		if attested {
			wantName = "mcp_shared__attested"
		}
		tools, err := connector.Tools(ctx, line, []string{"shared/*"})
		if err != nil || len(tools) != 1 || tools[0].Spec.Name != wantName {
			t.Fatalf("%s tools = %+v, %v", line, tools, err)
		}
		res, err := tools[0].Local.(capability.ResultInvoker).InvokeResult(ctx, capability.Invocation{
			BusinessLine: line, EndUser: "u", SessionID: "ses_v1_000_000_s", Arguments: `{"ref":"#2.0"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if attested {
			if len(res.Page) == 0 || len(res.Content) != 0 {
				t.Fatalf("%s attested result = %+v", line, res)
			}
		} else if len(res.Page) != 0 || model.Text(res.Content) != line {
			t.Fatalf("%s ordinary result = %+v", line, res)
		}
	}
}
