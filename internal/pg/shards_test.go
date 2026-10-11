package pg

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var (
	createTableRE = regexp.MustCompile(`(?is)CREATE TABLE (\w+)\s*\((.*?)\);`)
	addColumnRE   = regexp.MustCompile(`(?i)ALTER TABLE (\w+) ADD COLUMN (\w+)`)
	likeRE        = regexp.MustCompile(`(?i)^\s*LIKE (\w+)`)
)

// tableColumns 从迁移中解析出每张表的列名（含 ALTER TABLE ... ADD COLUMN 与 LIKE 复制的列）。
func tableColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range files {
		b, err := migrations.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
		all.WriteByte('\n')
	}
	sql := all.String()
	cols := map[string]map[string]bool{}
	for _, m := range createTableRE.FindAllStringSubmatch(sql, -1) {
		name, body := strings.ToLower(m[1]), m[2]
		cols[name] = map[string]bool{}
		if l := likeRE.FindStringSubmatch(body); l != nil {
			for c := range cols[strings.ToLower(l[1])] {
				cols[name][c] = true
			}
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if f := strings.Fields(strings.TrimSpace(line)); len(f) > 0 {
				cols[name][strings.ToLower(f[0])] = true
			}
		}
	}
	for _, m := range addColumnRE.FindAllStringSubmatch(sql, -1) {
		cols[strings.ToLower(m[1])][strings.ToLower(m[2])] = true
	}
	return cols
}

// TestEveryTableHasShardKey：迁移中的每张表都在 Tables 中登记了分片键（ADR-0028），且键所在的列存在。
func TestEveryTableHasShardKey(t *testing.T) {
	cols := tableColumns(t)
	if len(cols) == 0 {
		t.Fatal("no tables parsed from migrations")
	}
	for name, c := range cols {
		k, ok := Tables[name]
		if !ok {
			t.Errorf("table %s has no shard key: register it in pg.Tables (ADR-0028)", name)
			continue
		}
		switch k.Class {
		case BySession, ByEndUser:
			if !c[k.Column] {
				t.Errorf("table %s: shard key column %q not found", name, k.Column)
			}
		case Global:
			if k.Column != "" {
				t.Errorf("table %s: a global table has no shard key column", name)
			}
		default:
			t.Errorf("table %s: unknown key class %q", name, k.Class)
		}
	}
	for name := range Tables {
		if cols[name] == nil {
			t.Errorf("pg.Tables lists %s, which no migration creates", name)
		}
	}
}
