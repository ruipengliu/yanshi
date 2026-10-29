package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg/pgtest"
)

func TestOrdinalAllocation(t *testing.T) {
	rows := []bookRow{{ordinal: "1"}, {}, {ordinal: "2"}}
	if err := assignOrdinals(rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ordinal != "1" || rows[1].ordinal != "1.5" || rows[2].ordinal != "2" {
		t.Fatal("insertion renumbered anchors", rows)
	}
	rows = []bookRow{rows[0], rows[2]}
	if err := assignOrdinals(rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ordinal != "1" || rows[1].ordinal != "2" {
		t.Fatal("deletion compressed ordinals", rows)
	}
	rows = []bookRow{rows[1], rows[0]}
	if err := assignOrdinals(rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ordinal != "2" || rows[1].ordinal != "3" {
		t.Fatal("move rewrote retained anchor", rows)
	}
}

type entityVersion struct {
	Ordinal string
	Xmin    string
}

func projectVersions(t *testing.T, pool *pgxpool.Pool) map[string]entityVersion {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT payload,ordinal::text,xmin::text FROM projects WHERE business_line=$1 AND end_user=$2`, []byte("bl"), []byte("u"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]entityVersion{}
	for rows.Next() {
		var raw []byte
		var version entityVersion
		if err := rows.Scan(&raw, &version.Ordinal, &version.Xmin); err != nil {
			t.Fatal(err)
		}
		var project domain.Project
		if err := unmarshalPayload(raw, &project); err != nil {
			t.Fatal(err)
		}
		out[project.ID] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeleteEntityLeavesOtherRowsUntouched(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool, captureClock)
	ctx := context.Background()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1"}, {ID: "P-2"}, {ID: "P-3"}}}, time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := projectVersions(t, pool)
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		// 当前没有项目删除命令；用公开快照恢复构造实体消失，检验存储的 delete 差分。
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		var parts bookParts
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		parts["Projects"], err = json.Marshal([]domain.Project{{ID: "P-2"}, {ID: "P-3"}})
		if err != nil {
			return err
		}
		data, err = json.Marshal(parts)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, b)
	}); err != nil {
		t.Fatal(err)
	}
	after := projectVersions(t, pool)
	if len(after) != 2 || after["P-2"] != before["P-2"] || after["P-3"] != before["P-3"] {
		t.Fatal("delete rewrote or renumbered another entity", before, after)
	}
	if _, ok := after["P-1"]; ok {
		t.Fatal("deleted entity row survived")
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		projects := b.State().Projects
		if len(projects) != 2 || projects[0].ID != "P-2" || projects[1].ID != "P-3" {
			return errors.New("load changed retained entity order")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceReplacementUpdatesOriginalRow(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool, captureClock)
	ctx := context.Background()
	file := func(seq int) {
		t.Helper()
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			args := []byte(fmt.Sprintf(`{"source":{"from":"#%d.0","to":"#%d.0"}}`, seq, seq))
			call := domain.NewCall(b, "unbound", args, nil, time.Time{})
			call.FileChanges()
			return call.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
	file(1)
	file(2)
	versions := func() map[string]entityVersion {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT payload,ordinal::text,xmin::text FROM changesets WHERE business_line=$1 AND end_user=$2`, []byte("bl"), []byte("u"))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]entityVersion{}
		for rows.Next() {
			var raw []byte
			var version entityVersion
			if err := rows.Scan(&raw, &version.Ordinal, &version.Xmin); err != nil {
				t.Fatal(err)
			}
			var change domain.Changeset
			if err := unmarshalPayload(raw, &change); err != nil {
				t.Fatal(err)
			}
			out[change.ID] = version
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := versions()
	if _, err := pool.Exec(ctx, `CREATE TABLE write_audit (operation text NOT NULL);
		CREATE FUNCTION audit_changeset() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO write_audit VALUES (TG_OP); RETURN NULL; END; $$;
		CREATE TRIGGER audit_changeset AFTER INSERT OR UPDATE OR DELETE ON changesets
		FOR EACH ROW EXECUTE FUNCTION audit_changeset();`); err != nil {
		t.Fatal(err)
	}
	file(1)
	after := versions()
	if len(after) != 2 || after["CS-0003"].Ordinal != before["CS-0001"].Ordinal || after["CS-0002"] != before["CS-0002"] {
		t.Fatal("replacement changed another row or ordinal", before, after)
	}
	if _, ok := after["CS-0001"]; ok {
		t.Fatal("old changeset ID survived replacement")
	}
	var operations []string
	rows, err := pool.Query(ctx, `SELECT operation FROM write_audit`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		operations = append(operations, operation)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []string{"UPDATE"}) {
		t.Fatal("replacement must update exactly one existing row", operations)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		changes := b.State().Changesets
		if len(changes) != 2 || changes[0].ID != "CS-0003" || changes[1].ID != "CS-0002" {
			return errors.New("load reordered replacement")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingOwnersWithMultipleTasks(t *testing.T) {
	s := New(pgtest.New(t), captureClock)
	ctx := context.Background()
	var tasks []domain.DeletionTask
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		for i, date := range []string{"2026-10-26", "2026-10-27"} {
			if err := b.BindDay(date, "s-"+date); err != nil {
				return err
			}
			var task domain.DeletionTask
			var err error
			if i == 0 {
				task, err = b.EraseDay(date, time.Time{})
			} else {
				// 只有范围擦除可重建，先保存原文使擦除端点有效。
				if _, err := b.AppendCaptures(date, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "测试原文"}}); err != nil {
					return err
				}
				task, err = b.Erase(date, "1.0", "1.0", time.Time{})
			}
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		owners, err := s.PendingDeletions(ctx)
		if err != nil || len(owners) != want {
			t.Fatal("pending owners are not distinct", owners, err)
		}
	}
	check(1)
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := b.CompleteDeletionTask(tasks[0].ID, nil, time.Time{}); err != nil {
			return err
		}
		return b.CompleteDeletionWithRebuild(tasks[1].ID, nil, true, time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		return b.CompleteDeletionRebuild(tasks[1].ID)
	}); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func TestMapEntriesWrittenIndependently(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool, captureClock)
	ctx := context.Background()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		for _, session := range []string{"s1", "s2"} {
			b.Submitted(session, "整理交谈")
		}
		b.BindWeek("2026-10-20", "w1")
		b.BindWeek("2026-10-27", "w2")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read := func(table, key string) map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT "+key+",xmin::text FROM "+table+" WHERE business_line=$1 AND end_user=$2", []byte("bl"), []byte("u"))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key []byte
			var version string
			if err := rows.Scan(&key, &version); err != nil {
				t.Fatal(err)
			}
			out[string(key)] = version
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	submitted := read("submitted", "session_id")
	weeks := read("week_sessions", "week_start")
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Submitted("s1", "整理今天")
		b.BindWeek("2026-10-20", "new-week")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newSubmitted := read("submitted", "session_id")
	newWeeks := read("week_sessions", "week_start")
	if len(newSubmitted) != 2 || len(newWeeks) != 2 || submitted["s1"] == newSubmitted["s1"] || submitted["s2"] != newSubmitted["s2"] || weeks["2026-10-20"] == newWeeks["2026-10-20"] || weeks["2026-10-27"] != newWeeks["2026-10-27"] {
		t.Fatal("map update rewrote another entry")
	}
}

func TestLoadUsesNumericOrdinal(t *testing.T) {
	s := New(pgtest.New(t), captureClock)
	ctx := context.Background()
	projects := make([]domain.Project, 12)
	for i := range projects {
		projects[i].ID = fmt.Sprintf("P-%02d", i)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: projects}, time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		got := b.State().Projects
		if len(got) != len(projects) {
			return errors.New("load lost entities")
		}
		for i := range got {
			if got[i].ID != projects[i].ID {
				return errors.New("ordinal sorted as text")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
