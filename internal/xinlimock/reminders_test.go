package xinlimock

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestReminders(t *testing.T) {
	base := Commitment{ID: "C-0001", Kind: "mine", Counterpart: "张总", Content: "发送验收方案", Status: "open", DueDate: "2026-10-28"}
	old := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	base.Revisions = []Revision{{SpokenAt: old, RecordedAt: old}}
	for _, tc := range []struct {
		name   string
		change func(*Commitment)
		want   []string
	}{
		{"newer", func(m *Commitment) {}, []string{"C-0002"}},
		{"kind", func(m *Commitment) { m.Kind = "theirs" }, []string{"C-0001", "C-0002"}},
		{"counterpart", func(m *Commitment) { m.Counterpart = "李总" }, []string{"C-0001", "C-0002"}},
		{"empty counterpart", func(m *Commitment) { m.Counterpart = "" }, []string{"C-0001", "C-0002"}},
		{"done", func(m *Commitment) { m.Status = "done" }, []string{"C-0001"}},
		{"cancelled", func(m *Commitment) { m.Status = "cancelled" }, []string{"C-0001"}},
		{"merged", func(m *Commitment) { m.MergedInto = base.ID }, []string{"C-0001"}},
		{"unverified", func(m *Commitment) { m.Unverified = "未核实" }, []string{"C-0001"}},
		{"no due", func(m *Commitment) { m.DueDate = "" }, []string{"C-0001"}},
		{"late recording of old speech", func(m *Commitment) { m.Revisions[0].SpokenAt = old.Add(-time.Hour) }, []string{"C-0001"}},
		{"missing speech uses newer recording", func(m *Commitment) { m.Revisions[0].SpokenAt = time.Time{} }, []string{"C-0002"}},
		{"missing speech uses older recording", func(m *Commitment) { m.Revisions[0] = Revision{RecordedAt: old.Add(-time.Hour)} }, []string{"C-0001"}},
		{"recorded tie break", func(m *Commitment) { m.Revisions[0].SpokenAt = old }, []string{"C-0002"}},
		{"id tie break", func(m *Commitment) { m.Revisions[0] = base.Revisions[0] }, []string{"C-0002"}},
		{"last revision", func(m *Commitment) {
			m.Revisions = append(m.Revisions, Revision{SpokenAt: old.Add(-time.Hour), RecordedAt: old.Add(2 * time.Hour)})
		}, []string{"C-0001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := base
			next.ID = "C-0002"
			next.Revisions = []Revision{{SpokenAt: old.Add(time.Hour), RecordedAt: old.Add(time.Hour)}}
			tc.change(&next)
			st := State{Commitments: []Commitment{base, next}}
			before, _ := json.Marshal(st)
			checkReminders(t, &st, tc.want)
			after, _ := json.Marshal(st)
			if string(before) != string(after) {
				t.Fatal("reminder selection changed ledger")
			}
			slices.Reverse(st.Commitments)
			slices.Reverse(tc.want)
			checkReminders(t, &st, tc.want)
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

func TestRemindersTransitive(t *testing.T) {
	// 首尾内容不相近，中间一条与两端都相近；中间即使已被更新的一条压住，仍须连接整组。
	st := State{Commitments: []Commitment{
		{ID: "C-0003", Content: "甲乙丙丁戊"},
		{ID: "C-0001", Content: "甲乙丙丁戊己庚辛壬癸"},
		{ID: "C-0002", Content: "己庚辛壬癸"},
	}}
	for i := range st.Commitments {
		m := &st.Commitments[i]
		m.Kind, m.Counterpart, m.Status, m.DueDate = "mine", "张总", "open", "2026-10-28"
	}
	a, b, c := &st.Commitments[0], &st.Commitments[1], &st.Commitments[2]
	if !possibleDuplicate(a, b) || !possibleDuplicate(b, c) || possibleDuplicate(a, c) {
		t.Fatal("invalid transitive fixture")
	}
	checkReminders(t, &st, []string{"C-0003"})
	b.Status = "done"
	checkReminders(t, &st, []string{"C-0003", "C-0002"})
}

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
