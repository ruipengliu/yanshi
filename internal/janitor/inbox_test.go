package janitor_test

import (
	"context"
	"slices"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
)

type recordingInbox struct {
	node.Inbox
	users []string
}

func (in *recordingInbox) RemoveSession(ctx context.Context, endUser, sid string) error {
	in.users = append(in.users, endUser)
	return in.Inbox.RemoveSession(ctx, endUser, sid)
}

func TestInboxDeletionWithAndWithoutSessionIndex(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, mode := range []string{"indexed", "index_deleted", "log_deleted", "log_and_index_deleted"} {
			name := backend + "/" + mode
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				clk := clock.NewFake(time.Unix(1000, 0))
				stores := app.Memory(clk)
				if backend == "postgres" {
					pool, notifier := pgtest.Fresh(t)
					stores = app.Postgres(pool, notifier, clk)
				}
				in := &recordingInbox{Inbox: stores.Inbox}
				stores.Inbox = in
				agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
				if err != nil {
					t.Fatal(err)
				}
				core := app.NewCore(stores, app.Options{Clock: clk, IDs: ids.Sequential, Agents: agents, Model: model.NewGateway(), Blobs: artifact.NewMemBlobs()})
				sid, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
				if err != nil {
					t.Fatal(err)
				}
				if err := in.Put(ctx, "u", "same", &v1.Invoke{SessionId: sid, CallId: "c"}); err != nil {
					t.Fatal(err)
				}
				if err := in.Put(ctx, "other", "same", &v1.Invoke{SessionId: "ses_v1_000_000_keep", CallId: "c"}); err != nil {
					t.Fatal(err)
				}
				if err := core.Service.Delete(ctx, sid, "end_user", ""); err != nil {
					t.Fatal(err)
				}
				if mode != "indexed" {
					// 索引丢失时先用投影；两者都已删的旧状态才走离线恢复。
					if mode == "log_deleted" || mode == "log_and_index_deleted" {
						if err := stores.Log.Delete(ctx, sid); err != nil {
							t.Fatal(err)
						}
					}
					if mode != "log_deleted" {
						if err := stores.Index.Delete(ctx, "u", sid); err != nil {
							t.Fatal(err)
						}
					}
				}
				j := core.Janitor("j", sandbox.NewFake(), nil, 0)
				j.BeforeStage = func(ctx context.Context, stage string) {
					if stage == "sweep" {
						// 第一遍清扫后到达的投递仍须由第二遍撤回。
						if err := in.Put(ctx, "u", "same", &v1.Invoke{SessionId: sid, CallId: "late"}); err != nil {
							t.Fatal(err)
						}
					}
				}
				drain(t, j)
				want := []string{"u", "u"}
				if mode == "log_and_index_deleted" {
					want = []string{"", ""}
				}
				if !slices.Equal(in.users, want) {
					t.Fatalf("sweep users = %v, want %v", in.users, want)
				}
				if items, _, err := in.Pending(ctx, "u", "same"); err != nil || len(items) != 0 {
					t.Fatalf("deleted session inbox: %v, %v", items, err)
				}
				if items, _, err := in.Pending(ctx, "other", "same"); err != nil || len(items) != 1 || items[0].GetSessionId() != "ses_v1_000_000_keep" {
					t.Fatalf("other end user inbox: %v, %v", items, err)
				}
				if tomb, err := stores.Deletions.Get(ctx, sid); err != nil || tomb.CompletedAt.IsZero() {
					t.Fatalf("deletion incomplete: %+v, %v", tomb, err)
				}
			})
		}
	}
}
