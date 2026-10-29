package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/domain"
	personaPG "persona/pg"
	personaTest "persona/pg/pgtest"
	"persona/store"
	personaMem "persona/store/memstore"
	personaStore "persona/store/pgstore"
	"xinli/account"
	"xinli/kernel"
	"xinli/orchestrator"
	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/task"
	"xinli/task/memstore"
	"xinli/task/pgstore"
)

type storage interface {
	account.Store
	task.Store
}
type backend struct {
	app     storage
	persona store.Store
	reopen  func()
}

func setup(t *testing.T, kind string) *backend {
	t.Helper()
	b := &backend{}
	if kind == "memory" {
		b.app = memstore.New()
		b.persona = personaMem.New()
		b.reopen = func() {}
	} else {
		pool, dsn := pgtest.NewWithDSN(t)
		books, bookDSN := personaTest.NewWithDSN(t)
		b.app = pgstore.New(pool)
		b.persona = personaStore.New(books)
		b.reopen = func() {
			pool.Close()
			books.Close()
			var err error
			pool, err = pg.Open(context.Background(), dsn, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			books, err = personaPG.Open(context.Background(), bookDSN, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(books.Close)
			b.app = pgstore.New(pool)
			b.persona = personaStore.New(books)
		}
	}
	return b
}
func service(b *backend, now *time.Time) *account.Service {
	return &account.Service{Store: b.app, Provider: account.TestProvider{Identities: []account.TestIdentity{{BusinessLine: "bl", Subject: "test-user", Code: "a-test-secret-code"}, {BusinessLine: "other", Subject: "test-user", Code: "a-test-secret-code"}}}, Now: func() time.Time { return *now }, AccessTTL: time.Minute, RefreshTTL: time.Hour}
}
func login(t *testing.T, s *account.Service, device string) account.Credentials {
	t.Helper()
	out, err := s.Login(context.Background(), "bl", "a-test-secret-code", device, "ios")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func allowed(s *account.Service, token string) bool {
	return s.WithToken(context.Background(), token, func(context.Context, task.Owner, string) error { return nil }) == nil
}
func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func each(t *testing.T, fn func(*testing.T, *backend)) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) { fn(t, setup(t, kind)) })
	}
}

func TestSessionsAndRevocation(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		first := login(t, s, "phone")
		second := login(t, s, "tablet")
		if first.Owner != second.Owner || first.AccessToken == second.AccessToken {
			t.Fatal("identity or random token")
		}
		other, err := s.Login(ctx, "other", "a-test-secret-code", "phone", "ios")
		require(t, err)
		if other.Owner == first.Owner {
			t.Fatal("cross-business-line identity")
		}
		record, err := b.app.ReadAccount(ctx, first.Owner)
		require(t, err)
		payload, _ := json.Marshal(record)
		for _, token := range []string{first.AccessToken, first.RefreshToken, second.AccessToken, second.RefreshToken} {
			if strings.Contains(string(payload), token) {
				t.Fatal("plaintext token persisted")
			}
		}
		if !strings.Contains(string(payload), account.Hash(first.AccessToken)) {
			t.Fatal("hash missing")
		}
		b.reopen()
		s.Store = b.app
		if !allowed(s, first.AccessToken) {
			t.Fatal("restart lost session")
		}
		fresh, err := s.Refresh(ctx, first.RefreshToken)
		require(t, err)
		if allowed(s, first.AccessToken) || !allowed(s, fresh.AccessToken) {
			t.Fatal("refresh did not rotate access")
		}
		if _, err = s.Refresh(ctx, first.RefreshToken); !errors.Is(err, account.ErrUnauthorized) {
			t.Fatal("refresh replay")
		}
		require(t, s.Logout(ctx, first.Owner, "phone"))
		if allowed(s, fresh.AccessToken) || !allowed(s, second.AccessToken) {
			t.Fatal("device logout isolation")
		}
		now = now.Add(2 * time.Minute)
		if allowed(s, second.AccessToken) {
			t.Fatal("access did not expire")
		}
		second, err = s.Refresh(ctx, second.RefreshToken)
		require(t, err)
		require(t, s.Logout(ctx, first.Owner, ""))
		if allowed(s, second.AccessToken) {
			t.Fatal("logout all")
		}
		active := login(t, s, "phone")
		now = now.Add(2 * time.Hour)
		if _, err = s.Refresh(ctx, active.RefreshToken); !errors.Is(err, account.ErrUnauthorized) {
			t.Fatal("refresh did not expire")
		}
		active = login(t, s, "phone")
		require(t, s.Disable(ctx, first.Owner))
		if allowed(s, active.AccessToken) {
			t.Fatal("disable authentication")
		}
		if _, err = s.Refresh(ctx, active.RefreshToken); !errors.Is(err, account.ErrUnauthorized) {
			t.Fatal("disable refresh")
		}
		if _, err = s.Login(ctx, "bl", "a-test-secret-code", "new", "ios"); !errors.Is(err, account.ErrInactive) {
			t.Fatal("disabled identity registered again")
		}
		if !allowed(s, other.AccessToken) && now.Before(other.AccessExpiresAt) {
			t.Fatal("other tenant revoked")
		}
	})
}

