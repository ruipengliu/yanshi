package main

import (
	"context"
	"testing"
	"time"

	"xinli/pg/pgtest"
	"xinli/task"
)

func TestTaskDatabaseMustBeIndependent(t *testing.T) {
	t.Setenv("YANSHI_PG_DSN", "postgres://localhost/kernel")
	for _, dsn := range []string{"", "postgres://localhost/persona", "postgres://localhost/kernel"} {
		if _, _, err := openTasks(context.Background(), "pg", dsn, "postgres://localhost/persona"); err == nil {
			t.Fatal("accepted shared or missing application database")
		}
	}
}

func TestTaskDatabaseReopens(t *testing.T) {
	ctx := context.Background()
	t.Setenv("YANSHI_PG_DSN", "postgres://localhost/kernel")
	_, dsn := pgtest.NewWithDSN(t)
	s, closeStore, err := openTasks(ctx, "pg", dsn, "postgres://localhost/persona")
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	source := task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "day", ID: "day/2026-10-28", Version: 1}
	spec := task.Spec{Operation: "collect", Target: "project/p", Date: "2026-10-28"}
	if err := s.Receive(ctx, source, []task.Spec{spec}, time.Now()); err != nil {
		t.Fatal(err)
	}
	closeStore()
	next, closeNext, err := openTasks(ctx, "pg", dsn, "postgres://localhost/persona")
	if err != nil {
		t.Fatal(err)
	}
	defer closeNext()
	if _, err := next.Get(ctx, task.Identity(source, spec)); err != nil {
		t.Fatal("application task lost on restart", err)
	}
}
