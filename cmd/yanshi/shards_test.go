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
