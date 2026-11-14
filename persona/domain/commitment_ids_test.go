package domain

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSeedCommitmentIDAllocation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seed       []string
		wantSeed   []string
		wantCreate []string
		wantNext   string
	}{
		{"low", []string{"C-0001"}, []string{"C-0001"}, []string{"C-0002", "C-0003"}, "C-0004"},
		{"sparse", []string{"C-0001", "C-0003"}, []string{"C-0001", "C-0003"}, []string{"C-0002", "C-0004"}, "C-0005"},
		{"high", []string{"C-9000", "C-9001"}, []string{"C-9000", "C-9001"}, []string{"C-0001", "C-0002"}, "C-0003"},
		{"nonstandard", []string{"custom", "C-quote", "C-１２"}, []string{"custom", "C-quote", "C-１２"}, []string{"C-0001", "C-0002"}, "C-0003"},
		{"mixed", []string{"", "C-0001", "", "C-0003"}, []string{"C-0002", "C-0001", "C-0004", "C-0003"}, []string{"C-0005", "C-0006"}, "C-0007"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
			b := NewBook()
			seed := SeedData{}
			for _, id := range tc.seed {
				seed.Commitments = append(seed.Commitments, SeedCommitment{ID: id, Kind: "mine", Content: "预置承诺"})
			}
			b.Seed(seed, now)
			var ids []string
			for _, m := range b.State().Commitments {
				ids = append(ids, m.ID)
			}
			if !slices.Equal(ids, tc.wantSeed) {
				t.Fatalf("seed IDs = %v, want %v", ids, tc.wantSeed)
			}
			restored, err := b.Clone()
			if err != nil {
				t.Fatal(err)
			}
			b = restored
			cs := Changeset{CreateCommitments: []Commitment{{Kind: "mine"}, {ID: "#local", Kind: "mine"}}}
			b.commit(&cs, -1, nil)
			if got := []string{cs.CreateCommitments[0].ID, cs.CreateCommitments[1].ID}; !slices.Equal(got, tc.wantCreate) {
				t.Fatalf("created IDs = %v, want %v", got, tc.wantCreate)
			}
			copy, err := b.Clone()
			if err != nil {
				t.Fatal(err)
			}
			copy.Seed(SeedData{Commitments: []SeedCommitment{{Kind: "mine"}}}, now)
			if got := copy.State().Commitments; got[len(got)-1].ID != tc.wantNext {
				t.Fatalf("ID after snapshot = %s, want %s", got[len(got)-1].ID, tc.wantNext)
			}
		})
	}
}

func TestCommitmentIDAllocationReservesPendingIDs(t *testing.T) {
	b := NewBook()
	cs := Changeset{CreateCommitments: []Commitment{{}, {ID: "C-0001"}, {}, {ID: "C-0003"}}}
	b.commit(&cs, -1, nil)
	var ids []string
	for _, m := range cs.CreateCommitments {
		ids = append(ids, m.ID)
	}
	if want := []string{"C-0002", "C-0001", "C-0004", "C-0003"}; !slices.Equal(ids, want) {
		t.Fatalf("pending IDs collided: %v, want %v", ids, want)
	}
}

func TestCommitmentIDAllocationKeepsClosedAndMergedIDs(t *testing.T) {
	b := NewBook()
	b.Seed(SeedData{Commitments: []SeedCommitment{
		{ID: "C-0001", Kind: "mine", Status: "done"},
		{ID: "C-0002", Kind: "mine", Status: "cancelled"},
		{ID: "C-0003", Kind: "mine", Revisions: []SeedRevision{{Op: "merge", Fields: map[string]string{"merged_into": "C-0004"}}}},
		{ID: "C-0004", Kind: "mine"},
	}}, time.Time{})
	if m := b.commitment("C-0003"); m == nil || m.MergedInto != "C-0004" {
		t.Fatal("seed merge not applied")
	}
	cs := Changeset{CreateCommitments: []Commitment{{Kind: "mine"}, {Kind: "mine"}}}
	b.commit(&cs, -1, nil)
	if got := []string{cs.CreateCommitments[0].ID, cs.CreateCommitments[1].ID}; !slices.Equal(got, []string{"C-0005", "C-0006"}) {
		t.Fatalf("closed or merged IDs reused: %v", got)
	}
}

func TestRefileCommitmentIDsAfterSeed(t *testing.T) {
	for _, mode := range []string{"matched", "explicit", "replace"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
			b := NewBook()
			b.Seed(SeedData{Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "预置承诺"}}}, now)
			cites := map[string]Citation{"#1.0": {Speaker: "wearer"}, "#1.1": {Speaker: "wearer"}, "#1.2": {Speaker: "wearer"}}
			file := func(items []map[string]any) {
				t.Helper()
				args, err := json.Marshal(map[string]any{
					"source":      map[string]string{"from": "#1.0", "to": "#1.2"},
					"exchanges":   map[string]any{"create": []map[string]any{{"from": "#1.0", "to": "#1.2", "title": "约定", "summary": "交稿安排", "projects": []map[string]string{{"match": "none"}}}}},
					"commitments": map[string]any{"create": items},
				})
				if err != nil {
					t.Fatal(err)
				}
				c := NewCall(b, "day", args, cites, now)
				if out := c.FileChanges(); c.Err() != nil || !strings.Contains(out, "已收下") {
					t.Fatalf("file_changes = %q, error = %v", out, c.Err())
				}
			}
			a := map[string]any{"exchange": "x1.0", "kind": "mine", "content": "发报价", "refs": []string{"#1.0"}}
			item := map[string]any{"exchange": "x1.0", "kind": "mine", "content": "带样板", "refs": []string{"#1.1"}}
			file([]map[string]any{a, item})
			for id, content := range map[string]string{"C-0001": "预置承诺", "C-0002": "发报价", "C-0003": "带样板"} {
				if m := b.commitment(id); m == nil || m.Content != content {
					t.Fatalf("initial ID %s collided with seed: %+v", id, m)
				}
			}
			if mode == "explicit" {
				item["id"], item["refs"] = "C-0003", []string{"#1.2"}
			}
			fresh := map[string]any{"exchange": "x1.0", "kind": "mine", "content": "寄合同", "refs": []string{"#1.2"}}
			items := []map[string]any{fresh, item, a}
			if mode == "replace" {
				items = []map[string]any{fresh, item}
			}
			file(items)
			for id, content := range map[string]string{"C-0001": "预置承诺", "C-0003": "带样板", "C-0004": "寄合同"} {
				if m := b.commitment(id); m == nil || m.Content != content {
					t.Fatalf("refile changed identity %s: %+v", id, m)
				}
			}
			if mode == "replace" && b.commitment("C-0002") != nil {
				t.Fatal("unmatched replaced commitment remained")
			}
			if mode != "replace" {
				if m := b.commitment("C-0002"); m == nil || m.Content != "发报价" {
					t.Fatal("matched commitment ID changed")
				}
			}
		})
	}
}
