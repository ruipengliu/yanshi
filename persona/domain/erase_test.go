package domain

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const eraseDate = "2026-10-27"

func eraseBook(t *testing.T) *Book {
	t.Helper()
	var segs []CaptureSegment
	for i := range 6 {
		g := captureTestSegment(captureTestNow.Add(time.Duration(i-20) * time.Hour))
		g.Text = "原文" + (Ref{Seq: 1, Index: i}).String()
		segs = append(segs, g)
	}
	return captureTestBook(t, segs)
}

func eraseChanges(t *testing.T, b *Book, session, args string) string {
	t.Helper()
	cites := map[string]Citation{}
	for i := range 6 {
		r := "#" + (Ref{Seq: 1, Index: i}).String()
		cites[r] = Citation{Ref: r, Speaker: "wearer"}
	}
	result := NewCall(b, session, json.RawMessage(args), cites, captureTestNow).FileChanges()
	if !strings.HasPrefix(result, "已收下变更集") {
		t.Fatal(result)
	}
	return result
}

func eraseCommit(b *Book, cs Changeset) {
	cs.Key = "test:" + cs.ID
	if cs.Author == "" {
		cs.Author = AuthorDay
	}
	if cs.Session == "" && cs.Author == AuthorDay {
		cs.Session = "s"
	}
	b.commit(&cs, -1, nil)
}

