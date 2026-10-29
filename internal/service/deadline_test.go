package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

func TestCreateDeleteAt(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Store: &session.Store{Log: memlog.New(), Clock: clk, IDs: ids.Sequential("s")}, Agents: agents, Index: lifecycle.NewMemIndex()}
	for _, at := range []time.Time{clk.Now(), clk.Now().Add(-time.Second)} {
		if _, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", DeleteAt: at}); !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("deadline %v: %v", at, err)
		}
	}
	for _, at := range []time.Time{{}, clk.Now().Add(time.Hour)} {
		sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", DeleteAt: at})
		if err != nil {
			t.Fatal(err)
		}
		st, err := svc.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		indexed, err := svc.Index.Get(ctx, "u", sid)
		if err != nil || !indexed.DeleteAt.Equal(at) {
			t.Fatalf("index: %+v, %v", indexed, err)
		}
		if at.IsZero() {
			if st.Created.GetDeleteAt() != nil {
				t.Fatal("unset deadline was persisted")
			}
		} else if !st.Created.GetDeleteAt().AsTime().Equal(at) {
			t.Fatal("event lost deadline")
		}
	}
}
