package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

func writeShardConfig(t *testing.T, first, second string) string {
	t.Helper()
	cfg := app.ShardConfig{
		Table:         shard.Config{Shards: []int{0, 1}, Ranges: []shard.Range{{First: 0, Last: 511, Shard: 0}, {First: 512, Last: 1023, Shard: 1}}},
		Compatibility: shard.CompatibilityConfig{Version: "test-new-cell"},
		Databases:     []app.ShardDatabase{{ID: 0, SQLDSN: first, DirectDSN: first}, {ID: 1, SQLDSN: second, DirectDSN: second}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shards.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenShardsMigrationsAndIdentity(t *testing.T) {
	first, second := pgtest.Database(t), pgtest.Database(t)
	path := writeShardConfig(t, first, second)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	if _, close, err := openShards(ctx, path, 1, false, clock.Real{}, logger); err == nil {
		close()
		t.Fatal("unmigrated shards accepted")
	}
	if err := migrateShards(ctx, path); err != nil {
		t.Fatal(err)
	}
	st, close, err := openShards(ctx, path, 1, false, clock.Real{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if len(st.shardConnections) != 2 || st.routingDigest == "" {
		t.Fatal("missing shard connections or identity")
	}
	if err := (&readiness{st: st}).check(ctx); err != nil {
		t.Fatal(err)
	}
	// 连接字符串不同也必须拒绝同一个 database。
	alias := first + " application_name=same-database-alias"
	if strings.HasPrefix(first, "postgres://") || strings.HasPrefix(first, "postgresql://") {
		u, err := url.Parse(first)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("application_name", "same-database-alias")
		u.RawQuery = q.Encode()
		alias = u.String()
	}
	path = writeShardConfig(t, first, alias)
	if _, close, err := openShards(ctx, path, 1, false, clock.Real{}, logger); err == nil {
		close()
		t.Fatal("same physical database assembled twice")
	}
}

// 逐角色的可服务规则不把两片简单 AND；控制库断开也不关闭无依赖的健康流量。
func TestPartialShardReadiness(t *testing.T) {
	first, second := pgtest.Database(t), pgtest.Database(t)
	path := writeShardConfig(t, first, second)
	ctx := context.Background()
	if err := migrateShards(ctx, path); err != nil {
		t.Fatal(err)
	}
	st, closeAll, err := openShards(ctx, path, 1, false, clock.Real{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	st.shardConnections[0].pool.Close()
	for _, role := range []string{"gateway", "worker", "sandbox", "janitor"} {
		r := &readiness{st: st, roles: map[string]bool{role: true}}
		rows, ready := r.shardReport(ctx)
		if !ready || len(rows) != 2 || rows[0].SQL || !rows[1].SQL {
			t.Fatalf("role %s lost healthy shard: %+v", role, rows)
		}
	}
}

func TestShardIdentityRejectsSwappedDSNs(t *testing.T) {
	first, second := pgtest.Database(t), pgtest.Database(t)
	ctx := context.Background()
	if err := migrateShards(ctx, writeShardConfig(t, first, second)); err != nil {
		t.Fatal(err)
	}
	if _, closeAll, err := openShards(ctx, writeShardConfig(t, second, first), 1, false, clock.Real{}, slog.New(slog.DiscardHandler)); err == nil {
		closeAll()
		t.Fatal("swapped shard identities accepted")
	}
	if err := migrateShards(ctx, writeShardConfig(t, second, first)); err == nil {
		t.Fatal("migration rebound an existing identity")
	}
}

func TestShardColdStartWithUnavailablePeer(t *testing.T) {
	first, second := pgtest.Database(t), pgtest.Database(t)
	ctx := context.Background()
	if err := migrateShards(ctx, writeShardConfig(t, first, second)); err != nil {
		t.Fatal(err)
	}
	path := writeShardConfig(t, first, "postgres://unused@127.0.0.1:1/unavailable?sslmode=disable&connect_timeout=1")
	st, closeAll, err := openShards(ctx, path, 1, false, clock.Real{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	rows, ready := (&readiness{st: st, roles: map[string]bool{"gateway": true}}).shardReport(ctx)
	if !ready || len(rows) != 2 || rows[1].SQL {
		t.Fatalf("cold start lost healthy shard: %+v", rows)
	}
}

func TestShardPoolMetricsReassembly(t *testing.T) {
	path := writeShardConfig(t, pgtest.Database(t), pgtest.Database(t))
	ctx := context.Background()
	if err := migrateShards(ctx, path); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		func() {
			st, closeAll, err := openShards(ctx, path, 1, false, clock.Real{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll()
			var want int32
			for _, part := range st.shardConnections {
				want += part.pool.Stat().MaxConns()
			}
			families, err := metrics.Registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, family := range families {
				if family.GetName() != "yanshi_db_pool_max_conns" {
					continue
				}
				found = true
				if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 || family.Metric[0].GetGauge().GetValue() != float64(want) {
					t.Fatalf("shard pools not aggregated once: %v", family)
				}
			}
			if !found {
				t.Fatal("missing shard pool metrics")
			}
		}()
	}
}