func TestDisabledEntrypoints(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		creds := login(t, s, "phone")
		require(t, s.Disable(ctx, creds.Owner))
		called := false
		protected := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		req := httptest.NewRequest("GET", "/product", nil)
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		response := httptest.NewRecorder()
		protected.ServeHTTP(response, req)
		if response.Code != 401 || called {
			t.Fatal("BFF auth reached handler")
		}
		capture := account.PersonaAccess{Store: b.persona, Guard: s}
		err := capture.Update(ctx, "bl", creds.Owner.EndUser, func(*domain.Book) error { called = true; return nil })
		if !errors.Is(err, account.ErrInactive) || called {
			t.Fatal("upload accepted")
		}
		source := task.Source{Owner: creds.Owner, Kind: "schedule_day", ID: "schedule/day/test", Version: 1}
		err = b.app.Receive(ctx, source, []task.Spec{{Operation: "day", Target: "day/test"}}, now)
		if !errors.Is(err, account.ErrInactive) {
			t.Fatal("schedule accepted")
		}
		source.Kind = "day"
		source.ID = "fact/day/test"
		if err = b.app.Receive(ctx, source, []task.Spec{{Operation: "collect", Target: "project/test"}}, now); !errors.Is(err, account.ErrInactive) {
			t.Fatal("fanout accepted")
		}
		// 旧输入入口也不能绕过账号闸门，即使 Persona 尚未建立注销屏障。
		orch := orchestrator.New(ctx, b.persona, nil, orchestrator.Options{Tasks: b.app, Accounts: s})
		defer func() { require(t, orch.Shutdown(ctx)) }()
		if _, err = orch.DayInput(ctx, "bl", creds.Owner.EndUser, "2026-10-29", "day", "input"); !errors.Is(err, account.ErrInactive) {
			t.Fatal("direct day input accepted")
		}
		if _, err = orch.Week(ctx, "bl", creds.Owner.EndUser); !errors.Is(err, account.ErrInactive) {
			t.Fatal("direct week input accepted")
		}
	})
}

type fakeKernel struct {
	tiers     []string
	tierErr   error
	deleteErr error
	requests  int
	polls     int
}

