package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

func TestShardOutageHealthySessionContinues(t *testing.T) {
	l, err := shard.ParseDeployment([]byte(`{"table":{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]},"compatibility":{"version":"e2e"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	parts := map[int]app.Stores{}
	for id := range 2 {
		pool, notifier := pgtest.FreshDatabase(t)
		parts[id] = app.PostgresShard(pool, notifier, clock.Real{}, l, id)
	}
	owner, _ := l.Locate(shard.Target{Kind: shard.EndUser, Key: "u"})
	bad := 1 - owner
	var offline atomic.Bool
	routed, err := app.Sharded(ctx, l, parts, app.ShardOptions{Available: func(_ context.Context, id int) error {
		if offline.Load() && id == bad {
			return shard.ErrUnavailable
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := memStores()
	st.Stores = routed
	e := &env{t: t, srv: instance(t, st, 2, true)}
	sessions := map[int]string{}
	for i := 0; len(sessions) < 2 && i < 40; i++ {
		sid := e.newSession()
		id, _ := l.Locate(shard.Target{Kind: shard.SessionData, Key: sid})
		sessions[id] = sid
	}
	if len(sessions) != 2 {
		t.Fatal("missing shard coverage")
	}
	if err := routed.Deletions.CreateRequest(ctx, &lifecycle.Request{ID: "progress", BusinessLine: "bl", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	offline.Store(true)
	var progress map[string]any
	if code := e.doAs(serviceToken(t), http.MethodGet, "/v1/deletions/progress", nil, &progress); code != http.StatusServiceUnavailable || progress["done"] != nil {
		t.Fatalf("incomplete deletion progress: %d %+v", code, progress)
	}
	started := time.Now()
	if code := e.do(http.MethodGet, "/v1/sessions/"+sessions[bad], nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable target: %d", code)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("unavailable target exceeded budget")
	}
	good := sessions[owner]
	e.do(http.MethodPost, "/v1/sessions/"+good+"/inputs", map[string]string{"text": "healthy"}, nil)
	e.until(good, func(v sessionView) bool { status, _ := lastRun(v); return status == "completed" })
	created := false
	for range 40 {
		var result struct {
			ID string `json:"session_id"`
		}
		code := e.do(http.MethodPost, "/v1/sessions", map[string]string{"agent": "dev"}, &result)
		if code == http.StatusServiceUnavailable {
			continue
		}
		if code != http.StatusCreated {
			t.Fatalf("healthy create: %d", code)
		}
		id, _ := l.Locate(shard.Target{Kind: shard.SessionData, Key: result.ID})
		if id != owner {
			t.Fatal("created on unavailable shard")
		}
		created = true
		break
	}
	if !created {
		t.Fatal("healthy creation never succeeded")
	}
	if code := e.doAs(serviceToken(t), http.MethodGet, fmt.Sprintf("/v1/outbox?shard=%d", owner), nil, nil); code != http.StatusOK {
		t.Fatalf("healthy outbox: %d", code)
	}
	offline.Store(false)
	e.do(http.MethodPost, "/v1/sessions/"+sessions[bad]+"/inputs", map[string]string{"text": "recovered"}, nil)
	e.until(sessions[bad], func(v sessionView) bool { status, _ := lastRun(v); return status == "completed" })
}
