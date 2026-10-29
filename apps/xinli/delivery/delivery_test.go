package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/paging"
	"persona/query"
	"persona/store"
	"xinli/account"
	"xinli/delivery"
	"xinli/kernel"
	"xinli/pg/pgtest"
	"xinli/recovery"
	"xinli/task"
	"xinli/task/memstore"
	"xinli/task/pgstore"
)

type appStore interface {
	delivery.Store
	recovery.Consumer
	account.Store
	account.Enumerator
	task.Store
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	store   appStore
	reopen  func() appStore
	owner   task.Owner
	now     time.Time
	kernel  *fakeKernel
	persona *fakePersona
	sink    *delivery.Fake
	worker  *delivery.Worker
}

func setup(t *testing.T, backend string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), owner: task.Owner{BusinessLine: "bl", EndUser: "user"}}
	if backend == "pg" {
		pool := pgtest.New(t)
		f.reopen = func() appStore { return pgstore.New(pool) }
		f.store = f.reopen()
	} else {
		f.store = memstore.New()
		f.reopen = func() appStore { return f.store }
	}
	_, err := f.store.Register(f.ctx, "identity", account.Account{Owner: f.owner, Status: account.Active, TimeZone: "UTC", CreatedAt: f.now})
	if err != nil {
		t.Fatal(err)
	}
	f.kernel = &fakeKernel{owner: f.owner, waiting: "question", receipts: map[string]kernel.InputReceipt{}}
	f.persona = &fakePersona{}
	f.sink = &delivery.Fake{}
	f.worker = &delivery.Worker{Store: f.store, Accounts: f.store, Persona: f.persona, Kernel: f.kernel, Push: f.sink, Channel: f.sink, Now: func() time.Time { return f.now }}
	return f
}

func each(t *testing.T, fn func(*testing.T, *fixture)) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) { fn(t, setup(t, backend)) })
	}
}

func (f *fixture) envelope(kind string, seq uint64) kernel.Envelope {
	return kernel.Envelope{BusinessLine: f.owner.BusinessLine, EndUser: f.owner.EndUser, SessionID: "session", Agent: "xinli-assistant", EnvelopeID: fmt.Sprintf("env-%d", seq), Seq: seq, Kind: kind, RunID: "run", CallID: "call"}
}

