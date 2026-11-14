package eval

import (
	"testing"

	"yanshi/internal/eval/xinlifixture"
)

func TestXinliNoTextFinalFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*xinlifixture.State)
	}{
		{"exchange title", func(s *xinlifixture.State) { s.Exchanges[0].Title = "失眠" }},
		{"exchange summary", func(s *xinlifixture.State) { s.Exchanges[0].Summary = "失眠" }},
		{"exchange decisions", func(s *xinlifixture.State) { s.Exchanges[0].Decisions = []xinlifixture.Decision{{Text: "失眠"}} }},
		{"exchange risks", func(s *xinlifixture.State) { s.Exchanges[0].Risks = []string{"失眠"} }},
		{"exchange awareness", func(s *xinlifixture.State) { s.Exchanges[0].Awareness = []string{"失眠"} }},
		{"proposed project", func(s *xinlifixture.State) { s.Exchanges[0].Projects = []xinlifixture.Link{{Proposed: "失眠"}} }},
		{"day headline", func(s *xinlifixture.State) { s.Days[0].Headline = "失眠" }},
		{"day highlights", func(s *xinlifixture.State) { s.Days[0].Highlights = []string{"失眠"} }},
		{"day habits", func(s *xinlifixture.State) { s.Days[0].Awareness.Habits = []string{"失眠"} }},
		{"day suggestions", func(s *xinlifixture.State) { s.Days[0].Awareness.Suggestions = []string{"失眠"} }},
		{"project name", func(s *xinlifixture.State) { s.Projects[0].Name = "失眠" }},
		{"project status", func(s *xinlifixture.State) { s.Projects[0].Status = "失眠" }},
		{"project next steps", func(s *xinlifixture.State) { s.Projects[0].NextSteps = []string{"失眠"} }},
		{"project questions", func(s *xinlifixture.State) { s.Projects[0].OpenQuestions = []string{"失眠"} }},
		{"archived project", func(s *xinlifixture.State) { s.Projects[0].Archived, s.Projects[0].Name = true, "失眠" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &xinlifixture.State{Exchanges: []xinlifixture.Exchange{{ID: "x"}}, Days: []xinlifixture.Day{{Date: "2026-10-23"}}, Projects: []xinlifixture.Project{{ID: "P-1"}}}
			tc.set(s)
			var checked, pass bool
			(&instance{}).checkXinli(func(name string, ok bool, _ string) {
				if name == "xinli no_text 失眠" {
					checked, pass = true, ok
				}
			}, &XinliExpect{NoText: []string{"失眠"}}, s)
			if !checked || pass {
				t.Fatal("no_text missed final stored text")
			}
		})
	}
}

func TestXinliNoTextExcludesHistory(t *testing.T) {
	s := &xinlifixture.State{
		Exchanges:   []xinlifixture.Exchange{{ID: "current", Summary: "工作安排"}, {ID: "old", Summary: "失眠", MergedInto: "current"}},
		Projects:    []xinlifixture.Project{{ID: "P-1", Name: "工作"}, {ID: "P-2", Name: "失眠", MergedInto: "P-1"}},
		Changesets:  []xinlifixture.Changeset{{CreateExchanges: []xinlifixture.Exchange{{Summary: "失眠"}}}},
		Commitments: []xinlifixture.Commitment{{Content: "失眠"}},
	}
	for _, e := range []*XinliExpect{{}, {NoText: []string{"失眠"}}} {
		checked := 0
		(&instance{}).checkXinli(func(name string, pass bool, _ string) {
			if name == "xinli no_text 失眠" {
				checked++
				if !pass {
					t.Fatal("historical text failed final-state check")
				}
			}
		}, e, s)
		if checked != len(e.NoText) {
			t.Fatalf("no_text opt-in checked %d assertions", checked)
		}
	}
}

func TestXinliNoTextAllowsRejectedAttempt(t *testing.T) {
	dir := t.TempDir()
	writeCase(t, dir, "rejected-health", `name: rejected-health
agent: day
trials: 1
now: '2026-10-23T18:30:00+08:00'
setup: {xinli: {}}
turns:
  - captures: [{speaker: wearer, text: 先谈工作。, at: '2026-10-23 09:00'}]
    input: 'call mcp_xinli__captures {"all":true}'
  - input: 'call mcp_xinli__file_changes {"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"工作","summary":"失眠","projects":[{"match":"none"}]}]}}'
    expect:
      call_results: [{capability: mcp_xinli__file_changes, contains: 没有被接受}]
      xinli: {no_text: [失眠], exchanges: {max: 0}}
  - input: 'call mcp_xinli__file_changes {"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"工作","summary":"商量工作","projects":[{"match":"none"}]}]}}'
    expect:
      xinli: {no_text: [失眠], exchanges: {min: 1, max: 1}}
`)
	rep := run(t, dir)
	if c := rep.Cases[0]; c.Passed != 1 {
		t.Fatalf("rejected health draft failed final check: %+v", c)
	}
}
