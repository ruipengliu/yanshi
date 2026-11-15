package service_test

import (
	"context"
	"errors"
	"testing"

	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/shard"
)

func TestCreateSessionIDs(t *testing.T) {
	ctx := context.Background()
	owned, _ := ids.SessionID("specified", "u")
	foreign, _ := ids.SessionID("specified", "other")
	for _, tc := range []struct {
		name, id string
		invalid  bool
	}{
		{name: "generated"},
		{name: "specified", id: owned},
		// 旧格式 ID 的兼容已在上线前删除（ADR-0051）。
		{name: "unversioned", id: "ses_legacy", invalid: true},
		{name: "foreign", id: foreign, invalid: true},
		{name: "version", id: "ses_v2_000_000_token", invalid: true},
		{name: "malformed", id: "ses_v1_000_token", invalid: true},
		{name: "object", id: "art_v1_000_token", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := ids.Sequential("id")
			consumed := 0
			store := &session.Store{Log: memlog.New(), Clock: clock.Real{}, IDs: func() string { consumed++; return gen() }}
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
			if err != nil {
				t.Fatal(err)
			}
			svc := &service.Service{Store: store, Agents: agents}
			sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", SessionID: tc.id})
			if tc.invalid {
				if !errors.Is(err, service.ErrInvalid) || consumed != 0 {
					t.Fatalf("invalid ID: %s, %v, consumed %d", sid, err, consumed)
				}
				events, err := eventlog.ReadAll(ctx, store.Log, tc.id, 0)
				if err != nil || len(events) != 0 {
					t.Fatalf("invalid ID wrote events: %v, %v", events, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1 // SessionCreated 的 Event ID。
			if tc.id == "" {
				wantCalls++
				want, _ := ids.SessionID("id-1", "u")
				if sid != want {
					t.Fatalf("generated %s, want %s", sid, want)
				}
			} else if sid != tc.id {
				t.Fatalf("specified ID changed: %s", sid)
			}
			if consumed != wantCalls {
				t.Fatalf("consumed %d IDs, want %d", consumed, wantCalls)
			}
			if slot, err := ids.EndUserSlotOfSession(sid); err != nil || slot != shard.Hash("u") {
				t.Fatalf("EndUser slot: %d, %v", slot, err)
			}
		})
	}
}
