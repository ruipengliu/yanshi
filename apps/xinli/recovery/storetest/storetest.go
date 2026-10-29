// Package storetest 用同一组故障轨迹验证内存和 PG 的恢复协议。
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"xinli/account"
	"xinli/kernel"
	"xinli/recovery"
	"xinli/task"
)

type Store interface {
	recovery.Consumer
	task.Store
	account.Store
	account.Enumerator
	Recorded(context.Context, string) ([]kernel.Envelope, error)
}

func Run(t *testing.T, factory func(*testing.T) (Store, func() Store)) {
	t.Run("FailedBatchDoesNotAdvanceAndScopesAreIsolated", func(t *testing.T) {
		s, _ := factory(t)
		ctx := context.Background()
		p := recovery.Progress{BusinessLine: "bl", Version: 1, Phase: recovery.Catchup}
		e := kernel.Envelope{EnvelopeID: "one", BusinessLine: "bl", EndUser: "u", SessionID: "a", Seq: 2, Kind: "question"}
		invalid := e
		invalid.Kind = "unknown"
		if err := s.Commit(ctx, 0, p, recovery.Batch{Envelopes: []kernel.Envelope{e, invalid}}); err == nil {
			t.Fatal("invalid batch committed")
		}
		loaded, err := s.Load(ctx, "bl")
		if err != nil || loaded.Version != 0 {
			t.Fatalf("failed batch advanced: %+v %v", loaded, err)
		}
		if err := s.Commit(ctx, 0, p, recovery.Batch{Envelopes: []kernel.Envelope{e}}); err != nil {
			t.Fatal(err)
		}
		collision := e
		collision.SessionID = "b"
		p.Version = 2
		if err := s.Commit(ctx, 1, p, recovery.Batch{Envelopes: []kernel.Envelope{collision}}); err == nil {
			t.Fatal("envelope ID reused for another event")
		}
		records, err := s.Recorded(ctx, "bl")
		if err != nil || len(records) != 1 || records[0].SessionID != "a" {
			t.Fatalf("partial failed batch: %+v %v", records, err)
		}
		cp, err := s.Session(ctx, "bl", "b")
		if err != nil || cp.EndUser != "" {
			t.Fatalf("failed batch left known Session: %+v %v", cp, err)
		}
		other, err := s.Load(ctx, "other")
		if err != nil || other.Version != 0 {
			t.Fatalf("scope leak: %+v %v", other, err)
		}
		records, err = s.Recorded(ctx, "other")
		if err != nil || len(records) != 0 {
			t.Fatalf("record scope leak: %+v %v", records, err)
		}
	})

	t.Run("PreviouslyRecordedTasksAndCreations", func(t *testing.T) {
		s, _ := factory(t)
		register(t, s, "u", account.Active)
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
		source := task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "ensure", ID: "legacy", Version: 1}
		spec := task.Spec{Operation: "ensure", Target: "day/2026-10-29"}
		if err := s.Receive(ctx, source, []task.Spec{spec}, now); err != nil {
			t.Fatal(err)
		}
		taskRow, ok, err := s.Claim(ctx, task.Identity(source, spec), now, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		creation := task.Creation{Owner: source.Owner, ID: task.CreationID(taskRow), TaskID: taskRow.TaskID,
			Target: taskRow.Target, Agent: "a", Generation: taskRow.LeaseGeneration, LeaseUntil: taskRow.LeaseUntil, CreatedAt: now}
		if err := s.BeginCreation(ctx, taskRow, creation, now); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordCreated(ctx, creation.ID, "orphan"); err != nil {
			t.Fatal(err)
		}
		taskRow.State, taskRow.Session = task.Succeeded, "legacy"
		if err := s.Save(ctx, taskRow, now, true); err != nil {
			t.Fatal(err)
		}
		f := newKernel()
		f.logs["legacy"], f.owners["legacy"] = history("legacy", "u"), "u"
		untilFollow(t, &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: s})
		cp, err := s.Session(ctx, "bl", "legacy")
		if err != nil || cp.Gone || cp.Seq != 7 {
			t.Fatalf("legacy task omitted: %+v %v", cp, err)
		}
		cp, err = s.Session(ctx, "bl", "orphan")
		if err != nil || !cp.Gone {
			t.Fatalf("creation not confirmed: %+v %v", cp, err)
		}
	})

	t.Run("IndependentShardProgress", func(t *testing.T) {
		s, _ := factory(t)
		f := newKernel()
		f.heads = []kernel.OutboxShard{{Shard: 0, Status: "ready", Next: "H0"}, {Shard: 2, Status: "ready", Next: "H2"}}
		f.busy = true
		r := &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: s}
		ctx := context.Background()
		for range 12 {
			if _, err := r.Step(ctx, "bl"); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Contains(f.readShards, 0) || !slices.Contains(f.readShards, 2) {
			t.Fatalf("busy shard starved another: %v", f.readShards)
		}
		before, err := s.Load(ctx, "bl")
		if err != nil {
			t.Fatal(err)
		}
		f.outboxError = errOffline
		if _, err := r.Step(ctx, "bl"); err != errOffline {
			t.Fatalf("outage: %v", err)
		}
		after, err := s.Load(ctx, "bl")
		if err != nil || before.Version != after.Version {
			t.Fatalf("outage advanced progress: %+v %v", after, err)
		}
		f.outboxError, f.busy = nil, false
		p := untilFollow(t, r)
		if p.Positions[0].H != "H0" || p.Positions[1].H != "H2" || p.Positions[0].After == "H0" || p.Positions[1].After == "H2" {
			t.Fatalf("shard positions: %+v", p.Positions)
		}
	})

	t.Run("ExpiredRecoveryAndConfirmation", func(t *testing.T) {
		s, reopen := factory(t)
		ctx := context.Background()
		register(t, s, "u", account.Active)
		register(t, s, "v", account.Deleting)
		f := newKernel()
		f.sessions["u"] = []string{"a", "empty", "logical"}
		f.sessions["v"] = []string{"closing"}
		for _, pair := range [][2]string{{"a", "u"}, {"missing-live", "u"}, {"closing", "v"}} {
			f.logs[pair[0]], f.owners[pair[0]] = history(pair[0], pair[1]), pair[1]
		}
		f.viewErrors["logical"] = kernel.ErrNotFound
		f.readErrors["empty"] = kernel.ErrNotFound
		f.owners["empty"] = "u"
		// 已知而未列出的两个 Session：一个仍可读，一个确实 404。
		p := recovery.Progress{BusinessLine: "bl", Version: 1, Phase: recovery.Follow, Positions: []recovery.Position{{Shard: 0, After: "expired"}}}
		if err := s.Commit(ctx, 0, p, recovery.Batch{Envelopes: []kernel.Envelope{
			{BusinessLine: "bl", SessionID: "missing-live", EndUser: "u", Seq: 6, Kind: "run_completed", EnvelopeID: "known-live"},
			{BusinessLine: "bl", SessionID: "missing-gone", EndUser: "u", Seq: 6, Kind: "run_completed", EnvelopeID: "known-gone"},
		}}); err != nil {
			t.Fatal(err)
		}
		f.failConfirm = true
		r := &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: s, PageSize: 2}
		p, err := r.Step(ctx, "bl")
		if err != nil || p.Phase != recovery.Heads {
			t.Fatalf("expired: %+v %v", p, err)
		}
		checkedPending := false
		restarted := false
		for range 100 {
			p, err = r.Step(ctx, "bl")
			if err != nil {
				if err != errOffline {
					t.Fatal(err)
				}
				cp, loadErr := s.Session(ctx, "bl", "missing-gone")
				if loadErr != nil || cp.Gone {
					t.Fatalf("unlisted item deleted on network error: %+v %v", cp, loadErr)
				}
				checkedPending = true
				continue
			}
			if p.Phase == recovery.Events && p.Session == "a" && !restarted {
				// 关闭连接池后重建消费者，当前 owner、Session、H 必须仍可继续。
				s = reopen()
				r = &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: s, PageSize: 2}
				restarted = true
			}
			if p.Phase == recovery.Follow {
				break
			}
		}
		if p.Phase != recovery.Follow || !checkedPending || !restarted || f.headCalls != 1 {
			t.Fatalf("did not converge: %+v pending=%v restart=%v heads=%d", p, checkedPending, restarted, f.headCalls)
		}
		for _, id := range []string{"missing-gone", "empty", "logical"} {
			cp, err := s.Session(ctx, "bl", id)
			if err != nil || !cp.Gone || cp.EndUser != "" || cp.Seq != 0 {
				t.Fatalf("not inaccessible: %+v %v", cp, err)
			}
		}
		for _, id := range []string{"a", "closing", "missing-live"} {
			cp, err := s.Session(ctx, "bl", id)
			if err != nil || cp.Gone || cp.Seq != 7 {
				t.Fatalf("not recovered: %+v %v", cp, err)
			}
		}
		records, err := s.Recorded(ctx, "bl")
		if err != nil || len(records) != 9 {
			t.Fatalf("records=%d %v", len(records), err)
		}
		// H 之后新建且 ID 在游标前的 Session，从出站发现；同一事件的真实信封不会重复接收。
		f.follow = []kernel.Envelope{
			{EnvelopeID: "a-question", BusinessLine: "bl", EndUser: "u", SessionID: "a", Seq: 3, Kind: "question"},
			{EnvelopeID: "new", BusinessLine: "bl", EndUser: "u", SessionID: "0-new", Seq: 2, Kind: "run_failed"},
		}
		if _, err := r.Step(ctx, "bl"); err != nil {
			t.Fatal(err)
		}
		records, err = s.Recorded(ctx, "bl")
		if err != nil || len(records) != 10 {
			t.Fatalf("dedup/new session: %d %v", len(records), err)
		}
		cp, err := s.Session(ctx, "bl", "0-new")
		if err != nil || cp.EndUser != "u" || cp.Seq != 0 {
			t.Fatalf("sparse envelope advanced log checkpoint: %+v %v", cp, err)
		}
	})

	t.Run("HeadExpiresAndEpochChanges", func(t *testing.T) {
		s, _ := factory(t)
		register(t, s, "u", account.Active)
		f := newKernel()
		f.sessions["u"], f.logs["a"], f.owners["a"] = []string{"a"}, history("a", "u"), "u"
		f.expireH = true
		r := &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: s, PageSize: 2}
		p := untilFollow(t, r)
		if p.Round != 2 || f.headCalls != 2 {
			t.Fatalf("H not restarted: %+v heads=%d", p, f.headCalls)
		}
		records, err := s.Recorded(context.Background(), "bl")
		if err != nil || len(records) != 3 {
			t.Fatalf("restart duplicated: %d %v", len(records), err)
		}
		f.epoch = true
		p = untilFollow(t, r)
		if p.Round != 3 || f.headCalls != 3 {
			t.Fatalf("epoch not restarted: %+v", p)
		}
	})

	t.Run("AtomicCrashAndCAS", func(t *testing.T) {
		s, reopen := factory(t)
		register(t, s, "u", account.Active)
		ctx := context.Background()
		p := recovery.Progress{BusinessLine: "bl", Version: 1, Phase: recovery.Events, Round: 1,
			Owner: "u", Session: "a", ReturnPhase: recovery.Sessions, Positions: []recovery.Position{{Shard: 0, H: "H1", After: "H1"}}}
		if err := s.Commit(ctx, 0, p, recovery.Batch{}); err != nil {
			t.Fatal(err)
		}
		f := newKernel()
		f.logs["a"], f.owners["a"] = history("a", "u"), "u"
		fault := &crashConsumer{Consumer: s, before: true}
		r := &recovery.Recoverer{Accounts: s, Kernel: f, Consumer: fault, PageSize: 2}
		if _, err := r.Step(ctx, "bl"); err != errOffline {
			t.Fatalf("before commit: %v", err)
		}
		cp, _ := s.Session(ctx, "bl", "a")
		if cp.Seq != 0 {
			t.Fatal("failed commit changed checkpoint")
		}
		fault.before, fault.after = false, true
		if _, err := r.Step(ctx, "bl"); err != errOffline {
			t.Fatalf("lost commit response: %v", err)
		}
		s = reopen()
		r.Consumer, r.Accounts = s, s
		cp, err := s.Session(ctx, "bl", "a")
		if err != nil || cp.Seq != 2 || len(cp.Questions) != 1 {
			t.Fatalf("page not durable: %+v %v", cp, err)
		}
		if err := s.Commit(ctx, 0, p, recovery.Batch{Inaccessible: "a"}); !errors.Is(err, recovery.ErrConflict) {
			t.Fatalf("stale owner accepted: %v", err)
		}
		untilFollow(t, r)
		records, err := s.Recorded(ctx, "bl")
		if err != nil || len(records) != 3 || records[0].Kind != "question" || records[0].Seq != 3 {
			t.Fatalf("replay across page/restart: %+v %v", records, err)
		}
	})

	t.Run("AccountEnumerationAndCleanup", func(t *testing.T) {
		s, _ := factory(t)
		ctx := context.Background()
		for i, user := range []string{"z", "Z", "a-1", "a_1"} {
			register(t, s, user, []account.Status{account.Active, account.Disabled, account.Deleting, account.Deleted}[i])
		}
		var got []string
		after := ""
		for range 10 {
			owners, err := s.AccountOwners(ctx, "bl", after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(owners) == 0 {
				break
			}
			after = owners[0].EndUser
			got = append(got, after)
		}
		if !slices.Equal(got, []string{"Z", "a-1", "a_1", "z"}) {
			t.Fatalf("owners: %v", got)
		}
		owner := task.Owner{BusinessLine: "bl", EndUser: "a-1"}
		e := kernel.Envelope{EnvelopeID: "e", BusinessLine: "bl", EndUser: owner.EndUser, SessionID: "a", Seq: 2, Kind: "run_completed"}
		p := recovery.Progress{BusinessLine: "bl", Version: 1}
		if err := s.Commit(ctx, 0, p, recovery.Batch{Envelopes: []kernel.Envelope{e}}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateAccount(ctx, owner, func(r *account.Record) error {
			r.Account.Status, r.Deletion.Stage, r.Cleanup = account.Deleted, "completed", true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		p.Version++
		if err := s.Commit(ctx, 1, p, recovery.Batch{Envelopes: []kernel.Envelope{e}, Checkpoint: &recovery.Checkpoint{SessionID: "a", EndUser: owner.EndUser, Seq: 2}}); err != nil {
			t.Fatal(err)
		}
		records, err := s.Recorded(ctx, "bl")
		if err != nil || len(records) != 0 {
			t.Fatalf("late page resurrected account: %+v %v", records, err)
		}
		cp, err := s.Session(ctx, "bl", "a")
		if err != nil || !cp.Gone || cp.EndUser != "" {
			t.Fatalf("cleanup: %+v %v", cp, err)
		}
	})

	t.Run("DeletionEnvelopeWinsOverReplay", func(t *testing.T) {
		s, _ := factory(t)
		ctx := context.Background()
		ordinary := kernel.Envelope{EnvelopeID: "ordinary", BusinessLine: "bl", EndUser: "u", SessionID: "a", Seq: 3, Kind: "question"}
		deleted := kernel.Envelope{EnvelopeID: "deleted", BusinessLine: "bl", SessionID: "a", Kind: "session_deleted"}
		for i, batch := range []recovery.Batch{
			{Envelopes: []kernel.Envelope{ordinary}},
			{Envelopes: []kernel.Envelope{deleted, ordinary}},
			{Envelopes: []kernel.Envelope{ordinary, deleted}},
		} {
			if err := s.Commit(ctx, uint64(i), recovery.Progress{BusinessLine: "bl", Version: uint64(i + 1)}, batch); err != nil {
				t.Fatal(err)
			}
		}
		records, err := s.Recorded(ctx, "bl")
		if err != nil || len(records) != 1 || records[0].Kind != "session_deleted" {
			t.Fatalf("deletion replay: %+v %v", records, err)
		}
	})
}

func register(t *testing.T, s Store, user string, status account.Status) {
	t.Helper()
	if _, err := s.Register(context.Background(), "identity/"+user, account.Account{Owner: task.Owner{BusinessLine: "bl", EndUser: user}, Status: status}); err != nil {
		t.Fatal(err)
	}
}

func untilFollow(t *testing.T, r *recovery.Recoverer) recovery.Progress {
	t.Helper()
	for range 150 {
		p, err := r.Step(context.Background(), "bl")
		if err != nil {
			t.Fatal(err)
		}
		if p.Phase == recovery.Follow {
			return p
		}
	}
	t.Fatal("recovery did not converge")
	return recovery.Progress{}
}

var errOffline = errors.New("offline")

type crashConsumer struct {
	recovery.Consumer
	before, after bool
}

func (c *crashConsumer) Commit(ctx context.Context, expected uint64, p recovery.Progress, b recovery.Batch) error {
	if c.before {
		return errOffline
	}
	if err := c.Consumer.Commit(ctx, expected, p, b); err != nil {
		return err
	}
	if c.after {
		return errOffline
	}
	return nil
}

type fakeKernel struct {
	heads                       []kernel.OutboxShard
	readShards                  []int
	busy                        bool
	outboxError                 error
	sessions                    map[string][]string
	logs                        map[string][]json.RawMessage
	owners                      map[string]string
	viewErrors, readErrors      map[string]error
	headCalls                   int
	expireH, epoch, failConfirm bool
	follow                      []kernel.Envelope
}

func newKernel() *fakeKernel {
	return &fakeKernel{sessions: map[string][]string{}, logs: map[string][]json.RawMessage{}, owners: map[string]string{},
		viewErrors: map[string]error{}, readErrors: map[string]error{}}
}

func (f *fakeKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	f.headCalls++
	if f.heads != nil {
		return f.heads, nil
	}
	return []kernel.OutboxShard{{Shard: 0, Status: "ready", Next: fmt.Sprintf("H%d", f.headCalls)}}, nil
}
func (f *fakeKernel) Outbox(_ context.Context, _ string, shard int, after string, limit int) (kernel.OutboxPage, error) {
	f.readShards = append(f.readShards, shard)
	if f.outboxError != nil {
		return kernel.OutboxPage{}, f.outboxError
	}
	if after == "expired" || f.expireH && after == "H1" {
		return kernel.OutboxPage{}, &kernel.HTTPError{StatusCode: http.StatusGone, Reason: "outbox_cursor_expired"}
	}
	if f.epoch {
		f.epoch = false
		return kernel.OutboxPage{}, &kernel.HTTPError{StatusCode: http.StatusGone, Reason: "outbox_epoch_changed"}
	}
	page := kernel.OutboxPage{Next: after + "+", Events: []kernel.Envelope{}}
	if f.busy && shard == 0 {
		page.Events = []kernel.Envelope{{BusinessLine: "bl", EndUser: "u", SessionID: "busy", Seq: 2, Kind: "run_completed", EnvelopeID: "busy"}}
	}
	if len(f.follow) > 0 {
		page.Events, f.follow = f.follow[:min(limit, len(f.follow))], f.follow[min(limit, len(f.follow)):]
	}
	return page, nil
}
func (f *fakeKernel) StableSessions(_ context.Context, line, user, after string, limit int) ([]kernel.SessionInfo, error) {
	out := []kernel.SessionInfo{}
	for _, id := range f.sessions[user] {
		if id > after && len(out) < limit {
			out = append(out, kernel.SessionInfo{ID: id, EndUser: user, Agent: "a", CreatedAt: time.Now()})
		}
	}
	return out, nil
}
func (f *fakeKernel) SessionView(_ context.Context, line, id string) (kernel.SessionView, error) {
	if f.failConfirm && id == "missing-gone" {
		f.failConfirm = false
		return kernel.SessionView{}, errOffline
	}
	if err := f.viewErrors[id]; err != nil {
		return kernel.SessionView{}, err
	}
	if f.owners[id] == "" {
		return kernel.SessionView{}, kernel.ErrNotFound
	}
	return kernel.SessionView{ID: id, BusinessLine: line, EndUser: f.owners[id]}, nil
}
func (f *fakeKernel) Events(_ context.Context, line, id string, after uint64, limit int) (kernel.EventPage, error) {
	if err := f.readErrors[id]; err != nil {
		return kernel.EventPage{}, err
	}
	all := f.logs[id]
	if after > uint64(len(all)) {
		return kernel.EventPage{}, errors.New("bad test checkpoint")
	}
	end := min(int(after)+limit, len(all))
	return kernel.EventPage{Events: all[int(after):end], Last: uint64(end)}, nil
}

func history(id, user string) []json.RawMessage {
	payloads := []string{
		fmt.Sprintf(`"session_created":{"business_line":"bl","end_user":%q,"agent":{"name":"a","version":"1"}}`, user),
		`"assistant_message":{"run_id":"r","tool_calls":[{"call_id":"q","capability":"ask_user"}]}`,
		`"tool_call_started":{"run_id":"r","call_id":"q","node_id":"@user"}`,
		`"tool_call_started":{"run_id":"r","call_id":"q","node_id":"@user"}`,
		`"tool_result":{"run_id":"r","call_id":"q"}`,
		`"run_completed":{"run_id":"r"}`,
		`"session_closed":{}`,
	}
	out := []json.RawMessage{}
	for i, payload := range payloads {
		out = append(out, json.RawMessage(fmt.Sprintf(`{"session_id":%q,"seq":"%d",%s}`, id, i+1, payload)))
	}
	return out
}
