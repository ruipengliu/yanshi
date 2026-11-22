package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

func keyedCreateService(t *testing.T) (*service.Service, *clock.Fake, service.CreateRequest) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"},
		&agentdef.Def{Name: "a", Version: "2", Model: "echo/any"},
		&agentdef.Def{Name: "b", Version: "1", Model: "echo/any"},
	)
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{
		Store:  &session.Store{Log: memlog.New(), Clock: clk, IDs: ids.Sequential("id")},
		Agents: agents, Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions(),
		SessionKeySecret: []byte("yanshi-test-session-key-secret-v1"),
	}
	return svc, clk, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", IdempotencyKey: "intent-1"}
}

func TestKeyedCreateReplay(t *testing.T) {
	ctx := context.Background()
	svc, clk, req := keyedCreateService(t)
	req.AgentVersion = "2"
	req.DeleteAt = clk.Now().Add(time.Hour)
	req.TimeZone = "UTC"
	id, replay, err := svc.CreateWithReplay(ctx, req)
	if err != nil || replay || id == "" {
		t.Fatalf("first create = %q, %v, %v", id, replay, err)
	}
	rel, err := svc.Agents.ParseReleases([]byte(`a: {stable: "1", withdrawn: ["2"]}`))
	if err != nil {
		t.Fatal(err)
	}
	svc.Agents.SetReleases(rel)
	if got, replay, err := svc.CreateWithReplay(ctx, req); err != nil || !replay || got != id {
		t.Fatalf("withdrawn version replay = %q, %v, %v", got, replay, err)
	}
	implicit := req
	implicit.AgentVersion = ""
	implicit.DeleteAt = req.DeleteAt.Add(999 * time.Millisecond)
	if got, replay, err := svc.CreateWithReplay(ctx, implicit); err != nil || !replay || got != id {
		t.Fatalf("replay without explicit version, same deadline second = %q, %v, %v", got, replay, err)
	}
	for name, change := range map[string]func(*service.CreateRequest){
		"agent":     func(r *service.CreateRequest) { r.Agent = "b" },
		"version":   func(r *service.CreateRequest) { r.AgentVersion = "1" },
		"deadline":  func(r *service.CreateRequest) { r.DeleteAt = r.DeleteAt.Add(time.Second) },
		"unset":     func(r *service.CreateRequest) { r.DeleteAt = time.Time{} },
		"time zone": func(r *service.CreateRequest) { r.TimeZone = "" },
	} {
		t.Run(name, func(t *testing.T) {
			other := req
			change(&other)
			if _, err := svc.Create(ctx, other); !errors.Is(err, service.ErrConflict) {
				t.Fatalf("changed params: %v", err)
			}
		})
	}
	events, err := eventlog.ReadAll(ctx, svc.Store.Log, id, 0)
	if err != nil || len(events) != 1 || events[0].GetSessionCreated() == nil {
		t.Fatalf("replays changed the Event log: %v, %v", events, err)
	}
	clk.Advance(2 * time.Hour)
	if got, err := svc.Create(ctx, req); err != nil || got != id {
		t.Fatalf("replay before cleanup with an elapsed deadline = %q, %v", got, err)
	}
	expired := req
	expired.IdempotencyKey = "new-key"
	if _, err := svc.Create(ctx, expired); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("new Session with elapsed deadline = %v", err)
	}
	if err := svc.Delete(ctx, id, "retention", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, req); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("replay after deletion = %v", err)
	}
}

func TestKeyedCreateScopeAndValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, req := keyedCreateService(t)
	otherLine, otherUser := req, req
	otherLine.BusinessLine, otherUser.EndUser = "other", "other"
	seen := map[string]bool{}
	for _, r := range []service.CreateRequest{req, otherLine, otherUser} {
		id, err := svc.Create(ctx, r)
		if err != nil || id == "" || seen[id] {
			t.Fatalf("scope %s/%s = %q, %v", r.BusinessLine, r.EndUser, id, err)
		}
		seen[id] = true
	}
	valid := req
	valid.IdempotencyKey = strings.Repeat("AZaz09._:-", 12) + "aaaaaaaa"
	if _, err := svc.Create(ctx, valid); err != nil {
		t.Fatalf("128 allowed bytes: %v", err)
	}
	for _, key := range []string{"space here", "line\nbreak", "键", "/", strings.Repeat("a", 129)} {
		invalid := req
		invalid.IdempotencyKey = key
		if _, err := svc.Create(ctx, invalid); !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("key %q = %v", key, err)
		}
	}
	specified := req
	specified.SessionID, _ = ids.SessionID("specified", req.EndUser)
	if _, err := svc.Create(ctx, specified); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("explicit ID with key = %v", err)
	}
	svc.SessionKeySecret = nil
	if _, err := svc.Create(ctx, req); !errors.Is(err, service.ErrInvalid) || !strings.Contains(err.Error(), "idempotency keys are not configured") {
		t.Fatalf("unconfigured keys = %v", err)
	}
	req.IdempotencyKey = ""
	if _, err := svc.Create(ctx, req); err != nil {
		t.Fatalf("unkeyed create = %v", err)
	}
}

