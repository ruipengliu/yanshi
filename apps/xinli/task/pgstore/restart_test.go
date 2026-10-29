package pgstore_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/task"
	"xinli/task/pgstore"
)

func TestRestartProcess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	if dsn := os.Getenv("XINLI_TASK_CHILD_DSN"); dsn != "" {
		pool, err := pg.Open(ctx, dsn, "")
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		var database, schema string
		if err := pool.QueryRow(ctx, `SELECT current_database(),current_schema()`).Scan(&database, &schema); err != nil {
			t.Fatal(err)
		}
		if database != pgtest.Database || schema == "public" || schema == "" {
			t.Fatal("child database/schema not isolated")
		}
		s := pgstore.New(pool)
		v, ok, err := s.Claim(ctx, os.Getenv("XINLI_TASK_CHILD_ID"), now.Add(2*time.Minute), time.Minute)
		if err != nil || !ok || v.State != task.Unknown || v.LeaseGeneration != 2 {
			t.Fatal("child did not recover lease", ok, err)
		}
		// 子进程在对账前退出；第三个持有者仍必须看到 Unknown，而非误认尚未提交。
		return
	}
	pool, dsn := pgtest.NewWithDSN(t)
	s := pgstore.New(pool)
	source := task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "day", ID: "day/2026-10-28", Version: 1}
	spec := task.Spec{Operation: "collect", Target: "project/p"}
	if err := s.Receive(ctx, source, []task.Spec{spec}, now); err != nil {
		t.Fatal(err)
	}
	id := task.Identity(source, spec)
	old, ok, err := s.Claim(ctx, id, now, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	old.State = task.Unknown
	old.Session = "session"
	if err := s.Save(ctx, old, now, false); err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestRestartProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "XINLI_TASK_CHILD_DSN="+dsn, "XINLI_TASK_CHILD_ID="+id)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, output)
	}
	old.State = task.Succeeded
	if err := s.Save(ctx, old, now.Add(2*time.Minute), true); !errors.Is(err, task.ErrLeaseLost) {
		t.Fatal("parent late completion accepted", err)
	}
	v, ok, err := s.Claim(ctx, id, now.Add(4*time.Minute), time.Minute)
	if err != nil || !ok || v.State != task.Unknown || v.LeaseGeneration != 3 {
		t.Fatal("restart lost unknown state", ok, err)
	}
}
