package pgnode_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
)

func TestNodeInboxMigrationPreservesOwners(t *testing.T) {
	for _, mode := range []string{"owned", "missing_owner", "conflicting_owner"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			pool, notifier := pgtest.Fresh(t)
			// 恢复旧主键与列后执行真实新增迁移；不改写历史迁移。
			if _, err := pool.Exec(ctx, `
				ALTER TABLE nodes DROP CONSTRAINT nodes_pkey;
				ALTER TABLE nodes ADD PRIMARY KEY (node_id);
				ALTER TABLE inbox DROP CONSTRAINT inbox_pkey;
				ALTER TABLE inbox DROP COLUMN end_user;
				ALTER TABLE inbox ADD PRIMARY KEY (node_id, call_id);
				ALTER TABLE inbox_versions DROP CONSTRAINT inbox_versions_pkey;
				ALTER TABLE inbox_versions DROP COLUMN end_user;
				ALTER TABLE inbox_versions ADD PRIMARY KEY (node_id);
				DELETE FROM schema_migrations WHERE version BETWEEN 34 AND 38`); err != nil {
				t.Fatal(err)
			}
			clk := clock.NewFake(time.Unix(1000, 0))
			index := pglifecycle.Index{Pool: pool}
			if err := index.Put(ctx, &lifecycle.Session{ID: "s", BusinessLine: "bl", EndUser: "u", Agent: "a", CreatedAt: clk.Now(), LastInputAt: clk.Now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO nodes (node_id, business_line, end_user, label, kind, host_app, capabilities, online, last_seen, gen)
				VALUES ('n', 'bl', 'u', 'pc', 'desktop', '', ''::bytea, true, $1, 7)`, clk.Now()); err != nil {
				t.Fatal(err)
			}
			if mode == "conflicting_owner" {
				if _, err := pool.Exec(ctx, `UPDATE nodes SET end_user = 'other' WHERE node_id = 'n'`); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"n", "sbx_s"} {
				data, err := proto.Marshal(&v1.Invoke{SessionId: "s", CallId: "c", Capability: "read_file"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO inbox (node_id, call_id, data, ord, session_id) VALUES ($1, 'c', $2, nextval('inbox_seq'), 's')`, id, data); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO inbox_versions (node_id, version) VALUES ($1, 9)`, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `INSERT INTO inbox_versions (node_id, version) VALUES ('old-empty-node', 42)`); err != nil {
				t.Fatal(err)
			}
			if mode == "missing_owner" {
				if err := index.Delete(ctx, "u", "s"); err != nil {
					t.Fatal(err)
				}
			}
			err := pg.Migrate(ctx, pool)
			if mode != "owned" {
				if err == nil {
					t.Fatal("migration guessed an owner")
				}
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox`).Scan(&count); err != nil || count != 2 {
					t.Fatalf("legacy calls lost: %d, %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := pg.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			in := pgnode.NewInbox(pool, notifier)
			for _, id := range []string{"n", "sbx_s"} {
				items, version, err := in.Pending(ctx, "u", id)
				if err != nil || len(items) != 1 || items[0].GetSessionId() != "s" || version != 9 {
					t.Fatalf("migrated %s: %v, version %d, %v", id, items, version, err)
				}
			}
			dir := pgnode.NewDirectory(pool, clk)
			if _, gen, err := dir.Register(ctx, node.Info{NodeID: "n", Scope: node.Scope{BusinessLine: "bl", EndUser: "u"}, Label: "pc"}); err != nil || gen != 8 {
				t.Fatalf("old generation lost: %d, %v", gen, err)
			}
			if _, gen, err := dir.Register(ctx, node.Info{NodeID: "n", Scope: node.Scope{BusinessLine: "bl", EndUser: "other"}, Label: "pc"}); err != nil || gen != 1 {
				t.Fatalf("same node ID for another end user: %d, %v", gen, err)
			}
		})
	}
}
