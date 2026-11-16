package session_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/session"
)

// 委托交接的复制记录（ADR-0055）只做审计：字段齐全、副本在本 Session 的槽位上、来源是另一个 Session。
func TestArtifactCopiedValidation(t *testing.T) {
	const sid = "ses_v1_005_000_s"
	for _, kind := range []string{"valid", "call", "source", "artifact", "self", "slot"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := &session.Store{Log: memlog.New(), Clock: clock.NewFake(time.Unix(0, 0)), IDs: ids.Sequential("e")}
			st := &session.State{SessionID: sid}
			if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{}}}); err != nil {
				t.Fatal(err)
			}
			m := &v1.ArtifactCopied{CallId: "call", SourceSessionId: "ses_v1_001_000_p", SourceArtifactId: "art_v1_001_src", ArtifactId: "art_v1_005_dst"}
			switch kind {
			case "call":
				m.CallId = ""
			case "source":
				m.SourceArtifactId = ""
			case "artifact":
				m.ArtifactId = ""
			case "self":
				m.SourceSessionId = sid
			case "slot":
				m.ArtifactId = "art_v1_006_dst"
			}
			before := st.Seq
			err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ArtifactCopied{ArtifactCopied: m}})
			if kind == "valid" {
				if err != nil || st.Seq != before+1 {
					t.Fatalf("valid copy record: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid copy record accepted")
			}
		})
	}
}
