package eval

import (
	"strings"
	"testing"
)

func TestLoadRedoTurns(t *testing.T) {
	for _, extra := range []string{"input: hello", `input: ""`, "input: null", "scheduled: true", "await_background: true", "advance_to_next_schedule: true", "new_session: true", "card_action: {}", "ui_context: {}", "from_device: pc", "captures_file: missing.yaml", "captures: [{speaker: wearer, text: hello}]", "xinli_edit: [{gold: x}]"} {
		t.Run(extra, func(t *testing.T) {
			dir := t.TempDir()
			writeCase(t, dir, "invalid", "name: invalid\nturns:\n  - input: hello\n  - redo: true\n    "+extra+"\n")
			if _, err := LoadSuite(dir); err == nil || !strings.Contains(err.Error(), "redo") {
				t.Fatalf("conflicting redo: %v", err)
			}
		})
	}
	dir := t.TempDir()
	writeCase(t, dir, "first", "name: first\nturns:\n  - redo: true\n")
	if _, err := LoadSuite(dir); err == nil {
		t.Fatal("redo accepted as the first turn")
	}
	dir = t.TempDir()
	writeCase(t, dir, "valid", "name: valid\nturns:\n  - input: hello\n  - redo: true\n")
	cases, err := LoadSuite(dir)
	if err != nil || !cases[0].Turns[1].Redo {
		t.Fatalf("valid redo: %v", err)
	}
}

func TestRedoEvalFlow(t *testing.T) {
	c := &Case{Name: "redo", Agent: "assistant", Trials: 1, Turns: []Turn{
		{Input: "hello", Expect: Expect{ReplyContains: []string{"hello"}}},
		{Redo: true, Expect: Expect{ReplyContains: []string{"hello"}}},
		{Redo: true, Expect: Expect{ReplyContains: []string{"hello"}}},
	}}
	rep, err := Run(t.Context(), scheduleConfig(t), []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cases[0].Passed != rep.Cases[0].Trials {
		t.Fatalf("redo flow: %+v", rep.Cases[0])
	}
}
