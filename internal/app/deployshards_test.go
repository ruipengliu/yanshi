package app

import (
	"os"
	"testing"
)

// 示例部署（deploy/k8s/overlays/example）的分片配置符合上线约定：1024 槽均分到 8 片、每片 128 槽，
// 每片独立的 database 与地址（ADR-0051 §5.4）。
func TestExampleDeploymentShards(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/k8s/overlays/example/shards.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, loc, err := ParseShardConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Shards(); len(got) != 8 {
		t.Fatalf("shards = %v, want 8", got)
	}
	counts := map[int]int{}
	for _, r := range cfg.Table.Ranges {
		counts[r.Shard] += r.Last - r.First + 1
	}
	for id, n := range counts {
		if n != 128 {
			t.Errorf("shard %d has %d slots, want 128", id, n)
		}
	}
	seen := map[string]bool{}
	for _, db := range cfg.Databases {
		for _, dsn := range []string{db.SQLDSN, db.DirectDSN} {
			if seen[dsn] {
				t.Errorf("DSN reused across shards: shard %d", db.ID)
			}
			seen[dsn] = true
		}
	}
}