func (k *fakeKernel) SetTier(_ context.Context, _, _, tier string) (kernel.Tier, error) {
	k.tiers = append(k.tiers, tier)
	return kernel.Tier{Name: tier, LimitMicros: 100}, k.tierErr
}
func (k *fakeKernel) DeleteEndUser(context.Context, string, string) (string, error) {
	k.requests++
	if k.deleteErr != nil {
		err := k.deleteErr
		k.deleteErr = nil
		return "", err
	}
	return "request", nil
}
func (k *fakeKernel) EndUserDeletion(context.Context, string, string) (kernel.EndUserDeletion, error) {
	k.polls++
	completed := 0
	if k.polls > 1 {
		completed = 1
	}
	return kernel.EndUserDeletion{RequestID: "request", Sessions: 1, Completed: completed, Done: completed == 1}, nil
}
func job(t *testing.T, b *backend, owner task.Owner, op string, version uint64, now time.Time) task.Task {
	t.Helper()
	source := task.Source{Owner: owner, Kind: "account", ID: op, Version: version}
	spec := task.Spec{Operation: op, Target: op}
	out, err := b.app.Get(context.Background(), task.Identity(source, spec))
	require(t, err)
	return out
}
func TestTierVersionsAndFailures(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		c := login(t, s, "phone")
		k := &fakeKernel{}
		w := &account.Worker{Accounts: s, Tasks: b.app, Kernel: k, Persona: b.persona}
		require(t, s.SetEntitlement(ctx, c.Owner, "old"))
		require(t, s.SetEntitlement(ctx, c.Owner, "new"))
		old := job(t, b, c.Owner, account.TierOperation, 1, now)
		latest := job(t, b, c.Owner, account.TierOperation, 2, now)
		out, err := w.Run(ctx, old.TaskID)
		require(t, err)
		if out.State != task.Terminated || len(k.tiers) != 0 {
			t.Fatal("stale tier called kernel")
		}
		_, err = w.Run(ctx, latest.TaskID)
		require(t, err)
		r, err := b.app.ReadAccount(ctx, c.Owner)
		require(t, err)
		if r.Account.SyncedVersion != 2 || r.Account.EffectiveTier.Name != "new" {
			t.Fatal("effective tier not synchronized")
		}
		require(t, s.SetEntitlement(ctx, c.Owner, ""))
		reset := job(t, b, c.Owner, account.TierOperation, 3, now)
		_, err = w.Run(ctx, reset.TaskID)
		require(t, err)
		if k.tiers[len(k.tiers)-1] != "" {
			t.Fatal("default tier reset missing")
		}
		require(t, s.SetEntitlement(ctx, c.Owner, "unknown"))
		unknown := job(t, b, c.Owner, account.TierOperation, 4, now)
		k.tierErr = &kernel.HTTPError{StatusCode: 400}
		out, err = w.Run(ctx, unknown.TaskID)
		if err == nil || out.State != task.Failed || out.LastErrorClass != "configuration" {
			t.Fatal("unknown tier retries forever")
		}
		count, err := b.app.FailureCount(ctx, c.Owner)
		require(t, err)
		if count != 1 {
			t.Fatal("failure not counted")
		}
		attempts := len(k.tiers)
		now = now.Add(time.Hour)
		_, _ = w.Run(ctx, unknown.TaskID)
		if len(k.tiers) != attempts {
			t.Fatal("terminal job retried")
		}
		require(t, s.SetEntitlement(ctx, c.Owner, "late"))
		late := job(t, b, c.Owner, account.TierOperation, 5, now)
		_, err = s.BeginDeletion(ctx, c.Owner)
		require(t, err)
		_, err = w.Run(ctx, late.TaskID)
		require(t, err)
		if len(k.tiers) != attempts {
			t.Fatal("deleting account tier executed")
		}
	})
}

var errCrash = errors.New("injected crash")

type crashStore struct {
	account.Store
	stage string
	after bool
	fired bool
}

func (s *crashStore) UpdateAccount(ctx context.Context, o task.Owner, fn func(*account.Record) error) error {
	hit := false
	err := s.Store.UpdateAccount(ctx, o, func(r *account.Record) error {
		hit = !s.fired && r.Deletion.Stage == s.stage
		if err := fn(r); err != nil {
			return err
		}
		if hit && !s.after {
			s.fired = true
			return errCrash
		}
		return nil
	})
	if err == nil && hit && s.after {
		s.fired = true
		return errCrash
	}
	return err
}

