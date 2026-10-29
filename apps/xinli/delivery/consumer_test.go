package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
)

type recoveringKernel struct {
	*fakeKernel
	reason string
	e      kernel.Envelope
}

func (k *recoveringKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	return []kernel.OutboxShard{{Shard: 0, Status: "ready", Next: "H"}}, nil
}
func (k *recoveringKernel) Outbox(_ context.Context, _ string, _ int, after string, _ int) (kernel.OutboxPage, error) {
	if strings.HasPrefix(after, "cursor-") {
		return kernel.OutboxPage{}, &kernel.HTTPError{StatusCode: 410, Reason: k.reason}
	}
	if after == "H" {
		return kernel.OutboxPage{Events: []kernel.Envelope{k.e}, Next: "caught-up"}, nil
	}
	return kernel.OutboxPage{Events: []kernel.Envelope{}, Next: "caught-up"}, nil
}
func (k *recoveringKernel) StableSessions(_ context.Context, _, user, after string, _ int) ([]kernel.SessionInfo, error) {
	if after != "" {
		return []kernel.SessionInfo{}, nil
	}
	return []kernel.SessionInfo{{ID: "session", EndUser: user}}, nil
}
func (k *recoveringKernel) Events(_ context.Context, _, _ string, after uint64, _ int) (kernel.EventPage, error) {
	if after > 0 {
		return kernel.EventPage{Events: []json.RawMessage{}, Last: after}, nil
	}
	created := fmt.Sprintf(`{"session_id":"session","seq":"1","session_created":{"business_line":"bl","end_user":%q,"agent":{"name":"xinli-assistant","version":"2"}}}`, k.owner.EndUser)
	completed := `{"session_id":"session","seq":"2","run_completed":{"run_id":"run"}}`
	return kernel.EventPage{Events: []json.RawMessage{json.RawMessage(created), json.RawMessage(completed)}, Last: 2}, nil
}

func TestExpiredAndEpochRecoveryFeedsDelivery(t *testing.T) {
	for _, reason := range []string{"outbox_cursor_expired", "outbox_epoch_changed"} {
		t.Run(reason, func(t *testing.T) {
			each(t, func(t *testing.T, f *fixture) {
				f.receive()
				k := &recoveringKernel{fakeKernel: f.kernel, reason: reason, e: f.envelope("run_completed", 2)}
				c := delivery.Consumer{Progress: f.store, Store: f.store, Accounts: f.store, Kernel: k}
				if err := c.Poll(f.ctx, "bl"); err != nil {
					t.Fatal(err)
				}
				p, _ := f.store.Load(f.ctx, "bl")
				if p.Phase != recovery.Heads {
					t.Fatal("cursor reset skipped recovery")
				}
				for i := 0; i < 30; i++ {
					if err := c.Poll(f.ctx, "bl"); err != nil {
						t.Fatal(err)
					}
					p, _ = f.store.Load(f.ctx, "bl")
					if p.Phase == recovery.Follow {
						break
					}
				}
				if p.Phase != recovery.Follow {
					t.Fatal("recovery did not converge")
				}
				f.process()
				if len(f.state().Items) != 1 || len(f.state().Jobs) != 1 {
					t.Fatal("recovery/catchup lost or duplicated reply")
				}
			})
		})
	}
}

func TestFilteredServiceOutboxAndSingleApplicationSender(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Header.Get("Authorization") != "Bearer service-token" {
				t.Error("not a service request")
				w.WriteHeader(403)
				return
			}
			filter := r.URL.Query().Get("kinds")
			if filter != "approval_requested,question,run_completed,run_failed,session_deleted" {
				t.Error("unstable filter", filter)
				w.WriteHeader(400)
				return
			}
			switch r.URL.Path {
			case "/v1/outbox/shards":
				_ = json.NewEncoder(w).Encode(map[string]any{"shards": []kernel.OutboxShard{{Shard: 0, Status: "ready", Next: "head"}}})
			case "/v1/outbox":
				_ = json.NewEncoder(w).Encode(kernel.OutboxPage{Events: []kernel.Envelope{f.envelope("approval_requested", 4)}, Next: "next"})
			default:
				// 唯一归属边界：应用从不调用内核推送登记或 Connection。
				t.Error("unexpected kernel registration/request", r.URL.Path)
				w.WriteHeader(404)
			}
		}))
		defer server.Close()
		source := delivery.Source{Client: &kernel.Client{BaseURL: server.URL, Tokens: tokens{}}}
		if _, err := source.OutboxHeads(f.ctx, "bl"); err != nil {
			t.Fatal(err)
		}
		page, err := source.Outbox(f.ctx, "bl", 0, "head", 100)
		if err != nil {
			t.Fatal(err)
		}
		f.receive(page.Events...)
		f.process()
		f.receive(page.Events...)
		f.process()
		f.kernel.waiting = "approval"
		f.runJobs()
		f.runJobs()
		if requests != 2 || len(f.sink.Sent()) != 1 {
			t.Fatal("unexpected registration or duplicate application send")
		}
	})
}

type tokens struct{}

func (tokens) Token(_ context.Context, line string) (string, error) {
	if line != "bl" {
		return "", errors.New("wrong business line")
	}
	return "service-token", nil
}

type failedProgress struct{ recovery.Consumer }

func (f failedProgress) Commit(context.Context, uint64, recovery.Progress, recovery.Batch) error {
	return errors.New("database offline")
}

func TestPollDatabaseFailureRetainsCursor(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.receive()
		p, _ := f.store.Load(f.ctx, "bl")
		p.Version++
		p.Positions[0].After = "H"
		if err := f.store.Commit(f.ctx, p.Version-1, p, recovery.Batch{}); err != nil {
			t.Fatal(err)
		}
		k := &recoveringKernel{fakeKernel: f.kernel, e: f.envelope("run_completed", 2)}
		c := delivery.Consumer{Progress: failedProgress{Consumer: f.store}, Store: f.store, Kernel: k, Accounts: f.store}
		if err := c.Poll(f.ctx, "bl"); err == nil {
			t.Fatal("expected receive failure")
		}
		after, _ := f.store.Load(f.ctx, "bl")
		if after.Version != p.Version || after.Positions[0].After != "H" {
			t.Fatal("cursor advanced before persistence")
		}
		c.Progress = f.store
		if err := c.Poll(f.ctx, "bl"); err != nil {
			t.Fatal(err)
		}
		f.process()
		if len(f.state().Items) != 1 {
			t.Fatal("failed batch lost")
		}
	})
}

func TestB5FilterUpgradeRestartsRecovery(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.receive()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"outbox_filter_changed"}`))
		}))
		defer server.Close()
		c := delivery.Consumer{Progress: f.store, Store: f.store, Accounts: f.store, Kernel: delivery.Source{Client: &kernel.Client{BaseURL: server.URL}}}
		if err := c.Poll(f.ctx, "bl"); err != nil {
			t.Fatal(err)
		}
		p, _ := f.store.Load(f.ctx, "bl")
		if p.Phase != recovery.Heads {
			t.Fatal("B5 cursor reused for new filter")
		}
	})
}