func (f *fixture) receive(envelopes ...kernel.Envelope) {
	f.t.Helper()
	p, err := f.store.Load(f.ctx, "bl")
	if err != nil {
		f.t.Fatal(err)
	}
	version := p.Version
	p.Version++
	p.Phase = recovery.Follow
	p.Positions = []recovery.Position{{Shard: 0, H: "head", After: fmt.Sprintf("cursor-%d", p.Version)}}
	if err := f.store.Commit(f.ctx, version, p, recovery.Batch{Envelopes: envelopes}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) process() {
	f.t.Helper()
	c := delivery.Consumer{Store: f.store, Now: func() time.Time { return f.now }}
	if err := c.Process(f.ctx, "bl"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) state() delivery.State {
	f.t.Helper()
	s, err := f.store.ReadDelivery(f.ctx, f.owner)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) runJobs() {
	f.t.Helper()
	for id := range f.state().Jobs {
		if err := f.worker.Run(f.ctx, f.owner, id); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestReceiptCrashReplayAndRollback(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		e := f.envelope("run_completed", 4)
		f.receive(e)
		if len(f.state().Items) != 0 {
			t.Fatal("processed before durable receive boundary")
		}
		// PG 用新 Store 实例重启，内存复用持久存储、重建消费者。
		f.store = f.reopen()
		f.worker.Store, f.worker.Accounts = f.store, f.store
		f.process()
		f.receive(e)
		f.process()
		if s := f.state(); len(s.Items) != 1 || len(s.Jobs) != 1 {
			t.Fatalf("replay duplicated: %+v", s)
		}
		f.runJobs()
		f.runJobs()
		if len(f.sink.Sent()) != 1 {
			t.Fatal("duplicate send")
		}
		p, _ := f.store.Load(f.ctx, "bl")
		before := p.Version
		p.Version++
		p.Positions[0].After = "must-rollback"
		fresh := f.envelope("question", 5)
		conflict := f.envelope("question", 6)
		conflict.EnvelopeID = e.EnvelopeID
		if err := f.store.Commit(f.ctx, before, p, recovery.Batch{Envelopes: []kernel.Envelope{fresh, conflict}}); err == nil {
			t.Fatal("expected unique identity failure")
		}
		p, _ = f.store.Load(f.ctx, "bl")
		if p.Version != before || p.Positions[0].After == "must-rollback" {
			t.Fatal("cursor escaped failed transaction")
		}
		pending, err := f.store.Pending(f.ctx, "bl", 100)
		if err != nil || len(pending) != 0 {
			t.Fatalf("partial receipt escaped: %v %v", pending, err)
		}
		if err := f.store.UpdateDelivery(f.ctx, f.owner, func(s *delivery.State) error { s.Items = nil; return errors.New("rollback") }); err == nil {
			t.Fatal("expected rollback")
		}
		if len(f.state().Items) != 1 {
			t.Fatal("callback leaked")
		}
	})
}

func TestRecheckBeforeSend(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		change     func(*fixture)
	}{
		{"answered", "question", func(f *fixture) { f.kernel.waiting = "" }},
		{"approved", "approval_requested", func(f *fixture) { f.kernel.waiting = "" }},
		{"redo", "run_completed", func(f *fixture) { f.kernel.redo = true }},
		{"deleted", "run_completed", func(f *fixture) { f.kernel.deleted = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			each(t, func(t *testing.T, f *fixture) {
				f.receive(f.envelope(test.kind, 4))
				f.process()
				test.change(f)
				f.runJobs()
				for _, job := range f.state().Jobs {
					if job.State != "cancelled" {
						t.Fatal(job.State)
					}
				}
				if len(f.sink.Sent()) != 0 {
					t.Fatal("stale message sent")
				}
			})
		})
	}
}

func TestDeletionClearsInboxAndPending(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		e := f.envelope("question", 4)
		f.receive(e)
		f.process()
		f.receive(kernel.Envelope{EnvelopeID: "deleted", BusinessLine: "bl", SessionID: "session", Kind: "session_deleted"})
		if s := f.state(); len(s.Items) != 0 || len(s.Jobs) != 0 {
			t.Fatal("deletion did not cancel delivery")
		}
		f.receive(e)
		if err := f.store.Process(f.ctx, e, f.now); err != nil {
			t.Fatal(err)
		}
		f.runJobs()
		if len(f.state().Items) != 0 || len(f.sink.Sent()) != 0 {
			t.Fatal("late receipt resurrected session")
		}
	})
}

func TestInternalTerminalWakesSafeReconciliation(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		source := task.Source{Owner: f.owner, ID: "fact", Version: 1, Kind: "day"}
		spec := task.Spec{Operation: "collect", Target: "project/p"}
		if err := f.store.Receive(f.ctx, source, []task.Spec{spec}, f.now); err != nil {
			t.Fatal(err)
		}
		job, ok, err := f.store.Claim(f.ctx, task.Identity(source, spec), f.now, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim: %v", err)
		}
		job.State, job.Session = task.Accepted, "session"
		job.Receipt = kernel.InputReceipt{InputID: job.InputID, RunID: "run"}
		job.RetryAt = f.now.Add(time.Hour)
		if err := f.store.Save(f.ctx, job, f.now, true); err != nil {
			t.Fatal(err)
		}
		e := f.envelope("run_completed", 4)
		e.Agent = "xinli-day"
		f.receive(e)
		f.process()
		job, err = f.store.Get(f.ctx, job.TaskID)
		if err != nil || job.RetryAt.After(f.now) || job.State != task.Accepted || job.Receipt.Terminal != "" {
			t.Fatalf("unsafe terminal shortcut or missed wake: %+v %v", job, err)
		}
		if len(f.state().Items) != 0 || len(f.state().Jobs) != 0 {
			t.Fatal("internal session entered inbox")
		}
	})
}

