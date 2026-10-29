package idcheck_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/pg/idcheck"
	"yanshi/internal/pg/pgtest"
)

func TestHistoricalPreflight(t *testing.T) {
	for _, collision := range []bool{false, true} {
		name := "clean"
		if collision {
			name = "collision"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := pgtest.Fresh(t)
			id := "art_legacy"
			callID := "call_legacy"
			if collision {
				id = "art_v1_000_historical"
				callID = "call_v9_broken"
			}
			if err := (pgartifact.Meta{Pool: pool}).Create(ctx, &artifact.Meta{ID: id, SessionID: "ses_old", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			event := &v1.Event{Id: "event_old", SessionId: "ses_old", Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{
				RunId: "run_old", ToolCalls: []*v1.ToolCall{{CallId: callID, ArgumentsJson: `{"private":"mem_v9_secret"}`}},
				Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "secret_v9_private"}}}, {Kind: &v1.ContentBlock_Media{Media: &v1.Media{Uri: "artifact://" + id}}}},
			}}}
			data, err := proto.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO events(session_id,seq,data) VALUES('ses_old',1,$1)`, data); err != nil {
				t.Fatal(err)
			}
			var findings []idcheck.Finding
			count, err := idcheck.ScanHistorical(ctx, pool, func(f idcheck.Finding) error { findings = append(findings, f); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !collision {
				if count != 0 || idcheck.RequireClean(count) != nil {
					t.Fatalf("clean snapshot: %+v", findings)
				}
			} else {
				seen := map[string]bool{}
				for _, f := range findings {
					seen[f.Reason] = true
					if strings.Contains(f.ID, "private") || strings.Contains(f.ID, "secret") {
						t.Fatal("report included content")
					}
				}
				if !seen["historical_namespace_collision"] || !seen["invalid_id"] || idcheck.RequireClean(count) == nil {
					t.Fatalf("missed collision: %+v", findings)
				}
			}
			// 预检不得修改原 ID 或载荷。
			var stored string
			if err := pool.QueryRow(ctx, `SELECT id FROM artifacts`).Scan(&stored); err != nil || stored != id {
				t.Fatal(stored, err)
			}
		})
	}
}

func TestHistoricalPreflightDerivedPayloads(t *testing.T) {
	ctx := context.Background()
	pool, _ := pgtest.Fresh(t)
	snap := &v1.SessionSnapshot{SessionId: "ses_old", Runs: []*v1.RunSnapshot{{Id: "run_old", Decisions: map[string]string{"tool_result_injection/call_v8_old": "clean"}}}}
	data, err := proto.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO session_snapshots(session_id,seq,data) VALUES('ses_old',1,$1)`, data); err != nil {
		t.Fatal(err)
	}
	event := &v1.Event{Payload: &v1.Event_DecisionMade{DecisionMade: &v1.DecisionMade{Kind: "tool_result_injection", Subject: "call_v7_old"}}}
	data, err = proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events(session_id,seq,data) VALUES('ses_old',1,$1),('ses_old',2,$2),('ses_old',3,$3)`, data, []byte{0xff}, []byte{0xf8, 0x07, 0x01}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	_, err = idcheck.ScanHistorical(ctx, pool, func(f idcheck.Finding) error { seen[f.ID] = true; seen[f.Reason] = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !seen["call_v8_old"] || !seen["call_v7_old"] || !seen["invalid_protobuf"] || !seen["unparsed_protobuf_fields"] {
		t.Fatalf("derived references not scanned: %v", seen)
	}
}
