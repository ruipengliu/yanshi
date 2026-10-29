package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

var workWriteTools = []struct {
	name string
	args string
}{
	{"file_changes", `{"source":{"from":"#1.0","to":"#1.0"}}`},
	{"file_day", `{"day":{"headline":"迟到总览"}}`},
	{"set_status", `{"status":"迟到现状","next_steps":[],"open_questions":[]}`},
	{"update_commitments", `{"date":"2026-10-27"}`},
	{"propose_merge", `{"into":"p","from":"q","reason":"迟到合并"}`},
	{"file_week", `{"habits":[],"suggestions":[]}`},
}

type workNotifier struct{ calls atomic.Int64 }

func (n *workNotifier) DayFiled(context.Context, string, string, string, string, []string) {
	n.calls.Add(1)
}
func (n *workNotifier) WeekFiled(context.Context, string, string) { n.calls.Add(1) }

func TestProductionWorkErasureRejection(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			now := captureClock()
			var books store.Store = memstore.New(captureClock)
			if backend == "pg" {
				books = pgstore.New(pgtest.New(t), captureClock)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			update := func(fn func(*domain.Book) error) {
				t.Helper()
				if err := books.Update(ctx, "bl", "u", fn); err != nil {
					t.Fatal(err)
				}
			}
			lease := domain.WorkLease{Task: "t", Sequence: 1, Generation: 1, Until: now.Add(time.Minute)}
			const target = "day/2026-10-27"
			bind := func(sid, command string) {
				t.Helper()
				update(func(b *domain.Book) error {
					expected, err := b.ReserveWork(target, lease, now)
					if err != nil {
						return err
					}
					if err := b.BindWork(target, expected, lease, sid, now); err != nil {
						return err
					}
					return b.RegisterWorkCommand(target, sid, command, "day", lease, now)
				})
			}
			update(func(b *domain.Book) error {
				_, err := b.AppendCaptures("2026-10-27", "c", []domain.CaptureSegment{
					{Speaker: "wearer", Text: "保留原文", TimeZone: "UTC"},
					{Speaker: "wearer", Text: "擦除原文", TimeZone: "UTC"},
				})
				return err
			})
			bind("old", "one")
			notifier := &workNotifier{}
			hs := httptest.NewServer(Handler(books, Options{Now: captureClock, Notifier: notifier, Credentials: []Credential{
				{BusinessLine: "bl", Token: "work-test", ExpiresAt: now.Add(time.Hour)},
			}}))
			defer hs.Close()
			conn, err := mcp.NewClient(&mcp.Implementation{Name: "work-test", Version: "1"}, nil).Connect(ctx,
				&mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: credentialHTTPClient("work-test"), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			call := func(sid, tool, args string) *mcp.CallToolResult {
				t.Helper()
				result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(args),
					Meta: mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": sid}})
				if err != nil || result == nil {
					t.Fatalf("%s: result=%v error=%v", tool, result, err)
				}
				return result
			}
			accept := func(sid string) {
				t.Helper()
				before := notifier.calls.Load()
				result := call(sid, "file_day", `{"day":{"headline":"当天总览"}}`)
				if result.IsError || notifier.calls.Load() != before+1 {
					t.Fatal("current registered Session cannot file")
				}
			}
			// 同一凭据、HTTP 连接和参数先合法接受，排除鉴权或请求形状导致的假拒收。
			accept("old")
			var task domain.DeletionTask
			update(func(b *domain.Book) error {
				var err error
				task, err = b.Erase("2026-10-27", "#1.1", "#1.1", now)
				return err
			})
			snapshot := func() string {
				t.Helper()
				var raw []byte
				if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
					var err error
					raw, err = json.Marshal(b)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			reject := func(sid, reason string) {
				t.Helper()
				before, notified := snapshot(), notifier.calls.Load()
				for _, tool := range workWriteTools {
					result := call(sid, tool.name, tool.args)
					if !result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != reason {
						t.Fatalf("%s: work rejection missing: %+v", tool.name, result)
					}
					if snapshot() != before || notifier.calls.Load() != notified {
						t.Fatal("rejected work mutated state or notified")
					}
				}
			}
			if task.Session != "old" {
				t.Fatal("erasure did not capture the old binding")
			}
			reject("orphan", domain.ErrWorkCommand.Error())
			// 删除待办尚未完成也必须先报告绑定已停用，不能伪装为同一命令仍忙。
			reject("old", domain.ErrInactiveSession.Error())
			update(func(b *domain.Book) error { return b.CompleteDeletionTask(task.ID, nil, now) })
			reject("old", domain.ErrInactiveSession.Error())
			lease.Generation++
			bind("new", "two")
			reject("old", domain.ErrInactiveSession.Error())
			accept("new")
			// CAS 直接换绑会永久退役旧值；旧 HTTP 连接不能继承新绑定资格。
			lease.Generation++
			bind("replacement", "three")
			reject("new", domain.ErrWorkRetired.Error())
			accept("replacement")
			update(func(b *domain.Book) error { return b.FinishWorkCommand("replacement", "three") })
			reject("replacement", domain.ErrWorkCommand.Error())
			update(func(b *domain.Book) error {
				return b.RegisterWorkCommand(target, "replacement", "four", "day", lease, now)
			})
			accept("replacement")
		})
	}
}
