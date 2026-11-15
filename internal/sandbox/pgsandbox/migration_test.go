package pgsandbox_test

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/sdk/nodesdk"
)

func TestLedgerMigrationPreservesStartedAndRequiresSession(t *testing.T) {
	for _, missingSession := range []bool{false, true} {
		name := "started"
		if missingSession {
			name = "missing_session"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := pgtest.Fresh(t)
			// 在隔离 schema 恢复 C1 的账本约束，放入在途调用后应用真实新增迁移。
			_, err := pool.Exec(ctx, `
				ALTER TABLE sandbox_ledger DROP CONSTRAINT sandbox_ledger_pkey;
				ALTER TABLE sandbox_ledger ALTER COLUMN session_id DROP NOT NULL;
				ALTER TABLE sandbox_ledger ADD PRIMARY KEY (call_id);
				DELETE FROM schema_migrations WHERE version IN (32, 33)`)
			if err != nil {
				t.Fatal(err)
			}
			var sid any = "ses_v1_000_000_old"
			if missingSession {
				sid = nil
			}
			if _, err := pool.Exec(ctx, `INSERT INTO sandbox_ledger (call_id, state, session_id, updated_at) VALUES ('c', $1, $2, $3)`, int16(nodesdk.StateStarted), sid, time.Now()); err != nil {
				t.Fatal(err)
			}
			err = pg.Migrate(ctx, pool)
			if missingSession {
				if err == nil {
					t.Fatal("migration guessed a missing Session")
				}
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_ledger WHERE call_id = 'c' AND session_id IS NULL`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("legacy row lost: %d, %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := pg.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			l := pgsandbox.Ledger{Pool: pool}
			r, err := l.Get("ses_v1_000_000_old", "c")
			if err != nil || r.State != nodesdk.StateStarted {
				t.Fatalf("lost in-flight identity: %+v, %v", r, err)
			}
			if err := l.Put("ses_v1_000_000_other", "c", &nodesdk.Record{SessionID: "ses_v1_000_000_other", State: nodesdk.StateDone}); err != nil {
				t.Fatal(err)
			}
			r, err = l.Get("ses_v1_000_000_old", "c")
			if err != nil || r.State != nodesdk.StateStarted {
				t.Fatalf("old call overwritten: %+v, %v", r, err)
			}
		})
	}
}
