package xinlifixture

import (
	"slices"
	"testing"
)

func TestRemindersReadPersonaSelection(t *testing.T) {
	state := State{
		Commitments: []Commitment{{ID: "C1", Status: "done"}, {ID: "C2", Status: "open", DueDate: "2026-10-30"}, {ID: "C3"}},
		ReminderIDs: []string{"C3", "C1", "C1", "missing"},
	}
	var ids []string
	for _, m := range state.Reminders() {
		ids = append(ids, m.ID)
	}
	// 即使本地字段看似不符合提醒条件，也只采信 Persona 的结果；输出沿账本顺序、不重复。
	if !slices.Equal(ids, []string{"C1", "C3"}) {
		t.Fatalf("reminders = %v", ids)
	}
	state.ReminderIDs = []string{}
	if got := state.Reminders(); len(got) != 0 {
		t.Fatalf("empty selection returned %v", got)
	}
}
