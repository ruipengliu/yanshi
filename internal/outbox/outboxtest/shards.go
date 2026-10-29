package outboxtest

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/outbox"
)

// RunShards 在真实路由上验证两条流，不用拼接两个单库的消费结果冒充多片游标。
func RunShards(t *testing.T, s Stores, sessions [2]string) {
	t.Helper()
	ctx := context.Background()
	var heads [2]string
	for id, sid := range sessions {
		req := outbox.Request{BusinessLine: "stream-test", Shard: id}
		head, err := s.Outbox.Head(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		heads[id] = head
		e := outbox.Envelope{EnvelopeID: outbox.ID(sid, 1, outbox.RunCompleted), BusinessLine: req.BusinessLine, SessionID: sid, Seq: 1, Kind: outbox.RunCompleted}
		if _, err := s.Log.Append(ctx, sid, 0, []outbox.Envelope{e}, &v1.Event{}); err != nil {
			t.Fatal(err)
		}
	}
	for id, sid := range sessions {
		req := outbox.Request{BusinessLine: "stream-test", Shard: id, After: heads[id]}
		page := Published(t, s.Outbox, req, 1)
		if page.Events[0].SessionID != sid {
			t.Fatal("outbox crossed shard")
		}
		req.After = heads[1-id]
		if _, err := s.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrScope) {
			t.Fatalf("cross-shard cursor: %v", err)
		}
	}
	if _, err := s.Outbox.Head(ctx, outbox.Request{BusinessLine: "stream-test", Shard: 7}); !errors.Is(err, outbox.ErrShard) {
		t.Fatal("unknown shard accepted")
	}
}
