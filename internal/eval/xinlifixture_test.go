package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/eval/xinlifixture"
)

func TestXinliFixtureOperations(t *testing.T) {
	ctx := context.Background()
	client, servers, err := xinlifixture.Start(ctx, defaultXinliBin, BusinessLine, NoHealthBusinessLine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if len(servers) != 2 || servers[0].BusinessLine != BusinessLine || servers[1].BusinessLine != NoHealthBusinessLine {
		t.Fatalf("registrations: %+v", servers)
	}
	for _, server := range servers {
		if !server.CiteCaptures || !server.AttestsSpeakers {
			t.Fatalf("registration trust flags: %+v", server)
		}
	}
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	seed := xinlifixture.SeedData{
		Projects: []xinlifixture.Project{{ID: "P-1", Name: "项目"}},
		Days:     map[string]string{"2026-10-22": "昨日摘要"},
		Commitments: []xinlifixture.SeedCommitment{{
			ID: "C-1", Kind: "mine", Content: "发报价", Counterpart: "同事", DueDate: "2026-10-24", DueText: "明天", Project: "P-1",
			Status: "open", SpokenAt: at, Refs: []string{"#1.0"}, Unverified: "预置",
			Revisions: []xinlifixture.SeedRevision{{Author: xinlifixture.AuthorUser, Session: "edit", Op: "revise",
				Fields: map[string]string{"due_date": "2026-10-25"}, Refs: []string{"#2.0"}, Exchanges: []string{"x1"}, Reason: "改期", SpokenAt: at, RecordedAt: at}},
		}},
		Filed:     []xinlifixture.Day{{Date: "2026-10-22", Headline: "昨日总览", Awareness: xinlifixture.Awareness{Habits: []string{"复述确认"}}}},
		Exchanges: []xinlifixture.SeedExchange{{ID: "x1", Date: "2026-10-22", Title: "报价", Summary: "商定报价", Project: "P-1", Decisions: []string{"明天提交"}, Risks: []string{"待确认"}}},
	}
	if err := client.Seed(ctx, BusinessLine, "u", seed); err != nil {
		t.Fatal(err)
	}
	if err := client.Bind(ctx, BusinessLine, "u", "P-1", "project-session"); err != nil {
		t.Fatal(err)
	}
	if err := client.Submitted(ctx, BusinessLine, "u", "day-session", "整理今天"); err != nil {
		t.Fatal(err)
	}
	state, err := client.State(ctx, BusinessLine, "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Projects) != 1 || state.Projects[0].Session != "project-session" || len(state.Commitments) != 1 || state.Commitments[0].DueDate != "2026-10-25" || state.Commitments[0].Rev() != 2 || len(state.Days) != 1 || len(state.Exchanges) != 2 {
		t.Fatalf("state: %+v", state)
	}
	if err := client.Edit(ctx, BusinessLine, "u", "C-1", "done", ""); err != nil {
		t.Fatal(err)
	}
	state, err = client.State(ctx, BusinessLine, "u")
	if err != nil || state.Commitments[0].Open() {
		t.Fatalf("edited state: %+v, %v", state, err)
	}
	other, err := client.State(ctx, NoHealthBusinessLine, "u")
	if err != nil || len(other.Commitments) != 0 {
		t.Fatalf("business line isolation: %+v, %v", other, err)
	}
	if err := client.Edit(ctx, BusinessLine, "u", "missing", "done", ""); err == nil || !strings.Contains(err.Error(), "HTTP 400: no commitment missing") {
		t.Fatalf("fixture error: %v", err)
	}
	for range 2 {
		if err := client.Reset(ctx, BusinessLine, "u"); err != nil {
			t.Fatal(err)
		}
	}
	state, err = client.State(ctx, BusinessLine, "u")
	if err != nil || len(state.Projects) != 0 || len(state.Commitments) != 0 || len(state.Changesets) != 0 {
		t.Fatalf("reset state: %+v, %v", state, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.State(ctx, BusinessLine, "u"); err == nil {
		t.Fatal("closed fixture still serves HTTP")
	}
}

func TestXinliFixtureMissingBinary(t *testing.T) {
	_, _, err := xinlifixture.Start(context.Background(), t.TempDir()+"/missing", BusinessLine)
	if err == nil || !strings.Contains(err.Error(), "make eval") {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestXinliFixtureCapabilityOnly(t *testing.T) {
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "fixture-only", Version: "1", Model: "script/any", Capabilities: []string{"mcp:xinli/*"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []*Case{{Name: "fixture-only", Agent: "fixture-only"}}
	derived, versions, err := derive(Config{Agents: agents}, cases)
	if err != nil || !needsXinli(derived, cases, versions) {
		t.Fatalf("capability-only fixture: %v", err)
	}
}

// 显式检查外部 JSON 名，避免 YAML 用例仍能加载却在 HTTP Seed 时静默丢字段。
func TestXinliSeedJSON(t *testing.T) {
	raw, err := json.Marshal(xinlifixture.SeedData{Commitments: []xinlifixture.SeedCommitment{{DueDate: "2026-10-24", Revisions: []xinlifixture.SeedRevision{{RecordedAt: time.Now()}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"projects", "days", "commitments", "filed", "exchanges", "due_date", "due_text", "spoken_at", "recorded_at"} {
		if !strings.Contains(string(raw), `"`+key+`":`) {
			t.Fatalf("missing JSON key %s", key)
		}
	}
}