// 直接构造各类依据，隔离 source、refs 与交谈范围三种命中条件。
func TestEraseDirectCascade(t *testing.T) {
	for _, tc := range []struct {
		name string
		cs   Changeset
	}{
		{"source", Changeset{Source: Source{From: "#1.1", To: "#1.3"}}},
		{"create_refs", Changeset{CreateCommitments: []Commitment{{ID: "C-1", Refs: []string{"#1.2"}}}}},
		{"revise_refs", Changeset{ReviseCommitments: []CommitmentChange{{ID: "C-1", Refs: []string{"#1.2"}}}}},
		{"close_refs", Changeset{CloseCommitments: []CommitmentChange{{ID: "C-1", Refs: []string{"#1.2"}}}}},
		{"project_refs", Changeset{Author: AuthorProject, Session: "project", Source: Source{Date: eraseDate}, ReviseCommitments: []CommitmentChange{{ID: "C-1", Refs: []string{"#1.2"}, Exchanges: []string{"unaffected"}}}}},
		{"exchange_range", Changeset{CreateExchanges: []Exchange{{ID: "x1.1", Session: "s", Date: eraseDate, From: "#1.1", To: "#1.3"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := eraseBook(t)
			eraseCommit(b, tc.cs)
			if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err != nil {
				t.Fatal(err)
			}
			if len(b.State().Changesets) != 0 {
				t.Fatal("dependent changeset survived")
			}
		})
	}
}

func TestEraseTransitiveCascadeAndRefold(t *testing.T) {
	b := eraseBook(t)
	b.Seed(SeedData{Projects: []Project{{ID: "P-1", Name: "项目", Status: "保留现状"}}}, captureTestNow)
	for i, id := range []string{"x1.0", "x1.2", "x1.4"} {
		ref := "#" + (Ref{Seq: 1, Index: i * 2}).String()
		eraseCommit(b, Changeset{CreateExchanges: []Exchange{{ID: id, Session: "s", Date: eraseDate, From: ref, To: ref, Title: id}},
			CreateCommitments: []Commitment{{ID: "C-" + id, Content: id, Exchange: id, Session: "s", Refs: []string{ref}}}})
	}
	// 合并与改正同处当天整理；其摘要又被项目对账引用。
	eraseCommit(b, Changeset{Source: Source{Day: true}, MergeExchanges: []ExchangeMerge{{Into: "x1.0", From: []string{"x1.2"}, Summary: "合并摘要"}},
		ReviseExchanges: []ExchangeRevise{{ID: "x1.4", Summary: "当天改正"}}})
	eraseCommit(b, Changeset{Author: AuthorProject, Session: "project", Source: Source{Date: eraseDate},
		ReviseCommitments: []CommitmentChange{{ID: "C-x1.4", Exchanges: []string{"x1.0"}, Fields: map[string]string{"content": "依合并摘要改动"}}}})
	// 另一项目修订依赖同一被撤销变更集改过的交谈，也必须被撤销。
	eraseCommit(b, Changeset{Author: AuthorProject, Session: "project2", Source: Source{Date: eraseDate},
		CloseCommitments: []CommitmentChange{{ID: "C-x1.4", Exchanges: []string{"x1.4"}, Status: "done"}}})
	// 用户改动留在日志：消失的目标跳过，未消失的目标仍生效。
	if err := b.Edit("C-x1.2", "done", "", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if err := b.Edit("C-x1.4", "", "2026-11-01", captureTestNow); err != nil {
		t.Fatal(err)
	}
	eraseCommit(b, Changeset{Author: AuthorUser, Session: "s", ReviseExchanges: []ExchangeRevise{{ID: "x1.2", Title: "用户确认"}}})
	b.days = []Day{{Date: eraseDate, Session: "s", Headline: "当天总览"}}
	b.weeks = []Week{{Session: "week", Suggestions: []string{"保留回顾"}}}
	b.merges = []Merge{{ID: "M-1", Reason: "保留提议"}}
	if _, err := b.Erase(eraseDate, "#1.2", "#1.2", captureTestNow); err != nil {
		t.Fatal(err)
	}
	st := b.State()
	// 撤销当天摘要不撤销未命中原文的创建事实：保留两条创建与三条用户改动。
	// 承诺的 Exchange 只是出处，不能把摘要变化反向传回它自己的创建变更集。
	if len(st.Changesets) != 5 || len(st.Exchanges) != 2 || len(st.Commitments) != 2 || len(st.Days) != 0 {
		t.Fatalf("cascade sizes: changesets=%d exchanges=%d commitments=%d days=%d", len(st.Changesets), len(st.Exchanges), len(st.Commitments), len(st.Days))
	}
	if e := b.exchange("s", "x1.0"); e == nil || e.To != "#1.0" || e.Summary != "" {
		t.Fatal("merge not undone")
	}
	if e := b.exchange("s", "x1.4"); e == nil || e.Summary != "" {
		t.Fatal("revision not undone")
	}
	if m := b.commitment("C-x1.4"); m.Content != "x1.4" || m.Status != "open" || m.DueDate != "2026-11-01" {
		t.Fatal("refold lost user change")
	}
	if len(st.Weeks) != 1 || len(st.Merges) != 1 || st.Projects[0].Status != "保留现状" {
		t.Fatal("independent summaries removed")
	}
	if err := b.BindDay("2026-10-28", "other"); err != nil {
		t.Fatal(err)
	}
	// 相同编号的另一业务日不能影响这天的对象或项目修订。
	eraseCommit(b, Changeset{Session: "other", CreateExchanges: []Exchange{{ID: "x1.4", Session: "other", Date: "2026-10-28", From: "#1.4", To: "#1.4"}}})
	eraseCommit(b, Changeset{Author: AuthorProject, Session: "project3", Source: Source{Date: "2026-10-28"}, ReviseCommitments: []CommitmentChange{{ID: "C-x1.4", Exchanges: []string{"x1.4"}, Fields: map[string]string{"counterpart": "另一日依据"}}}})
	if _, err := b.Erase(eraseDate, "1.4", "1.4", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if b.exchange("other", "x1.4") == nil || !slices.ContainsFunc(b.changesets, func(cs Changeset) bool { return cs.Session == "project3" }) {
		t.Fatal("cross-day cascade")
	}
}

func TestEraseTombstonesAndRebuild(t *testing.T) {
	b := eraseBook(t)
	eraseChanges(t, b, "s", `{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"首段","projects":[{"match":"none"}]}]}}`)
	eraseChanges(t, b, "s", `{"source":{"from":"#1.4","to":"#1.4"},"exchanges":{"create":[{"from":"#1.4","to":"#1.4","title":"末段","projects":[{"match":"none"}]}]}}`)
	task, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if task.Session != "s" || !reflect.DeepEqual(task.Sessions, []string{"s"}) {
		t.Fatal(task)
	}
	if got := NewCall(b, "s", nil, nil, captureTestNow).FileChanges(); got != ErrInactiveSession.Error() {
		t.Fatal(got)
	}
	if c := NewCall(b, "s", nil, nil, captureTestNow); c.FileDay() != ErrInactiveSession.Error() || c.Filed != nil {
		t.Fatal("old file_day accepted")
	}
	if _, err := NewCall(b, "s", nil, nil, captureTestNow).Captures(captureTestOptions()); err != ErrInactiveSession {
		t.Fatal(err)
	}
	if err := b.BindDay(eraseDate, "new"); err != nil {
		t.Fatal(err)
	}
	before := len(b.changesets)
	// 新工作 Session 重交同一来源，旧来源整体取代，交谈仍只有一个。
	eraseChanges(t, b, "new", `{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"首段重交","projects":[{"match":"none"}]}]}}`)
	if len(b.changesets) != before || len(b.exchanges) != 2 || b.exchange("s", "x1.0").Title != "首段重交" {
		t.Fatal("source identity changed")
	}
	for _, args := range []string{
		`{"source":{"from":"#1.2","to":"#1.3"}}`,
		`{"source":{"from":"#1.1","to":"#1.2"}}`,
		`{"source":{"from":"#1.1","to":"#1.3"},"exchanges":{"create":[{"from":"#1.2","to":"#1.3","title":"已擦端点","projects":[{"match":"none"}]}]}}`,
		`{"source":{"from":"#1.1","to":"#1.3"},"commitments":{"create":[{"exchange":"x1.0","kind":"mine","content":"承诺","refs":["#1.2"]}]}}`,
	} {
		cites := map[string]Citation{"#1.2": {Ref: "#1.2", Speaker: "wearer"}, "#1.3": {Ref: "#1.3", Speaker: "wearer"}}
		got := NewCall(b, "new", json.RawMessage(args), cites, captureTestNow).FileChanges()
		if !strings.Contains(got, "不存在") || len(b.changesets) != before {
			t.Fatal("tombstone accepted", got)
		}
	}
	// source 与交谈可以跨空号，但端点必须还存在。
	eraseChanges(t, b, "new", `{"source":{"from":"#1.1","to":"#1.3"},"exchanges":{"create":[{"from":"#1.1","to":"#1.3","title":"跨空号","projects":[{"match":"none"}]}]}}`)
	eraseChanges(t, b, "new", `{"source":{"day":true},"exchanges":{"merge":[{"into":"x1.0","from":["x1.4"],"summary":"跨工作 Session 合并"}]}}`)
	if e := b.exchange("new", "x1.4"); e == nil || e.MergedInto != "x1.0" {
		t.Fatal("merge across sessions failed")
	}
	if err := b.BindDay(eraseDate, "third"); err != nil {
		t.Fatal(err)
	}
	eraseChanges(t, b, "third", `{"source":{"day":true},"exchanges":{"revise":[{"id":"x1.0","title":"取代合并"}]}}`)
	if e := b.exchange("third", "x1.4"); !e.Live() {
		t.Fatal("day replacement failed")
	}
	for _, session := range []string{"third", "fourth"} {
		if err := b.BindDay(eraseDate, session); err != nil {
			t.Fatal(err)
		}
		c := NewCall(b, session, json.RawMessage(`{"day":{"headline":"总览"}}`), nil, captureTestNow)
		if got := c.FileDay(); !strings.Contains(got, "3 段交谈") {
			t.Fatal(got)
		}
	}
	if len(b.days) != 1 || b.days[0].Session != "fourth" {
		t.Fatal("file_day identity changed")
	}
	if !strings.Contains(NewCall(b, "fourth", nil, nil, captureTestNow).DayLedger(), "取代合并") {
		t.Fatal("day ledger lost prior sessions")
	}
	if !strings.Contains(NewCall(b, "fourth", nil, nil, captureTestNow).RecentDays(), "末段") {
		t.Fatal("recent days lost prior sessions")
	}
	for _, cs := range b.changesets {
		if cs.Day != eraseDate {
			t.Fatal("missing business day", cs.ID)
		}
	}
}

func TestEraseProgress(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		minutes                         []int
		from, to, progress, want, first string
		source                          Source
	}{
		{"middle", []int{0, 1, 10, 11, 20}, "1.2", "1.2", "1.4", "1.1", "1.3", Source{}},
		{"unprocessed", []int{0, 1, 10, 11, 20}, "1.3", "1.3", "1.1", "1.1", "1.2", Source{}},
		{"split", []int{0, 4, 8, 12}, "1.1", "1.1", "1.3", "0", "1.0", Source{}},
		// 片段时间不保证单调；删除中间片段后，倒序时间可能使两个时段合并。
		{"merge", []int{0, 10, 1}, "1.1", "1.1", "1.2", "0", "1.0", Source{}},
		{"whole_span", []int{0, 1, 10, 11, 20}, "1.2", "1.3", "1.4", "1.1", "1.4", Source{}},
		{"removed_source", []int{0, 10, 20, 30}, "1.2", "1.2", "1.3", "0", "1.0", Source{From: "#1.0", To: "#1.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var segs []CaptureSegment
			for _, m := range tc.minutes {
				segs = append(segs, captureTestSegment(captureTestNow.Add(time.Duration(m-120)*time.Minute)))
			}
			b := captureTestBook(t, segs)
			day := b.captureDays[eraseDate]
			day.GapMS, day.GapSet, day.Progress = 300000, true, tc.progress
			day.ReadThrough = Ref{Seq: 1, Index: len(segs) - 1}
			if tc.source.From != "" {
				eraseCommit(b, Changeset{Source: tc.source})
			}
			if _, err := b.Erase(eraseDate, tc.from, tc.to, captureTestNow); err != nil {
				t.Fatal(err)
			}
			if day.Progress != tc.want || day.ReadThrough != (Ref{}) {
				t.Fatalf("position %s, want %s", day.Progress, tc.want)
			}
			if err := b.BindDay(eraseDate, "replacement"); err != nil {
				t.Fatal(err)
			}
			p, err := NewCall(b, "replacement", json.RawMessage(`{"after":"new","all":true}`), nil, captureTestNow).Captures(captureTestOptions())
			if err != nil || len(p.Segments) == 0 {
				t.Fatal("rebuild page", p, err)
			}
			if p.Segments[0].Ref != tc.first {
				t.Fatal("wrong restart", p)
			}
		})
	}
}

func TestEraseValidationTasksAndSnapshot(t *testing.T) {
	b := eraseBook(t)
	deadline := captureTestNow.Add(24 * time.Hour)
	if err := b.BindDay(eraseDate, "s", deadline); err != nil {
		t.Fatal(err)
	}
	if err := b.BindDay(eraseDate, "s", deadline.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.BindDay(eraseDate, "second"); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(b)
	for _, pair := range [][2]string{{"1.3", "1.2"}, {"0", "1.2"}, {"1.0", "99.0"}} {
		if _, err := b.Erase(eraseDate, pair[0], pair[1], captureTestNow); err == nil {
			t.Fatal("invalid range accepted")
		}
		after, _ := json.Marshal(b)
		if string(before) != string(after) {
			t.Fatal("invalid erasure mutated book")
		}
	}
	task, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "del_2026-10-27_1" || task.Kind != "erase" || task.Session != "second" || !task.DeleteAt.Equal(deadline) || !reflect.DeepEqual(task.Sessions, []string{"s", "second"}) {
		t.Fatal(task)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "原文1.2") || b.captureDays[eraseDate].containsRef(Ref{Seq: 1, Index: 2}) {
		t.Fatal("erased text survived")
	}
	if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err == nil {
		t.Fatal("erased endpoint accepted")
	}
	if _, err := b.AppendCaptures(eraseDate, "one", []CaptureSegment{{Speaker: "wearer", Text: "试图复用"}}); err == nil {
		t.Fatal("erased batch reused")
	}
	refs, err := b.AppendCaptures(eraseDate, "next", []CaptureSegment{{Speaker: "wearer", Text: "新批次"}})
	if err != nil || refs[0] != "2.0" {
		t.Fatal(refs, err)
	}
	if err := b.FailDeletionTask(task.ID, "delete_timeout", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if err := b.FailDeletionTask(task.ID, "clear_failed", captureTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := b.FailDeletionTask(task.ID, "原文不能存这里", captureTestNow); err == nil {
		t.Fatal("unsafe error accepted")
	}
	if tasks := b.PendingDeletionTasks(); len(tasks) != 1 || tasks[0].Error != "clear_failed" || !tasks[0].FailedAt.Equal(captureTestNow.Add(time.Minute)) {
		t.Fatal(tasks)
	}
	candidates := []MemoryCandidate{{ID: "mem-1", Summary: "供用户选择"}}
	if err := b.CompleteDeletionTask(task.ID, candidates, captureTestNow); err != nil {
		t.Fatal(err)
	}
	candidates[0].Summary = "不应修改存储"
	completed := b.DeletionTasks()
	if err := b.CompleteDeletionTask(task.ID, nil, captureTestNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.FailDeletionTask(task.ID, "late_failure", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if len(b.PendingDeletionTasks()) != 0 || !reflect.DeepEqual(completed, b.DeletionTasks()) {
		t.Fatal("completion is not idempotent")
	}
	if err := b.CompleteDeletionTask("missing", nil, captureTestNow); err == nil {
		t.Fatal("missing task accepted")
	}
	if err := b.FailDeletionTask("missing", "timeout", captureTestNow); err == nil {
		t.Fatal("missing task accepted")
	}
	clone, err := b.Clone()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(b)
	got, _ := json.Marshal(clone)
	if string(want) != string(got) {
		t.Fatal("snapshot lost erasure facts")
	}
	clone.deletionTasks[0].Candidates[0].Summary = "隔离"
	if b.DeletionTasks()[0].Candidates[0].Summary != "供用户选择" {
		t.Fatal("snapshot alias")
	}
	// 旧快照无历史绑定与待办，当前绑定仍须被识别，未绑定变更集仍然无 Day。
	var old Book
	if err := json.Unmarshal([]byte(`{"CaptureDays":{"2026-10-27":{"Session":"old","Progress":"0"}},"Changesets":[{"id":"one","session":"old","author":"xinli-day"},{"id":"two","session":"legacy","author":"xinli-day"}]}`), &old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.captureDays[eraseDate].Sessions, []string{"old"}) || old.changesets[0].Day != eraseDate || old.changesets[1].Day != "" || len(old.DeletionTasks()) != 0 {
		t.Fatal("old snapshot migration")
	}
}

func TestEraseDay(t *testing.T) {
	b := eraseBook(t)
	eraseCommit(b, Changeset{Source: Source{Day: true}})
	eraseCommit(b, Changeset{Source: Source{From: "#1.0", To: "#1.5"}})
	task, err := b.EraseDay(eraseDate, captureTestNow)
	if err != nil || task.Kind != "day" {
		t.Fatal(task, err)
	}
	if len(b.changesets) != 0 || len(b.captureDays[eraseDate].segments()) != 0 || b.CaptureProgress(eraseDate) != "0" {
		t.Fatal("day survived")
	}
	if err := b.BindDay(eraseDate, "rebuilt"); err != ErrDayDeleted {
		t.Fatal(err)
	}
	if _, err := b.AppendCaptures(eraseDate, "new", []CaptureSegment{{Speaker: "wearer", Text: "新原文"}}); err != ErrDayDeleted {
		t.Fatal(err)
	}
	again, err := b.EraseDay(eraseDate, captureTestNow.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(task, again) || len(b.DeletionTasks()) != 1 {
		t.Fatal("day retry changed task")
	}
	// 从未绑定的原文与空业务日也能删除，待办仍可持久化。
	unbound := NewBook()
	if _, err := unbound.AppendCaptures(eraseDate, "one", []CaptureSegment{{Speaker: "wearer", Text: "待擦"}}); err != nil {
		t.Fatal(err)
	}
	if task, err := unbound.Erase(eraseDate, "1.0", "1.0", captureTestNow); err != nil || task.Session != "" || len(task.Sessions) != 0 {
		t.Fatal(task, err)
	}
	if task, err := unbound.EraseDay(eraseDate, captureTestNow); err != nil || task.ID != "del_2026-10-27_2" {
		t.Fatal(task, err)
	}
	empty := NewBook()
	if err := empty.BindDay(eraseDate, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.EraseDay(eraseDate, captureTestNow); err != nil {
		t.Fatal(err)
	}
}

func TestErasedRevisionRefsAndCrossSessionResolve(t *testing.T) {
	b := eraseBook(t)
	b.Seed(SeedData{Projects: []Project{{ID: "P-1", Name: "已有项目"}}}, captureTestNow)
	eraseChanges(t, b, "s", `{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"待确认","projects":[{"match":"unsure","id":"P-1","name":"候选"}]}]},"commitments":{"create":[{"exchange":"x1.0","kind":"mine","content":"交样板","refs":["#1.0"]}]}}`)
	if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if err := b.BindDay(eraseDate, "new"); err != nil {
		t.Fatal(err)
	}
	if err := b.Resolve("new", "x1.0", "P-1", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if m := b.commitment("C-0001"); m.Project != "P-1" || m.Rev() != 2 {
		t.Fatal("cross-session resolution lost commitment")
	}
	for _, op := range []string{"revise", "close"} {
		args := `{"source":{"day":true},"commitments":{"` + op + `":[{"id":"C-0001","rev":2,"content":"改交样板","status":"done","refs":["#1.2"]}]}}`
		before, _ := json.Marshal(b)
		got := NewCall(b, "new", json.RawMessage(args), map[string]Citation{"#1.2": {Ref: "#1.2", Speaker: "wearer"}}, captureTestNow).FileChanges()
		after, _ := json.Marshal(b)
		if !strings.Contains(got, "不存在") || string(before) != string(after) {
			t.Fatal("erased revision accepted", got)
		}
	}
	// 停用多个工作 Session 后，第二次待办仍带完整历史，当前 Session 为空也必须记待办。
	task, err := b.Erase(eraseDate, "1.3", "1.3", captureTestNow)
	if err != nil || task.Session != "new" || !reflect.DeepEqual(task.Sessions, []string{"s", "new"}) {
		t.Fatal(task, err)
	}
	task, err = b.Erase(eraseDate, "1.5", "1.5", captureTestNow)
	if err != nil || task.Session != "" || !reflect.DeepEqual(task.Sessions, []string{"s", "new"}) {
		t.Fatal(task, err)
	}
}

func TestProjectExchangeBusinessDay(t *testing.T) {
	b := eraseBook(t)
	b.Seed(SeedData{Projects: []Project{{ID: "P-1", Name: "项目"}}, Commitments: []SeedCommitment{{ID: "C-1", Kind: "mine", Content: "交样板", Project: "P-1"}}}, captureTestNow)
	b.Bind("P-1", "project")
	if err := b.BindDay("2026-10-26", "yesterday"); err != nil {
		t.Fatal(err)
	}
	eraseCommit(b, Changeset{Session: "yesterday", CreateExchanges: []Exchange{{ID: "x1.0", Session: "yesterday", Date: "2026-10-26", From: "#1.0", To: "#1.0"}}})
	args := json.RawMessage(`{"date":"2026-10-27","revise":[{"id":"C-1","rev":1,"content":"改交样板","exchanges":["x1.0"]}]}`)
	if got := NewCall(b, "project", args, nil, captureTestNow).UpdateCommitments(); !strings.Contains(got, "不存在") {
		t.Fatal("cross-day basis accepted", got)
	}
	eraseCommit(b, Changeset{CreateExchanges: []Exchange{{ID: "x1.0", Session: "s", Date: eraseDate, From: "#1.0", To: "#1.0"}}})
	if got := NewCall(b, "project", args, nil, captureTestNow).UpdateCommitments(); !strings.HasPrefix(got, "已收下") {
		t.Fatal(got)
	}
	if _, err := b.EraseDay(eraseDate, captureTestNow); err != nil {
		t.Fatal(err)
	}
	if b.commitment("C-1").Content != "交样板" || b.exchange("yesterday", "x1.0") == nil {
		t.Fatal("wrong project cascade or cross-day deletion")
	}
}

func TestUnboundIdentityAndUnaffectedFold(t *testing.T) {
	b := eraseBook(t)
	// 未绑定 Session 的同号交谈不与业务日或其他旧 Session 合并。
	for _, session := range []string{"legacy1", "legacy2"} {
		eraseChanges(t, b, session, `{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"旧路径","projects":[{"match":"none"}]}]}}`)
		NewCall(b, session, json.RawMessage(`{"day":{"headline":"旧总览"}}`), nil, captureTestNow).FileDay()
	}
	for _, ref := range []string{"#1.0", "#1.4"} {
		eraseChanges(t, b, "s", `{"source":{"from":"`+ref+`","to":"`+ref+`"},"exchanges":{"create":[{"from":"`+ref+`","to":"`+ref+`","title":"当天","projects":[{"match":"none"}]}]}}`)
	}
	eraseChanges(t, b, "s", `{"source":{"day":true},"exchanges":{"merge":[{"into":"x1.0","from":["x1.4"],"summary":"未受影响的合并"}]}}`)
	if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if b.exchange("legacy1", "x1.0") == nil || b.exchange("legacy2", "x1.0") == nil || len(b.days) != 2 {
		t.Fatal("legacy identity lost")
	}
	if e := b.exchange("s", "x1.4"); e == nil || e.MergedInto != "x1.0" {
		t.Fatal("unaffected merge removed")
	}
	for _, cs := range b.changesets {
		if strings.HasPrefix(cs.Session, "legacy") && cs.Day != "" {
			t.Fatal("legacy assigned business day")
		}
	}
}

func TestBindExistingChangesetsSnapshot(t *testing.T) {
	b := NewBook()
	eraseChanges(t, b, "legacy", `{"source":{"from":"#1.0","to":"#1.0"}}`)
	if b.changesets[0].Day != "" {
		t.Fatal("unbound changeset got a day")
	}
	if err := b.BindDay(eraseDate, "legacy"); err != nil {
		t.Fatal(err)
	}
	clone, err := b.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if b.changesets[0].Day != eraseDate || !reflect.DeepEqual(b.State(), clone.State()) {
		t.Fatal("binding identity depends on snapshot round trip")
	}
}

func TestProjectCannotResubmitErasedRefs(t *testing.T) {
	b := eraseBook(t)
	b.Seed(SeedData{Projects: []Project{{ID: "P-1", Name: "项目"}}, Commitments: []SeedCommitment{{ID: "C-1", Kind: "mine", Content: "交样板", Project: "P-1"}}}, captureTestNow)
	b.Bind("P-1", "project")
	eraseChanges(t, b, "s", `{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"交付","projects":[{"match":"existing","id":"P-1"}]}]}}`)
	if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"date":"2026-10-27","revise":[{"id":"C-1","rev":1,"content":"改交样板","exchanges":["x1.0"],"refs":["#1.2"]}]}`)
	before, _ := json.Marshal(b)
	got := NewCall(b, "project", args, nil, captureTestNow).UpdateCommitments()
	after, _ := json.Marshal(b)
	if !strings.Contains(got, "不存在") || string(before) != string(after) {
		t.Fatal("project bypassed tombstone", got)
	}
}

// 摘要改正被撤销时，只撤去依赖摘要的修订；新建承诺仍以各自的 refs 为据。
func TestEraseSummaryRevisionKeepsOriginalCommitments(t *testing.T) {
	b := eraseBook(t)
	eraseCommit(b, Changeset{
		CreateExchanges:   []Exchange{{ID: "x1.0", Session: "s", Date: eraseDate, From: "#1.0", To: "#1.0", Summary: "原摘要"}},
		CreateCommitments: []Commitment{{ID: "C-1", Exchange: "x1.0", Session: "s", Content: "原承诺", Refs: []string{"#1.0"}}},
	})
	// 同一出处也可被另一条承诺使用，不能因摘要变化把它的独立原文依据一起撤销。
	eraseCommit(b, Changeset{CreateCommitments: []Commitment{{ID: "C-2", Exchange: "x1.0", Session: "s", Content: "另一承诺", Refs: []string{"#1.4"}}}})
	eraseCommit(b, Changeset{
		Source:            Source{Day: true},
		ReviseExchanges:   []ExchangeRevise{{ID: "x1.0", Summary: "改正摘要"}},
		ReviseCommitments: []CommitmentChange{{ID: "C-1", Refs: []string{"#1.2"}, Fields: map[string]string{"content": "原文修订"}}},
	})
	eraseCommit(b, Changeset{Author: AuthorProject, Session: "project", Source: Source{Date: eraseDate},
		ReviseCommitments: []CommitmentChange{{ID: "C-2", Exchanges: []string{"x1.0"}, Fields: map[string]string{"content": "摘要修订"}}}})
	if _, err := b.Erase(eraseDate, "1.2", "1.2", captureTestNow); err != nil {
		t.Fatal(err)
	}
	st := b.State()
	if len(st.Changesets) != 2 || len(st.Exchanges) != 1 || len(st.Commitments) != 2 {
		t.Fatalf("summary change removed creation facts: changesets=%d exchanges=%d commitments=%d", len(st.Changesets), len(st.Exchanges), len(st.Commitments))
	}
	if st.Exchanges[0].Summary != "原摘要" || st.Commitments[0].Content != "原承诺" || st.Commitments[1].Content != "另一承诺" {
		t.Fatal("dependent revisions survived refold")
	}
}
