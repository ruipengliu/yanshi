package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"xinli/task"
)

func creationTests(t *testing.T, factory Factory) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s, reopen := factory(t)
	source := task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "ensure", ID: "create", Version: 1}
	spec := task.Spec{Operation: "ensure", Target: "day/2026-10-28"}
	if err := s.Receive(ctx, source, []task.Spec{spec}, now); err != nil {
		t.Fatal(err)
	}
	old, claimed, err := s.Claim(ctx, task.Identity(source, spec), now, time.Minute)
	if err != nil || !claimed {
		t.Fatal(err, claimed)
	}
	intent := task.Creation{Owner: old.Owner, ID: task.CreationID(old), TaskID: old.TaskID, Target: old.Target, Agent: "xinli-day", Generation: old.LeaseGeneration, CreatedAt: now, LeaseUntil: old.LeaseUntil, DeleteAt: now.Add(24 * time.Hour)}
	if err := s.BeginCreation(ctx, old, intent, now); err != nil {
		t.Fatal(err)
	}
	s = reopen()
	list, err := s.Creations(ctx, now, 10)
	if err != nil || len(list) != 1 || list[0].Session != "" || !list[0].DeleteAt.Equal(intent.DeleteAt) {
		t.Fatal(list, err)
	}
	next, claimed, err := s.Claim(ctx, old.TaskID, now.Add(2*time.Minute), time.Minute)
	if err != nil || !claimed {
		t.Fatal(err, claimed)
	}
	intent.CreatedAt = now.Add(2 * time.Minute)
	if err := s.BeginCreation(ctx, old, intent, intent.CreatedAt); !errors.Is(err, task.ErrLeaseLost) {
		t.Fatal("old creation accepted", err)
	}
	// 迟到回包仍可保存 ID，但不能授予旧执行者绑定资格。
	if err := s.RecordCreated(ctx, intent.ID, "orphan"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCreated(ctx, intent.ID, "orphan"); err != nil {
		t.Fatal("same session rejected on replay", err)
	}
	if err := s.RecordCreated(ctx, intent.ID, "different"); !errors.Is(err, task.ErrInvalid) {
		t.Fatal(err)
	}
	if next.LeaseGeneration != old.LeaseGeneration+1 {
		t.Fatal("generation")
	}
	if err := s.DeferCreation(ctx, intent.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Creations(ctx, now.Add(time.Minute), 10); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if err := s.ForgetCreation(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Creations(ctx, now.Add(2*time.Hour), 10); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
}