func TestDeletionRestartAtEveryStep(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		for _, stage := range []string{"persona_barrier", "persona_purge", "kernel_request", "kernel_wait", "application_cleanup"} {
			for _, after := range []bool{false, true} {
				name := kind + "/" + stage + "/before"
				if after {
					name = kind + "/" + stage + "/after"
				}
				t.Run(name, func(t *testing.T) {
					b := setup(t, kind)
					now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
					s := service(b, &now)
					ctx := context.Background()
					c := login(t, s, "phone")
					require(t, s.Profile(ctx, c.Owner, "UTC", json.RawMessage(`{"setting":true}`)))
					require(t, b.persona.Update(ctx, "bl", c.Owner.EndUser, func(*domain.Book) error { return nil }))
					require(t, s.SetEntitlement(ctx, c.Owner, "old"))
					oldTier := job(t, b, c.Owner, account.TierOperation, 1, now)
					token, err := s.RequestDeletion(ctx, c.Owner)
					require(t, err)
					if allowed(s, c.AccessToken) {
						t.Fatal("deletion did not revoke")
					}
					d, err := s.DeletionWithToken(ctx, token)
					require(t, err)
					if d.Stage != "persona_barrier" {
						t.Fatal("wrong initial progress")
					}
					deletion := job(t, b, c.Owner, account.DeleteOperation, 1, now)
					crash := &crashStore{Store: b.app, stage: stage, after: after}
					s.Store = crash
					k := &fakeKernel{}
					for i := 0; i < 25; i++ {
						w := &account.Worker{Accounts: s, Tasks: b.app, Persona: b.persona, Kernel: k}
						_, runErr := w.Run(ctx, deletion.TaskID)
						if runErr != nil && !errors.Is(runErr, errCrash) && !errors.Is(runErr, task.ErrNotFound) {
							t.Fatal(runErr)
						}
						if crash.fired {
							b.reopen()
							s.Store = b.app
						}
						now = now.Add(3 * time.Minute)
						progress, err := s.Deletion(ctx, c.Owner)
						require(t, err)
						if progress.Stage == "completed" {
							break
						}
					}
					if !crash.fired {
						t.Fatal("crash boundary not exercised")
					}
					r, err := b.app.ReadAccount(ctx, c.Owner)
					require(t, err)
					if r.Account.Status != account.Deleted || r.Deletion.CompletedAt.IsZero() || len(r.Sessions) != 0 || len(r.Devices) != 0 || len(r.Account.Preferences) != 0 || r.Account.TimeZone != "" {
						t.Fatal("incomplete cleanup")
					}
					if _, err = b.app.Get(ctx, oldTier.TaskID); !errors.Is(err, task.ErrNotFound) {
						t.Fatal("old tier task retained")
					}
					if _, err = b.app.Get(ctx, deletion.TaskID); !errors.Is(err, task.ErrNotFound) {
						t.Fatal("deletion task retained")
					}
					if err = b.app.Receive(ctx, oldTier.Source, []task.Spec{oldTier.Spec}, now); !errors.Is(err, account.ErrInactive) {
						t.Fatal("late receive resurrected tasks")
					}
					if len(k.tiers) != 0 {
						t.Fatal("old tier executed during deletion")
					}
					progress, err := s.DeletionWithToken(ctx, token)
					require(t, err)
					if progress.Stage != "completed" {
						t.Fatal("completion query lost")
					}
					personaDone, err := b.persona.OwnerDeletion(ctx, "bl", c.Owner.EndUser)
					require(t, err)
					if personaDone.Status != "completed" {
						t.Fatal("Persona not purged")
					}
					newAccount := login(t, s, "phone")
					if newAccount.Owner == c.Owner {
						t.Fatal("reused EndUser ID")
					}
					if allowed(s, c.AccessToken) {
						t.Fatal("old session resurrected")
					}
				})
			}
		}
	}
}

func TestConcurrentRefreshAndRevocationGate(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		c := login(t, s, "phone")
		var wg sync.WaitGroup
		successes := make(chan account.Credentials, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := s.Refresh(ctx, c.RefreshToken)
				if err == nil {
					successes <- out
				}
			}()
		}
		wg.Wait()
		close(successes)
		if len(successes) != 1 {
			t.Fatal("refresh did not consume once")
		}
		fresh := <-successes
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- s.WithToken(ctx, fresh.AccessToken, func(context.Context, task.Owner, string) error { close(entered); <-release; return nil })
		}()
		<-entered
		short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if err := s.Disable(short, c.Owner); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("disable crossed active gate: %v", err)
		}
		close(release)
		require(t, <-done)
		require(t, s.Disable(ctx, c.Owner))
		if allowed(s, fresh.AccessToken) {
			t.Fatal("revoke not immediate")
		}
	})
}

// 外部暂时失败只能推进退避，不能跳过 Persona 或内核清理。
type failPersona struct {
	account.Persona
	fail bool
}

func (p *failPersona) BeginOwnerDeletion(ctx context.Context, bl, eu string, now time.Time) (store.OwnerDeletion, error) {
	if p.fail {
		p.fail = false
		return store.OwnerDeletion{}, errors.New("temporary")
	}
	return p.Persona.BeginOwnerDeletion(ctx, bl, eu, now)
}
func TestDeletionTransientFailureBackoff(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		c := login(t, s, "phone")
		_, err := s.BeginDeletion(ctx, c.Owner)
		require(t, err)
		d := job(t, b, c.Owner, account.DeleteOperation, 1, now)
		k := &fakeKernel{deleteErr: &kernel.HTTPError{StatusCode: 503}}
		p := &failPersona{Persona: b.persona, fail: true}
		w := &account.Worker{Accounts: s, Tasks: b.app, Kernel: k, Persona: p}
		out, err := w.Run(ctx, d.TaskID)
		if err == nil || !out.RetryAt.After(now) || out.Terminal() {
			t.Fatal("Persona failure did not back off")
		}
		before := out.Attempt
		out, err = w.Run(ctx, d.TaskID)
		require(t, err)
		if out.Attempt != before {
			t.Fatal("retried before due")
		}
		sawKernelFailure := false
		for range 15 {
			now = now.Add(10 * time.Minute)
			out, err = w.Run(ctx, d.TaskID)
			if err != nil {
				sawKernelFailure = true
				if out.Terminal() || !out.RetryAt.After(now) {
					t.Fatal("kernel failure did not back off")
				}
			}
			progress, e := s.Deletion(ctx, c.Owner)
			require(t, e)
			if progress.Stage == "completed" {
				break
			}
		}
		if !sawKernelFailure {
			t.Fatal("kernel retry not exercised")
		}
		progress, err := s.Deletion(ctx, c.Owner)
		require(t, err)
		if progress.Stage != "completed" {
			t.Fatal("retry never completed")
		}
	})
}

