package domain

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

// 快照携带领域规则的结果，消费者不再自行推导；没有提醒时也必须输出数组。
func TestBookStateReminderSnapshot(t *testing.T) {
	book := NewBook()
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	check := func(want []string) {
		t.Helper()
		state := book.State()
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Reminders []string `json:"reminders"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Reminders == nil || !slices.Equal(snapshot.Reminders, want) {
			t.Fatalf("snapshot reminders = %v, want %v", snapshot.Reminders, want)
		}
	}
	check([]string{})
	book.Seed(SeedData{Commitments: []SeedCommitment{{ID: "C-1", Kind: "mine", Content: "发报价", DueDate: "2026-10-24"}}}, now)
	check([]string{"C-1"})
	if err := book.Edit("C-1", "done", "", now); err != nil {
		t.Fatal(err)
	}
	check([]string{})
}
