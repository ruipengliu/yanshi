package httpapi_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/artifact"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

// 使用两个隔离 schema 模拟选库 seam；即使裸 ID 确实读到另一库的行，也不能把槽位或映射当授权。
func TestArtifactLocationStillChecksRealOwnership(t *testing.T) {
	pool0, _ := pgtest.Fresh(t)
	pool1, _ := pgtest.Fresh(t)
	for _, id := range []string{"art_v1_001_forged", "art_legacy_mapped"} {
		t.Run(id, func(t *testing.T) {
			e := newAuthzEnv(t)
			ctx := context.Background()
			table, err := shard.NewTable(shard.Config{Shards: []int{0, 1}, Ranges: []shard.Range{{First: 0, Last: 1023, Shard: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			compat, err := shard.NewCompatibility(table, shard.CompatibilityConfig{Version: "migrated", Entries: []shard.CompatibilityEntry{{Target: shard.Target{Kind: shard.Object, Key: "art_legacy_mapped"}, Shard: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			loc, err := shard.NewLocator(table, compat)
			if err != nil {
				t.Fatal(err)
			}
			meta := pgartifact.Meta{Pool: pool1}
			if err := meta.Create(ctx, &artifact.Meta{ID: id, SessionID: e.sid, MimeType: "text/plain", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			var selected atomic.Int32
			e.api.Artifacts.Meta = pgartifact.Meta{Select: func(target shard.Target) (*pgxpool.Pool, error) {
				n, err := loc.Locate(target)
				if err != nil {
					return nil, err
				}
				selected.Store(int32(n))
				if n == 1 {
					return pool1, nil
				}
				return pool0, nil
			}}
			for _, suffix := range []string{"", "/meta"} {
				status, _ := e.do(t, "GET", "/v1/artifacts/"+id+suffix, e.token(t, "demo/u2", time.Hour), nil)
				if status != http.StatusNotFound || selected.Load() != 1 {
					t.Fatalf("foreign artifact: HTTP %d, shard %d", status, selected.Load())
				}
			}
			status, _ := e.do(t, "GET", "/v1/artifacts/"+id+"/meta", e.token(t, "demo/u1", time.Hour), nil)
			if status != http.StatusOK {
				t.Fatalf("real owner: HTTP %d", status)
			}
			// 唯一目标未命中时，即使另一个库有同 ID，也不逐库寻找。
			missing := "art_v1_001_only_in_zero"
			if err := (pgartifact.Meta{Pool: pool0}).Create(ctx, &artifact.Meta{ID: missing, SessionID: e.sid, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			status, _ = e.do(t, "GET", "/v1/artifacts/"+missing+"/meta", e.token(t, "demo/u1", time.Hour), nil)
			if status != http.StatusNotFound {
				t.Fatalf("searched another shard: HTTP %d", status)
			}
			if err := (pgartifact.Meta{Pool: pool0}).Delete(ctx, e.sid, missing); err != nil {
				t.Fatal(err)
			}
		})
	}
}