func TestCommitmentRevisionAndPreferences(t *testing.T) {
	for _, change := range []string{"reschedule", "done", "cancelled", "unverified"} {
		t.Run(change, func(t *testing.T) {
			each(t, func(t *testing.T, f *fixture) {
				f.persona.items = []query.CommitmentView{{Commitment: domain.Commitment{ID: "promise", Kind: "mine", Status: "open", DueDate: "2026-10-29", Revisions: []domain.Revision{{}}}, Rev: 1}}
				// 免打扰让任务先持久化，随后业务变更发生在实际发送之前。
				if err := f.store.UpdateAccount(f.ctx, f.owner, func(r *account.Record) error {
					r.Account.Preferences = json.RawMessage(`{"notifications":{"channel":"push","quiet_start":"11:00","quiet_end":"13:00"}}`)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.worker.ScanOwner(f.ctx, f.owner); err != nil {
					t.Fatal(err)
				}
				if len(f.state().Jobs) != 1 || len(f.sink.Sent()) != 0 {
					t.Fatal("quiet period not respected")
				}
				m := &f.persona.items[0]
				m.Revisions = append(m.Revisions, domain.Revision{})
				m.Rev++
				switch change {
				case "reschedule":
					m.DueDate = "2026-11-01"
				case "unverified":
					m.Unverified = "needs_review"
				default:
					m.Status = change
				}
				f.now = f.now.Add(2 * time.Hour)
				if err := f.worker.ScanOwner(f.ctx, f.owner); err != nil {
					t.Fatal(err)
				}
				for _, job := range f.state().Jobs {
					if job.State != "cancelled" {
						t.Fatal("old reminder active")
					}
				}
				if len(f.sink.Sent()) != 0 {
					t.Fatal("old promise reminder sent")
				}
			})
		})
	}
}

func TestUnknownSendCanDuplicateAndRetriesPersist(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.receive(f.envelope("run_completed", 4))
		f.process()
		f.sink.Outcomes(errors.New("offline"), delivery.ErrUnknown, nil)
		var id string
		for key := range f.state().Jobs {
			id = key
		}
		if err := f.worker.Run(f.ctx, f.owner, id); err == nil {
			t.Fatal("expected failure")
		}
		job := f.state().Jobs[id]
		if job.State != "pending" || !job.RetryAt.After(f.now) || len(f.sink.Sent()) != 0 {
			t.Fatal("retry not persisted")
		}
		f.now = job.RetryAt
		if err := f.worker.Run(f.ctx, f.owner, id); !errors.Is(err, delivery.ErrUnknown) {
			t.Fatal(err)
		}
		f.now = f.state().Jobs[id].RetryAt
		if err := f.worker.Run(f.ctx, f.owner, id); err != nil {
			t.Fatal(err)
		}
		if len(f.sink.Sent()) != 2 {
			t.Fatal("fake hid unknown-result duplicate")
		}
		data, _ := json.Marshal(f.sink.Sent()[0].Push)
		if strings.Contains(string(data), "text") || strings.Contains(string(data), "body") {
			t.Fatal("push contains body")
		}
	})
}

func TestChannelDowngradeAndInboundDedup(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		in := delivery.Inbound{Store: f.store, Accounts: f.store, Kernel: f.kernel}
		if err := in.Bind(f.ctx, f.owner, "fake", "address"); err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateAccount(f.ctx, f.owner, func(r *account.Record) error {
			r.Account.Preferences = json.RawMessage(`{"notifications":{"channel":"channel"}}`)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.kernel.waiting = "approval"
		f.receive(f.envelope("approval_requested", 4))
		f.process()
		f.runJobs()
		sent := f.sink.Sent()
		if len(sent) != 1 || sent[0].Message == nil || !strings.Contains(sent[0].Message.Text, "App") || sent[0].Message.DeepLink == "" {
			t.Fatal("approval not downgraded")
		}
		if _, err := in.Message(f.ctx, f.owner, "fake", "address", "session", "approval", "approve", "yes"); !errors.Is(err, delivery.ErrChannelApproval) {
			t.Fatal("channel approved")
		}
		for i := 0; i < 2; i++ {
			if _, err := in.Message(f.ctx, f.owner, "fake", "address", "session", "msg", "message", "hello"); err != nil {
				t.Fatal(err)
			}
		}
		if f.kernel.submissions != 1 {
			t.Fatal("duplicate inbound submission")
		}
		if text := delivery.ChannelText("question"); !strings.Contains(text, "App") {
			t.Fatal(text)
		}
		if text := delivery.ChannelText("run_completed"); !strings.Contains(text, "卡片") {
			t.Fatal(text)
		}
		if _, err := in.Message(f.ctx, f.owner, "fake", "wrong", "session", "other", "message", "hello"); !errors.Is(err, account.ErrUnauthorized) {
			t.Fatal("unbound address accepted")
		}
	})
}