func TestHTTPLoginAndDeletionProgress(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		handler := s.Handler()
		request := func(method, path, token, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		w := request("POST", "/v1/auth/login", "", `{"business_line":"bl","proof":"a-test-secret-code","device_id":"phone","platform":"ios"}`)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var c account.Credentials
		require(t, json.Unmarshal(w.Body.Bytes(), &c))
		if strings.Contains(w.Body.String(), "kernel") {
			t.Fatal("kernel credential exposed")
		}
		if w = request("GET", "/v1/auth/account", c.RefreshToken, ""); w.Code != 401 {
			t.Fatal("refresh token authorized product access")
		}
		if w = request("PUT", "/v1/auth/account/tier", c.AccessToken, `{"tier":"paid"}`); w.Code < 400 {
			t.Fatal("App can set tier")
		}
		w = request("DELETE", "/v1/auth/account", c.AccessToken, "")
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var ticket map[string]string
		require(t, json.Unmarshal(w.Body.Bytes(), &ticket))
		if w = request("GET", "/v1/auth/account", c.AccessToken, ""); w.Code != 401 {
			t.Fatal("deletion retained login")
		}
		if w = request("GET", "/v1/auth/deletion", ticket["deletion_token"], ""); w.Code != 200 {
			t.Fatal("progress unavailable after logout")
		}
		if w = request("GET", "/v1/auth/account", ticket["deletion_token"], ""); w.Code != 401 {
			t.Fatal("deletion token granted login")
		}
	})
}

func TestAccountMutationRollback(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		c := login(t, s, "phone")
		err := b.app.UpdateAccount(ctx, c.Owner, func(r *account.Record) error {
			r.Account.Status = account.Deleted
			delete(r.Sessions, nextSession(r))
			return errCrash
		})
		if !errors.Is(err, errCrash) || !allowed(s, c.AccessToken) {
			t.Fatal("failed mutation leaked")
		}
		require(t, s.Disable(ctx, c.Owner))
		if _, err := s.BeginDeletion(ctx, c.Owner); err != nil {
			t.Fatal("disabled account cannot delete", err)
		}
	})
}
func nextSession(r *account.Record) string {
	for id := range r.Sessions {
		return id
	}
	return ""
}

type blockedTier struct {
	fakeKernel
	entered chan struct{}
	release chan struct{}
}

func (k *blockedTier) SetTier(ctx context.Context, bl, eu, tier string) (kernel.Tier, error) {
	close(k.entered)
	<-k.release
	return k.fakeKernel.SetTier(ctx, bl, eu, tier)
}
func TestTierCallSerializesEntitlementAndDeletion(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s := service(b, &now)
		ctx := context.Background()
		c := login(t, s, "phone")
		require(t, s.SetEntitlement(ctx, c.Owner, "first"))
		old := job(t, b, c.Owner, account.TierOperation, 1, now)
		k := &blockedTier{entered: make(chan struct{}), release: make(chan struct{})}
		w := &account.Worker{Accounts: s, Tasks: b.app, Kernel: k, Persona: b.persona}
		done := make(chan error, 1)
		go func() { _, err := w.Run(ctx, old.TaskID); done <- err }()
		<-k.entered
		short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		if err := s.SetEntitlement(short, c.Owner, "new"); !errors.Is(err, context.DeadlineExceeded) {
			t.Error("entitlement crossed old call", err)
		}
		cancel()
		short, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
		if _, err := s.BeginDeletion(short, c.Owner); !errors.Is(err, context.DeadlineExceeded) {
			t.Error("deletion crossed old call", err)
		}
		cancel()
		close(k.release)
		require(t, <-done)
		require(t, s.SetEntitlement(ctx, c.Owner, "new"))
		r, err := b.app.ReadAccount(ctx, c.Owner)
		require(t, err)
		if r.Account.EntitlementVersion != 2 || r.Account.SyncedVersion != 1 {
			t.Fatal("unexpected version after serialized update")
		}
	})
}
