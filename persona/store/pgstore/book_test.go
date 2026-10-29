package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"persona/domain"
	"persona/pg/pgtest"
)

// 纯账本转换无需数据库；非空、空及 nil 集合都必须保留领域快照语义。
func TestBookParts(t *testing.T) {
	for _, populated := range []bool{false, true} {
		book := domain.NewBook()
		if populated {
			book.Seed(domain.SeedData{
				Projects:    []domain.Project{{ID: "P-1", NextSteps: []string{"下一步"}}},
				Days:        map[string]string{"2026-10-27": "摘要"},
				Commitments: []domain.SeedCommitment{{ID: "C-1", Content: "承诺", Revisions: []domain.SeedRevision{{Fields: map[string]string{"due_date": "2026-10-30"}}}}},
				Filed:       []domain.Day{{Date: "2026-10-27", Headline: "总览"}},
			}, time.Time{})
			if err := book.BindDay("2026-10-27", "day"); err != nil {
				t.Fatal(err)
			}
			book.BindWeek("2026-10-20", "week")
			book.Submitted("day", "整理今天")
		}
		before, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := splitBook(book)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := joinBook(parts)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(restored)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := snapshotRows(book, nil)
		if err != nil {
			t.Fatal(err)
		}
		fromRows, err := bookFromRows(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		rowJSON, err := json.Marshal(fromRows)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(rowJSON) || !reflect.DeepEqual(book.State(), fromRows.State()) {
			t.Fatal("entity rows changed book facts or order")
		}

		if string(before) != string(after) || !reflect.DeepEqual(book.State(), restored.State()) {
			t.Fatal("book parts round trip changed facts or projection")
		}
	}
}

// 比较所有类别的每一行，项目绑定只允许一个实体的 xmin 改变。
func TestOnlyChangedEntityWritten(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool, captureClock)
	ctx := context.Background()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1"}, {ID: "P-2"}}}, time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	versions := func() map[string]map[string]string {
		t.Helper()
		out := map[string]map[string]string{}
		for _, c := range categories {
			key := "''::bytea"
			if c.kind != singleton {
				key = c.key
			}
			rows, err := pool.Query(ctx, "SELECT "+key+",xmin::text FROM "+c.table+" WHERE business_line=$1 AND end_user=$2", []byte("bl"), []byte("u"))
			if err != nil {
				t.Fatal(err)
			}
			out[c.table] = map[string]string{}
			for rows.Next() {
				var key []byte
				var version string
				if err := rows.Scan(&key, &version); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				out[c.table][string(key)] = version
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := versions()
	if err := s.Update(ctx, "bl", "u", func(*domain.Book) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, versions()) {
		t.Fatal("no-op update rewrote ledger")
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { b.Bind("P-1", "project"); return nil }); err != nil {
		t.Fatal(err)
	}
	after := versions()
	changed := 0
	for table, old := range before {
		if len(old) != len(after[table]) {
			t.Fatalf("unexpected entity count in %s", table)
		}
		for key, version := range old {
			if version != after[table][key] {
				changed++
				if table != "projects" || key != entityKey(entityKey("P-1"), "0") {
					t.Fatalf("unexpected rewrite in %s", table)
				}
			}
		}
	}
	if changed != 1 {
		t.Fatalf("updated %d rows, want 1", changed)
	}
}

func TestOwnerBinaryKeys(t *testing.T) {
	s := New(pgtest.New(t), captureClock)
	ctx := context.Background()
	owners := [][2]string{{"bl\x00u", ""}, {"bl", "u\x00"}, {"", "bl\x00u"}}
	for i, owner := range owners {
		if err := s.Update(ctx, owner[0], owner[1], func(b *domain.Book) error {
			b.Seed(domain.SeedData{Projects: []domain.Project{{ID: fmt.Sprintf("P-%d", i)}}}, time.Time{})
			return nil
		}); err != nil {
			t.Fatalf("owner %d: %v", i, err)
		}
	}
	for _, owner := range owners {
		if err := s.View(ctx, owner[0], owner[1], func(b *domain.Book) error {
			if len(b.State().Projects) != 1 {
				return errors.New("owner keys collided")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
