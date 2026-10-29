package bff_test

import (
	"encoding/json"
	"testing"

	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
)

func TestInboxOwnerReadAndDeletion(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			h := newHarness(t, backend)
			db := h.tasks.(delivery.Store)
			progress := h.tasks.(recovery.Consumer)
			e := kernel.Envelope{EnvelopeID: "reply", BusinessLine: "bl", EndUser: h.a.Owner.EndUser, SessionID: "s-1", Agent: "xinli-assistant", Seq: 4, Kind: "run_completed", RunID: "run"}
			if err := progress.Commit(h.ctx, 0, recovery.Progress{BusinessLine: "bl", Version: 1}, recovery.Batch{Envelopes: []kernel.Envelope{e}}); err != nil {
				t.Fatal(err)
			}
			if err := db.Process(h.ctx, e, h.now); err != nil {
				t.Fatal(err)
			}
			var out struct {
				Items []delivery.Item `json:"items"`
			}
			data := h.get(h.a, "/v1/inbox", 200)["items"]
			if err := json.Unmarshal(data, &out.Items); err != nil {
				t.Fatal(err)
			}
			if len(out.Items) != 1 || out.Items[0].Read {
				t.Fatal("missing unread reply")
			}
			id := out.Items[0].ID
			for _, other := range []struct{ token string }{{h.b.AccessToken}, {h.other.AccessToken}} {
				h.request(other.token, "POST", "/v1/inbox/"+id+"/read", `{}`, 404)
			}
			h.request(h.a.AccessToken, "POST", "/v1/inbox/"+id+"/read", `{}`, 204)
			h.request(h.a.AccessToken, "POST", "/v1/inbox/"+id+"/read", `{}`, 204)
			if err := json.Unmarshal(h.get(h.a, "/v1/inbox", 200)["items"], &out.Items); err != nil || !out.Items[0].Read {
				t.Fatal("read not durable", err)
			}
			if err := json.Unmarshal(h.get(h.b, "/v1/inbox", 200)["items"], &out.Items); err != nil || len(out.Items) != 0 {
				t.Fatal("cross-owner inbox", err)
			}
			deleted := kernel.Envelope{BusinessLine: "bl", SessionID: "s-1", Kind: "session_deleted", EnvelopeID: "deleted"}
			if err := progress.Commit(h.ctx, 1, recovery.Progress{BusinessLine: "bl", Version: 2}, recovery.Batch{Envelopes: []kernel.Envelope{deleted}}); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(h.get(h.a, "/v1/inbox", 200)["items"], &out.Items); err != nil || len(out.Items) != 0 {
				t.Fatal("deleted session in inbox", err)
			}
			h.request("", "POST", "/v1/inbox/"+id+"/read", `{}`, 401)
		})
	}
}