type fakePersona struct{ items []query.CommitmentView }

func (p *fakePersona) Read(_ context.Context, _ store.Owner, q query.Request) (json.RawMessage, error) {
	if q.Resource != "commitments" {
		return nil, errors.New("unexpected query")
	}
	return json.Marshal(paging.Page[query.CommitmentView]{Items: p.items})
}

type fakeKernel struct {
	owner         task.Owner
	waiting       string
	deleted, redo bool
	reply         bool
	submissions   int
	receipts      map[string]kernel.InputReceipt
}

func (k *fakeKernel) SessionView(_ context.Context, line, id string) (kernel.SessionView, error) {
	if k.deleted {
		return kernel.SessionView{}, kernel.ErrNotFound
	}
	out := kernel.SessionView{ID: id, BusinessLine: line, EndUser: k.owner.EndUser}
	out.Agent.Name = "xinli-assistant"
	if k.waiting != "" {
		out.Runs = []kernel.ThreadRun{{ID: "run", Waiting: &kernel.Waiting{Kind: k.waiting, CallID: "call"}}}
	}
	return out, nil
}
func (k *fakeKernel) Events(_ context.Context, _, _ string, after uint64, _ int) (kernel.EventPage, error) {
	if after == 0 && k.redo {
		return kernel.EventPage{Events: []json.RawMessage{json.RawMessage(`{"run_requested":{"run_id":"new","redo_of":"run"}}`)}, Last: 8}, nil
	}
	if after == 0 && k.reply {
		return kernel.EventPage{Events: []json.RawMessage{
			json.RawMessage(`{"seq":"2","assistant_message":{"run_id":"run","reasoning":"private reasoning","tool_calls":[{"arguments_json":"private arguments"}],"content":[{"text":{"text":"intermediate"}}]}}`),
			json.RawMessage(`{"seq":"3","assistant_message":{"run_id":"run","reasoning":"private reasoning","content":[{"text":{"text":"final reply"}}]}}`),
		}, Last: 3}, nil
	}
	return kernel.EventPage{Events: []json.RawMessage{}, Last: after}, nil
}
func (k *fakeKernel) SubmitInput(_ context.Context, _, id, _ string) (kernel.InputReceipt, error) {
	k.submissions++
	r := kernel.InputReceipt{InputID: id, RunID: "channel-run"}
	k.receipts[id] = r
	return r, nil
}
func (k *fakeKernel) FindInput(_ context.Context, _, id string) (kernel.InputReceipt, bool, error) {
	r, ok := k.receipts[id]
	return r, ok, nil
}

func TestUnverifiedAndPossibleDuplicateReminders(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.persona.items = []query.CommitmentView{
			{Commitment: domain.Commitment{ID: "old", Kind: "mine", Content: "交付报告", Counterpart: "同事", Status: "open", DueDate: "2026-10-29", UpdatedAt: f.now.Add(-time.Hour)}},
			{Commitment: domain.Commitment{ID: "new", Kind: "mine", Content: "交付报告", Counterpart: "同事", Status: "open", DueDate: "2026-11-01", UpdatedAt: f.now}},
			{Commitment: domain.Commitment{ID: "unverified", Kind: "mine", Content: "需核对", Status: "open", DueDate: "2026-10-29", Unverified: "unverified"}},
		}
		if err := f.worker.ScanOwner(f.ctx, f.owner); err != nil {
			t.Fatal(err)
		}
		if len(f.state().Jobs) != 0 || len(f.sink.Sent()) != 0 {
			t.Fatal("date filter revived duplicate or unverified promise")
		}
	})
}

