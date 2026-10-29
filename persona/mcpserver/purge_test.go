package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

func TestOwnerBarrierAllTools(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			var s store.Store = memstore.New(captureClock)
			if backend == "pg" {
				s = pgstore.New(pgtest.New(t), captureClock)
			}
			ctx := context.Background()
			notified := 0
			x := newRuntime(s, Options{Now: captureClock, Notifier: notifierFunc(func(context.Context, string, string, string, string, []string) { notified++ })})
			if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureClock()); err != nil {
				t.Fatal(err)
			}
			meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day",
				"yanshi/page_options": map[string]int{"limit": 200, "max_chars": 6000, "gap_ms": 300000, "max_tool_result": 8000}}
			for range 2 {
				for _, tool := range tools {
					readOnly := tool.tool.Annotations != nil && tool.tool.Annotations.ReadOnlyHint
					result, err := x.call(ctx, meta, []byte(`{}`), tool.handler, readOnly)
					if result != nil || !errors.Is(err, store.ErrOwnerDeleting) {
						t.Fatal("tool bypassed barrier", tool.tool.Name, err)
					}
				}
				result, err := x.captures(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "captures", Meta: meta, Arguments: []byte(`{}`)}})
				if result != nil || !errors.Is(err, store.ErrOwnerDeleting) {
					t.Fatal("capture page bypassed barrier", err)
				}
				if _, err := s.Purge(ctx, "bl", "u", 100, captureClock()); err != nil {
					t.Fatal(err)
				}
			}
			if notified != 0 {
				t.Fatal("blocked tool notified application")
			}
		})
	}
}