func TestKeyedCreatePartialIndex(t *testing.T) {
	ctx := context.Background()
	svc, clk, req := keyedCreateService(t)
	req.DeleteAt = clk.Now().Add(time.Hour)
	id, err := ids.KeyedSessionID(svc.SessionKeySecret, req.BusinessLine, req.EndUser, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Index.Put(ctx, &lifecycle.Session{ID: id, BusinessLine: req.BusinessLine, EndUser: req.EndUser,
		Agent: req.Agent, CreatedAt: clk.Now(), LastInputAt: clk.Now(), DeleteAt: req.DeleteAt}); err != nil {
		t.Fatal(err)
	}
	otherAgent, otherDeadline := req, req
	otherAgent.Agent = "b"
	otherDeadline.DeleteAt = req.DeleteAt.Add(time.Second)
	for _, r := range []service.CreateRequest{otherAgent, otherDeadline} {
		if _, err := svc.Create(ctx, r); !errors.Is(err, service.ErrConflict) {
			t.Fatalf("changed half-written create = %v", err)
		}
	}
	if events, err := eventlog.ReadAll(ctx, svc.Store.Log, id, 0); err != nil || len(events) != 0 {
		t.Fatalf("conflicting request wrote events: %v, %v", events, err)
	}
	if got, replay, err := svc.CreateWithReplay(ctx, req); err != nil || replay || got != id {
		t.Fatalf("resume half-written create = %q, %v, %v", got, replay, err)
	}
}

func TestKeyedCreateChecksStoredScope(t *testing.T) {
	ctx := context.Background()
	for _, where := range []string{"event", "index business line", "index EndUser"} {
		t.Run(where, func(t *testing.T) {
			svc, clk, req := keyedCreateService(t)
			id, err := ids.KeyedSessionID(svc.SessionKeySecret, req.BusinessLine, req.EndUser, req.IdempotencyKey)
			if err != nil {
				t.Fatal(err)
			}
			switch where {
			case "event":
				if _, err := svc.Create(ctx, service.CreateRequest{SessionID: id, BusinessLine: "other",
					EndUser: req.EndUser, Agent: req.Agent, AgentVersion: "1"}); err != nil {
					t.Fatal(err)
				}
			default:
				row := &lifecycle.Session{ID: id, BusinessLine: req.BusinessLine, EndUser: req.EndUser,
					Agent: req.Agent, CreatedAt: clk.Now(), LastInputAt: clk.Now()}
				if where == "index business line" {
					row.BusinessLine = "other"
				} else {
					row.EndUser = "other"
				}
				if err := svc.Index.Put(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.Create(ctx, req); !errors.Is(err, service.ErrConflict) {
				t.Fatalf("keyed create with foreign %s = %v", where, err)
			}
		})
	}
}

func TestKeyedCreateReplayWithdrawsForEndUserBarrier(t *testing.T) {
	ctx := context.Background()
	svc, clk, req := keyedCreateService(t)
	id, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// 只写屏障，模拟注销没有把首次创建列入清理的交错。
	barrier, err := svc.Deletions.MarkEndUser(ctx, &lifecycle.EndUserTombstone{
		BusinessLine: req.BusinessLine, EndUser: req.EndUser, RequestID: "del-test", RequestedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Create(ctx, req); got != "" || !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("replay during account deletion = %q, %v", got, err)
	}
	tombstone, err := svc.Deletions.Get(ctx, id)
	if err != nil {
		t.Fatalf("missing Session tombstone: %v", err)
	}
	if tombstone.RequestID != barrier.RequestID || tombstone.Reason != "account_deletion" {
		t.Fatalf("Session tombstone = %+v, want account deletion %s", tombstone, barrier.RequestID)
	}
	barrier, err = svc.Deletions.GetEndUser(ctx, req.BusinessLine, req.EndUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(barrier.Sessions) != 1 || barrier.Sessions[0] != id {
		t.Fatalf("EndUser Sessions = %v, want [%s]", barrier.Sessions, id)
	}
}

type hookedCreateLog struct {
	eventlog.Log
	before, after func()
}

func (l *hookedCreateLog) Append(ctx context.Context, id string, expected uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	if run := l.before; run != nil {
		l.before = nil
		run()
	}
	seq, err := l.Log.Append(ctx, id, expected, envelopes, events...)
	if err == nil && l.after != nil {
		l.after()
	}
	return seq, err
}

func TestConcurrentKeyedCreate(t *testing.T) {
	for _, different := range []bool{false, true} {
		name := "same parameters"
		if different {
			name = "different parameters"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			svc, _, req := keyedCreateService(t)
			other := req
			if different {
				other.Agent = "b"
			}
			log := &hookedCreateLog{Log: svc.Store.Log}
			svc.Store.Log = log
			var otherID string
			// 第二次创建也读到空日志，但第一次已写索引。相同参数时让第二次先提交。
			log.before = func() {
				var err error
				otherID, err = svc.Create(ctx, other)
				if different && !errors.Is(err, service.ErrConflict) || !different && err != nil {
					t.Fatalf("in-flight create = %q, %v", otherID, err)
				}
			}
			id, replay, err := svc.CreateWithReplay(ctx, req)
			if err != nil || id == "" || replay == different || (!different && id != otherID) {
				t.Fatalf("first request = %q, %v, %v; second ID = %q", id, replay, err, otherID)
			}
			events, err := eventlog.ReadAll(ctx, log, id, 0)
			if err != nil || len(events) != 1 || events[0].GetSessionCreated().GetAgent().GetName() != req.Agent {
				t.Fatalf("Event log = %v, %v", events, err)
			}
		})
	}
}

type createBarrierReads struct {
	lifecycle.Deletions
	err error
}

func (d createBarrierReads) GetEndUser(ctx context.Context, businessLine, endUser string) (*lifecycle.EndUserTombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.Deletions.GetEndUser(ctx, businessLine, endUser)
}

func TestKeyedCreateBarrierReadFailure(t *testing.T) {
	ctx := context.Background()
	svc, _, req := keyedCreateService(t)
	id, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	deletions, readErr := svc.Deletions, errors.New("cannot read EndUser deletion barrier")
	svc.Deletions = createBarrierReads{Deletions: deletions, err: readErr}
	if _, err := svc.Create(ctx, req); !errors.Is(err, readErr) || errors.Is(err, service.ErrNotFound) {
		t.Fatalf("replay during barrier failure = %v", err)
	}
	if _, err := deletions.Get(ctx, id); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("replay wrote a tombstone: %v", err)
	}
	next := req
	next.IdempotencyKey = "another-intent"
	if _, err := svc.Create(ctx, next); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("first create during barrier failure = %v", err)
	}
	svc.Deletions = deletions
	if _, err := svc.Create(ctx, next); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("retry of withdrawn create = %v", err)
	}
	if got, err := svc.Create(ctx, req); err != nil || got != id {
		t.Fatalf("retry of existing Session = %q, %v", got, err)
	}
}

func TestKeyedCreateDisconnectAfterAppend(t *testing.T) {
	svc, _, req := keyedCreateService(t)
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	svc.Store.Log = &hookedCreateLog{Log: svc.Store.Log, after: disconnect}
	svc.Deletions = createBarrierReads{Deletions: svc.Deletions}
	_, _ = svc.Create(ctx, req) // 连接已断，首次创建结果无法送达。
	id, replay, err := svc.CreateWithReplay(context.Background(), req)
	if err != nil || !replay || id == "" {
		t.Fatalf("retry after disconnection = %q, %v, %v", id, replay, err)
	}
}

// ambiguousCreateLog 写入成功后仍报告失败：提交结果未知，st.Created 无法据此确认本次写入。
type ambiguousCreateLog struct {
	eventlog.Log
	err error
}

func (l ambiguousCreateLog) Append(ctx context.Context, id string, expected uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	if _, err := l.Log.Append(ctx, id, expected, envelopes, events...); err != nil {
		return 0, err
	}
	return 0, l.err
}

func TestKeyedCreateAmbiguousAppendWithdrawsOnBarrierFailure(t *testing.T) {
	ctx := context.Background()
	svc, _, req := keyedCreateService(t)
	appendErr, readErr := errors.New("append outcome unknown"), errors.New("cannot read EndUser deletion barrier")
	svc.Store.Log = ambiguousCreateLog{Log: svc.Store.Log, err: appendErr}
	deletions := svc.Deletions
	svc.Deletions = createBarrierReads{Deletions: deletions, err: readErr}
	if _, err := svc.Create(ctx, req); err == nil {
		t.Fatal("ambiguous append during barrier failure succeeded")
	}
	id, err := ids.KeyedSessionID(svc.SessionKeySecret, req.BusinessLine, req.EndUser, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deletions.Get(ctx, id); err != nil {
		t.Fatalf("ambiguous keyed create was not withdrawn: %v", err)
	}
	svc.Deletions = deletions
	if _, err := svc.Create(ctx, req); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("retry of withdrawn create = %v", err)
	}
}