func TestAccountDeletionCleansDeliveryAndRejectsLateWrites(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.receive(f.envelope("question", 4))
		f.process()
		if err := f.store.UpdateAccount(f.ctx, f.owner, func(r *account.Record) error { r.Account.Status = account.Deleting; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateAccount(f.ctx, f.owner, func(r *account.Record) error {
			r.Account.Status = account.Deleted
			r.Account.Preferences = nil
			r.Deletion.Stage = "completed"
			r.Cleanup = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateDelivery(f.ctx, f.owner, func(s *delivery.State) error { s.Receive(f.envelope("question", 5), f.now); return nil }); !errors.Is(err, account.ErrInactive) {
			t.Fatal("late notification after deletion", err)
		}
		f.receive(f.envelope("question", 5))
		f.process()
		if _, err := f.store.ReadDelivery(f.ctx, f.owner); !errors.Is(err, account.ErrInactive) {
			t.Fatal("deleted data readable", err)
		}
	})
}

func TestChannelSessionKnownAndDeletedBeforeOutboxReply(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		in := delivery.Inbound{Store: f.store, Accounts: f.store, Kernel: f.kernel}
		if err := in.Bind(f.ctx, f.owner, "fake", "address"); err != nil {
			t.Fatal(err)
		}
		if _, err := in.Message(f.ctx, f.owner, "fake", "address", "session", "message", "message", "hello"); err != nil {
			t.Fatal(err)
		}
		known, err := f.store.Known(f.ctx, "bl", 1, "", 100)
		if err != nil || len(known) != 1 || known[0].SessionID != "session" {
			t.Fatalf("channel session missing from recovery: %v %v", known, err)
		}
		f.receive(kernel.Envelope{EnvelopeID: "deleted", BusinessLine: "bl", SessionID: "session", Kind: "session_deleted"})
		if len(f.state().Inbound) != 0 {
			t.Fatal("inbound receipt retained after session deletion")
		}
	})
}

func TestConcurrentConsumersAndSenders(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		e := f.envelope("run_completed", 4)
		f.receive(e)
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { errs <- f.store.Process(f.ctx, e, f.now) }()
		}
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		state := f.state()
		if len(state.Jobs) != 1 || len(state.Items) != 1 {
			t.Fatal("concurrent processing duplicated")
		}
		var id string
		for key := range state.Jobs {
			id = key
		}
		for i := 0; i < 2; i++ {
			go func() { errs <- f.worker.Run(f.ctx, f.owner, id) }()
		}
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if len(f.sink.Sent()) != 1 {
			t.Fatal("concurrent senders duplicated known success")
		}
	})
}

func TestQuietHoursCrossMidnight(t *testing.T) {
	prefs, err := delivery.ReadPreferences(json.RawMessage(`{"notifications":{"channel":"push","quiet_start":"22:00","quiet_end":"07:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		hour  int
		quiet bool
	}{{21, false}, {22, true}, {0, true}, {6, true}, {7, false}} {
		quiet, err := prefs.Quiet(time.Date(2026, 10, 29, test.hour, 0, 0, 0, time.UTC), "UTC")
		if err != nil || quiet != test.quiet {
			t.Fatalf("hour %d: %v %v", test.hour, quiet, err)
		}
	}
	if _, err := delivery.ReadPreferences(json.RawMessage(`{"notifications":{"channel":"unknown"}}`)); err == nil {
		t.Fatal("invalid channel accepted")
	}
}

func TestChannelFetchesCurrentReplyWithoutReasoning(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		in := delivery.Inbound{Store: f.store, Accounts: f.store, Kernel: f.kernel}
		if err := in.Bind(f.ctx, f.owner, "fake", "address"); err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateAccount(f.ctx, f.owner, func(r *account.Record) error {
			r.Account.Preferences = json.RawMessage(`{"notifications":{"channel":"channel"}}`)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.kernel.reply = true
		f.receive(f.envelope("run_completed", 4))
		f.process()
		f.runJobs()
		sent := f.sink.Sent()
		if len(sent) != 1 || sent[0].Message == nil {
			t.Fatal("missing channel reply")
		}
		text := sent[0].Message.Text
		if !strings.Contains(text, "final reply") || strings.Contains(text, "private") || strings.Contains(text, "intermediate") || sent[0].Message.DeepLink == "" {
			t.Fatal("incorrect channel rendering", text)
		}
		state, _ := json.Marshal(f.state())
		if strings.Contains(string(state), "final reply") || strings.Contains(string(state), "private") {
			t.Fatal("body persisted in application notification state")
		}
	})
}
