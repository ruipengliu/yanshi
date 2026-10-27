package mcpserver

import (
	"slices"
	"testing"
	"time"
)

func TestCommitmentUserDueKept(t *testing.T) {
	x := newServer(nil)
	x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "给张总发验收方案", Counterpart: "张总", DueDate: "2026-10-28"}}})
	if err := x.Edit("bl", "u", "C-0001", "", "2026-10-30"); err != nil {
		t.Fatal(err)
	}
	st := x.State("bl", "u")
	rev := st.Commitments[0].Rev()
	out := invoke(x, "ses", "file_changes", changes(span("#9.0", "#9.0"), nil, M{"revise": []M{{"id": "C-0001", "rev": rev, "due_date": "2026-10-28", "due_text": "下周三", "refs": []string{"#9.0"}}}}), said("#9.0", "wearer", "16:00"))
	contains(t, "locked due", out, "用户在 App 中改过期限")
	m := x.State("bl", "u").Commitments[0]
	if m.DueDate != "2026-10-30" || !slices.Contains(m.Locked, "due") || m.Rev() != rev {
		t.Fatalf("user edit overwritten: %+v", m)
	}
}

func TestRemindersPreferUserEdit(t *testing.T) {
	x := newServer(nil)
	x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "给张总发验收方案", Counterpart: "张总", DueDate: "2026-10-28"}}})
	if err := x.Edit("bl", "u", "C-0001", "", "2026-10-30"); err != nil {
		t.Fatal(err)
	}
	st := x.State("bl", "u")
	edited := st.Commitments[0]
	last := edited.Revisions[len(edited.Revisions)-1]
	if !last.SpokenAt.IsZero() || last.Author != AuthorUser {
		t.Fatalf("unexpected user revision: %+v", last)
	}
	later := last.RecordedAt.Add(time.Hour)
	duplicate := Commitment{ID: "C-0002", Kind: edited.Kind, Content: edited.Content, Counterpart: edited.Counterpart,
		Status: "open", DueDate: "2026-10-28", Revisions: []Revision{{Author: AuthorDay, SpokenAt: later, RecordedAt: later}}}
	for _, mode := range []string{"edit", "locked only", "author in earlier revision"} {
		t.Run(mode, func(t *testing.T) {
			m := edited
			m.Revisions = slices.Clone(edited.Revisions)
			switch mode {
			case "locked only":
				for i := range m.Revisions {
					m.Revisions[i].Author = AuthorDay
				}
			case "author in earlier revision":
				m.Locked = nil
				m.Revisions = append(m.Revisions, Revision{Author: AuthorDay, RecordedAt: last.RecordedAt})
			}
			state := State{Commitments: []Commitment{m, duplicate}}
			checkReminders(t, &state, []string{edited.ID})
			if got := state.Reminders()[0].DueDate; got != "2026-10-30" {
				t.Fatalf("reminder due = %s", got)
			}
			slices.Reverse(state.Commitments)
			checkReminders(t, &state, []string{edited.ID})
		})
	}
}

func checkReminders(t *testing.T, st *State, want []string) {
	t.Helper()
	var got []string
	for _, m := range st.Reminders() {
		got = append(got, m.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reminders = %v, want %v", got, want)
	}
}
