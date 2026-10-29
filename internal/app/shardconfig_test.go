package app_test

import (
	"strings"
	"testing"

	"yanshi/internal/app"
)

const newCellConfig = `{
 "table":{"shards":[0,7],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":7}]},
 "compatibility":{"version":"new-cell-v1","legacy_data_in_shard0":false,"entries":[]},
 "databases":[{"id":0,"sql_dsn":"postgres://host/db0","direct_dsn":"postgres://host/db0"},{"id":7,"sql_dsn":"postgres://host/db7","direct_dsn":"postgres://host/db7"}]
}`

func TestShardDeploymentConfig(t *testing.T) {
	c, l, err := app.ParseShardConfig([]byte(newCellConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Databases) != 2 || l.Shards()[1] != 7 {
		t.Fatal("stable shard IDs treated as array indexes")
	}
	_, digest := l.Identity()
	for _, change := range []struct{ name, from, to string }{
		{"legacy fallback", `"legacy_data_in_shard0":false`, `"legacy_data_in_shard0":true`},
		{"same database", `"id":7,"sql_dsn":"postgres://host/db7","direct_dsn":"postgres://host/db7"`, `"id":7,"sql_dsn":"postgres://host/db0","direct_dsn":"postgres://host/db0"`},
		{"direct missing", `"direct_dsn":"postgres://host/db7"`, `"direct_dsn":""`},
		{"unknown shard", `"id":7`, `"id":8`},
		{"duplicate shard", `"id":7`, `"id":0`},
		{"missing id", `"id":7,`, ``},
		{"unknown field", `"databases":`, `"other":0,"databases":`},
		{"duplicate field", `"id":7`, `"id":7,"id":7`},
		{"legacy moved", `"entries":[]`, `"entries":[{"kind":"session_data","key":"old","shard":7}]`},
	} {
		t.Run(change.name, func(t *testing.T) {
			if _, _, err := app.ParseShardConfig([]byte(strings.Replace(newCellConfig, change.from, change.to, 1))); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, _, err := app.ParseShardConfig([]byte(newCellConfig + ` {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	_, updated, err := app.ParseShardConfig([]byte(strings.ReplaceAll(newCellConfig, "postgres://host/", "postgres://another-host/")))
	if err != nil {
		t.Fatal(err)
	}
	_, otherDigest := updated.Identity()
	if digest != otherDigest {
		t.Fatal("DSNs leaked into route identity")
	}
	legacy := strings.Replace(newCellConfig, `"first":512,"last":1023,"shard":7`, `"first":512,"last":1023,"shard":0`, 1)
	legacy = strings.Replace(legacy, `"legacy_data_in_shard0":false`, `"legacy_data_in_shard0":true`, 1)
	if _, _, err := app.ParseShardConfig([]byte(legacy)); err != nil {
		t.Fatalf("existing cell in shard 0: %v", err)
	}
}
