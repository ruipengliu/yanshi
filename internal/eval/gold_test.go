package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/eval/xinlifixture"
)

// 一份小金标：上午答应周三交验收方案、下午改到周二；对方答应周一给名单；琐事可记可不记；邻座第三方的话不标。
func testGold() *Gold {
	return &Gold{Commitments: []GoldCommitment{
		{ID: "accept", Kind: "mine", Due: "2026-10-28", Revisions: []GoldRevision{{At: "2026-10-23 15:03", Due: "2026-10-27"}}},
		{ID: "list", Kind: "theirs", Due: "2026-10-26"},
		{ID: "dinner", Kind: "mine", Optional: true},
	}}
}

// 旁听：s/1.0 答应验收、s/1.1 名单、s/1.2 做饭、s/2.0 下午改期。
func testHear(b *goldBook) map[string]string {
	b.hear([]CaptureSegment{
		{Speaker: "wearer", Gold: "accept", At: "2026-10-23 09:30"},
		{Speaker: "other", Gold: "list", At: "2026-10-23 09:31"},
		{Speaker: "wearer", Gold: "dinner", At: "2026-10-23 09:32"},
	}, false)
	b.hear([]CaptureSegment{{Speaker: "wearer", Gold: "accept", At: "2026-10-23 15:03"}}, false)
	return map[string]string{"s/1.0": "accept", "s/1.1": "list", "s/1.2": "dinner", "s/2.0": "accept"}
}

func commitment(id, kind, due, status string, refs ...string) xinlifixture.Commitment {
	return xinlifixture.Commitment{ID: id, Kind: kind, DueDate: due, Status: status, Session: "s", Refs: refs, Content: id}
}

// 改期后旧的那条仍未完成：修订算错，旧期限的提醒也算错；做饭的琐事记下了不算误报；kind 记反的算误报与漏记。
func TestGoldScoreStaleAndKind(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	st := &xinlifixture.State{ReminderIDs: []string{"C2", "C1", "C4"}, Commitments: []xinlifixture.Commitment{
		commitment("C2", "mine", "2026-10-27", "open", "#2.0"),
		commitment("C1", "mine", "2026-10-28", "open", "#1.0"),
		commitment("C3", "mine", "", "open", "#1.2"),
		commitment("C4", "mine", "2026-10-26", "open", "#1.1"), // 对方答应的记成了佩戴者的
		{ID: "C0", Kind: "mine", Content: "预置的", Status: "open"},
	}}
	got, detail := b.score(st, labels)
	want := map[string]float64{
		"commitment_precision": 2.0 / 3, // C1、C2 对上 accept；C4 对不上；C3 是可选的
		"commitment_recall":    0.5,     // list 漏了
		"revision_accuracy":    0,       // accept 有两条未完成
		"due_accuracy":         1,       // 有一条未完成的期限对
		"reminder_precision":   1.0 / 3, // C2 对；C1 旧期限、C4 对不上
		"reminder_recall":      0.5,
	}
	assertMetrics(t, got, want, detail)
	if !strings.Contains(detail, "missed [list]") || !strings.Contains(detail, `"mine C4"`) {
		t.Fatalf("detail: %s", detail)
	}
}

// 旧的那条关闭、新的那条在另一个 Session 中新建并引用改期的原话：修订正确；未核实的承诺不提醒。
func TestGoldScoreRevised(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	old := commitment("C1", "mine", "2026-10-28", "cancelled", "#1.0")
	theirs := commitment("C2", "theirs", "2026-10-26", "open", "#1.1")
	theirs.Unverified = "引用的不是对方的话"
	st := &xinlifixture.State{ReminderIDs: []string{"C3"}, Commitments: []xinlifixture.Commitment{old, commitment("C3", "mine", "2026-10-27", "open", "#2.0"), theirs}}
	got, detail := b.score(st, labels)
	assertMetrics(t, got, map[string]float64{"commitment_precision": 1, "commitment_recall": 1, "revision_accuracy": 1, "due_accuracy": 1,
		"reminder_precision": 1, "reminder_recall": 0.5}, detail)
}

// 修订引用的原话在交出那次修订的变更集的 Session 中：新建时没有引用标了金标的原话，按修订的引用对应。
func TestGoldMatchByRevisionRefs(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	labels["day2/3.0"] = "accept"
	m := commitment("C1", "mine", "2026-10-27", "open", "#9.9")
	m.Revisions = []xinlifixture.Revision{{Op: "create", Refs: []string{"#9.9"}}, {Op: "revise", Changeset: "cs2", Refs: []string{"#3.0"}}}
	st := &xinlifixture.State{Commitments: []xinlifixture.Commitment{m}, Changesets: []xinlifixture.Changeset{{ID: "cs2", Session: "day2"}}}
	if got := b.match(st, labels)["C1"]; got == nil || got.ID != "accept" {
		t.Fatalf("matched %v, want accept", got)
	}
}

// 改动按说话时间生效：只听到上午的旁听时期限还是周三；用户在 App 中的改动直接生效。识别成了别人说的承诺无从判断，不计。
func TestGoldBookTimeline(t *testing.T) {
	b := newGoldBook(testGold())
	b.hear([]CaptureSegment{{Speaker: "wearer", Gold: "accept", At: "2026-10-23 09:30"}}, false)
	if it := b.byID["accept"]; it.due != "2026-10-28" || it.revised || !it.counted() {
		t.Fatalf("morning: %+v", it)
	}
	if b.byID["list"].heard {
		t.Fatal("list is not heard yet")
	}
	b.hear([]CaptureSegment{{Speaker: "other", Gold: "list", At: "2026-10-23 15:10", Heard: "wearer"}}, true)
	if it := b.byID["accept"]; it.due != "2026-10-27" || !it.revised {
		t.Fatalf("afternoon: %+v", it)
	}
	if b.byID["list"].counted() {
		t.Fatal("a commitment heard with the wrong speaker cannot be judged")
	}
	b.edit(&GoldEdit{Gold: "accept", Status: "done"})
	if it := b.byID["accept"]; it.status != "done" {
		t.Fatalf("edited: %+v", it)
	}
}

func assertMetrics(t *testing.T, got, want map[string]float64, detail string) {
	t.Helper()
	for k, w := range want {
		if g, ok := got[k]; !ok || g < w-1e-9 || g > w+1e-9 {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, w)
		}
	}
	if t.Failed() {
		t.Log(detail)
	}
}

// 指标的回归要越过固定幅度与两倍标准误；碎片率越小越好。
func TestCompareMetrics(t *testing.T) {
	report := func(m map[string]Metric) *Report {
		return &Report{Cases: []CaseResult{{Name: "c", PassRate: 1, Metrics: m}}}
	}
	base := report(map[string]Metric{
		"turn 3: commitment_recall": {Mean: 0.8, Std: 0.02, N: 3},
		"turn 3: purity":            {Mean: 0.8, Std: 0.2, N: 3},
		"turn 3: fragmentation":     {Mean: 1.1, Std: 0, N: 3},
	})
	got := Compare(base, report(map[string]Metric{
		"turn 3: commitment_recall": {Mean: 0.65, Std: 0.02, N: 3}, // 下降 0.15，波动小：回归
		"turn 3: purity":            {Mean: 0.65, Std: 0.2, N: 3},  // 下降 0.15，但在波动之内
		"turn 3: fragmentation":     {Mean: 1.3, Std: 0, N: 3},     // 变大：回归
	}))
	if len(got) != 2 || !strings.Contains(got[0], "commitment_recall") || !strings.Contains(got[1], "fragmentation") {
		t.Fatalf("regressions %q", got)
	}
}

// 用例的金标写错时在加载时就发现。
func TestLoadSuiteGold(t *testing.T) {
	gold := "commitments:\n  - {id: accept, kind: mine, due: \"2026-10-28\"}\n"
	for name, tc := range map[string]struct{ gold, captures, expect, err string }{
		"ok":            {gold, "gold: accept", "{gold: {commitment_recall: 0.8}}", ""},
		"new metrics":   {gold, "gold: accept", "{gold: {content_accuracy: 0.8, counterpart_accuracy: 0.8, revision_recall: 0.8}}", ""},
		"semantics":     {"commitments:\n  - {id: accept, kind: mine, who: [宋总, 宋], terms: [[成本, 核算], [菜单]]}\n", "gold: accept", "{}", ""},
		"empty who":     {"commitments:\n  - {id: accept, kind: mine, who: []}\n", "gold: accept", "{}", "who must not be empty"},
		"blank who":     {"commitments:\n  - {id: accept, kind: mine, who: [' ']}\n", "gold: accept", "{}", "non-empty strings"},
		"empty terms":   {"commitments:\n  - {id: accept, kind: mine, terms: []}\n", "gold: accept", "{}", "terms must not be empty"},
		"empty group":   {"commitments:\n  - {id: accept, kind: mine, terms: [[]]}\n", "gold: accept", "{}", "terms group must not be empty"},
		"blank term":    {"commitments:\n  - {id: accept, kind: mine, terms: [['']]}\n", "gold: accept", "{}", "non-empty strings"},
		"unknown label": {gold, "gold: other", "{}", "unknown gold commitment"},
		"never heard":   {gold + "  - {id: list, kind: theirs}\n", "gold: accept", "{}", `"list" is not labelled`},
		"bad metric":    {gold, "gold: accept", "{gold: {recall: 0.8}}", "unknown gold metric"},
		"bad kind":      {"commitments:\n  - {id: accept, kind: ours}\n", "gold: accept", "{}", "must be mine or theirs"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(f, s string) {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			write("data/gold.yaml", tc.gold)
			write("c.yaml", "name: c\ngold: data/gold.yaml\nsetup: {xinli: {}}\nturns:\n  - captures:\n      - {speaker: wearer, text: 好, "+tc.captures+"}\n    input: 整理今天\n    expect: {xinli: "+tc.expect+"}\n")
			_, err := LoadSuite(dir)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
		})
	}
}

// 一句话里答应了两件事：引用同一句的两条承诺分别对应两条金标，而不是都算给第一条；平票时计入的优先于可选的。
func TestGoldMatchTies(t *testing.T) {
	g := &Gold{Commitments: []GoldCommitment{
		{ID: "chat", Kind: "mine", Optional: true},
		{ID: "precheck", Kind: "mine", Due: "2026-10-16"},
		{ID: "recheck", Kind: "mine", Due: "2026-10-21"},
	}}
	b := newGoldBook(g)
	b.hear([]CaptureSegment{{Speaker: "wearer", Gold: "chat precheck recheck", At: "2026-10-13 15:10"}}, false)
	labels := map[string]string{"s/1.0": "chat precheck recheck"}
	st := &xinlifixture.State{Commitments: []xinlifixture.Commitment{
		commitment("C1", "mine", "2026-10-16", "open", "#1.0"),
		commitment("C2", "mine", "2026-10-21", "open", "#1.0"),
	}}
	m := b.match(st, labels)
	if m["C1"].ID != "precheck" || m["C2"].ID != "recheck" {
		t.Fatalf("matched %s, %s", m["C1"].ID, m["C2"].ID)
	}
}

// 他人的话被识别成佩戴者说的：照此记成 mine 的承诺对应到那条无从判断的金标，不算误报。
func TestGoldNoisyKind(t *testing.T) {
	b := newGoldBook(&Gold{Commitments: []GoldCommitment{{ID: "banner", Kind: "theirs"}, {ID: "list", Kind: "theirs"}}})
	b.hear([]CaptureSegment{
		{Speaker: "other", Heard: "wearer", Gold: "banner", At: "2026-10-13 10:10"},
		{Speaker: "other", Gold: "list", At: "2026-10-13 10:11"},
	}, true)
	st := &xinlifixture.State{ReminderIDs: []string{"C1"}, Commitments: []xinlifixture.Commitment{
		commitment("C1", "mine", "2026-10-16", "open", "#1.0"),
		commitment("C2", "theirs", "", "open", "#1.1"),
	}}
	got, detail := b.score(st, map[string]string{"s/1.0": "banner", "s/1.1": "list"})
	assertMetrics(t, got, map[string]float64{"commitment_precision": 1, "commitment_recall": 1}, detail)
}

// 改期修订原来的那条算原位修订；新建一条、关闭旧的，终态正确但不是原位。
func TestGoldRevisionInPlace(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	revised := commitment("C1", "mine", "2026-10-27", "open", "#1.0")
	revised.Revisions = []xinlifixture.Revision{{Op: "create"}, {Op: "revise", Refs: []string{"#2.0"}}}
	got, detail := b.score(&xinlifixture.State{ReminderIDs: []string{"C1"}, Commitments: []xinlifixture.Commitment{revised}}, labels)
	assertMetrics(t, got, map[string]float64{"revision_accuracy": 1, "revision_in_place": 1}, detail)

	old := commitment("C1", "mine", "2026-10-28", "cancelled", "#1.0")
	fresh := commitment("C2", "mine", "2026-10-27", "open", "#2.0")
	fresh.Revisions = []xinlifixture.Revision{{Op: "create"}}
	got, detail = b.score(&xinlifixture.State{ReminderIDs: []string{"C2"}, Commitments: []xinlifixture.Commitment{old, fresh}}, labels)
	assertMetrics(t, got, map[string]float64{"revision_accuracy": 1, "revision_in_place": 0}, detail)
}

// 内容与对象分别核对；关键词组必须在同一条承诺中全部出现，别名只需命中一个。
func TestGoldSemantics(t *testing.T) {
	for name, tc := range map[string]struct {
		contents             []string
		who                  string
		content, counterpart float64
	}{
		"correct":           {[]string{"冬季菜单成本表"}, "宋总", 1, 1},
		"synonym and alias": {[]string{"菜单核算表"}, "宋经理", 1, 1},
		"wrong content":     {[]string{"菜单照片"}, "宋总", 0, 1},
		"wrong counterpart": {[]string{"菜单成本表"}, "马师傅", 1, 0},
		"split groups":      {[]string{"菜单", "成本表"}, "宋总", 0, 1},
		"one complete":      {[]string{"照片", "菜单成本表"}, "宋总", 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			b := newGoldBook(&Gold{Commitments: []GoldCommitment{{ID: "cost", Kind: "mine", Who: []string{"宋总", "宋"}, Terms: [][]string{{"成本", "核算"}, {"菜单"}}}}})
			b.hear([]CaptureSegment{{Gold: "cost"}}, false)
			st := &xinlifixture.State{}
			for _, text := range tc.contents {
				m := commitment(text, "mine", "", "open", "#1.0")
				m.Content, m.Counterpart = text, tc.who
				st.Commitments = append(st.Commitments, m)
			}
			got, detail := b.score(st, map[string]string{"s/1.0": "cost"})
			assertMetrics(t, got, map[string]float64{"content_accuracy": tc.content, "counterpart_accuracy": tc.counterpart, "commitment_precision": 1, "commitment_recall": 1}, detail)
			if tc.content == 0 || tc.counterpart == 0 {
				if !strings.Contains(detail, "cost(got") || !strings.Contains(detail, tc.contents[0]) || !strings.Contains(detail, tc.who) {
					t.Fatal(detail)
				}
			}
		})
	}
}

// 漏抽仍占修订召回率的分母；旧正确率只看已对应的金标，未听到、可选与噪声不计。
func TestGoldRevisionRecallMissing(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	b.edit(&GoldEdit{Gold: "list", Status: "done"})
	b.edit(&GoldEdit{Gold: "dinner", Status: "done"})
	m := commitment("C1", "mine", "2026-10-27", "open", "#1.0")
	got, detail := b.score(&xinlifixture.State{ReminderIDs: []string{"C1"}, Commitments: []xinlifixture.Commitment{m}}, labels)
	assertMetrics(t, got, map[string]float64{"revision_accuracy": 1, "revision_recall": 0.5}, detail)
	got, detail = b.score(&xinlifixture.State{}, labels)
	assertMetrics(t, got, map[string]float64{"revision_recall": 0}, detail)
	if _, ok := got["revision_accuracy"]; ok {
		t.Fatal("missing commitments must not enter revision_accuracy")
	}
	b.byID["list"].unjudgeable = true
	b.byID["accept"].heard = false
	got, detail = b.score(&xinlifixture.State{}, labels)
	if _, ok := got["revision_recall"]; ok {
		t.Fatal(detail)
	}
}

// 没有新字段的旧金标保持原有指标；语义指标无分母时不输出。
func TestGoldLegacyMetrics(t *testing.T) {
	b := newGoldBook(testGold())
	labels := testHear(b)
	st := &xinlifixture.State{ReminderIDs: []string{"C1", "C2"}, Commitments: []xinlifixture.Commitment{
		commitment("C1", "mine", "2026-10-27", "open", "#1.0"),
		commitment("C2", "theirs", "2026-10-26", "open", "#1.1"),
	}}
	got, detail := b.score(st, labels)
	assertMetrics(t, got, map[string]float64{"commitment_precision": 1, "commitment_recall": 1, "revision_accuracy": 1, "revision_in_place": 0, "due_accuracy": 1, "reminder_precision": 1, "reminder_recall": 1}, detail)
	for _, name := range []string{"content_accuracy", "counterpart_accuracy"} {
		if _, ok := got[name]; ok {
			t.Fatalf("unexpected %s", name)
		}
	}
}

func TestGoldSemanticsCounted(t *testing.T) {
	g := testGold()
	for i := range g.Commitments {
		g.Commitments[i].Who = []string{"宋"}
		g.Commitments[i].Terms = [][]string{{"成本"}}
	}
	b := newGoldBook(g)
	labels := testHear(b)
	b.byID["list"].unjudgeable = true
	st := &xinlifixture.State{Commitments: []xinlifixture.Commitment{
		commitment("C2", "theirs", "", "open", "#1.1"),
		commitment("C3", "mine", "", "open", "#1.2"),
	}}
	got, detail := b.score(st, labels)
	for _, name := range []string{"content_accuracy", "counterpart_accuracy"} {
		if _, ok := got[name]; ok {
			t.Fatal(detail)
		}
	}
}

func TestGoldScoreReminderSelection(t *testing.T) {
	b := newGoldBook(&Gold{Commitments: []GoldCommitment{{ID: "plan", Kind: "mine", Due: "2026-10-30"}}})
	b.hear([]CaptureSegment{{Speaker: "wearer", Gold: "plan"}}, false)
	b.edit(&GoldEdit{Gold: "plan", Due: "2026-10-30"})
	old := commitment("C-0001", "mine", "2026-10-28", "open", "#1.0")
	newer := commitment("C-0002", "mine", "2026-10-30", "open", "#2.0")
	old.Content, newer.Content = "给张总发验收方案", "给张总发验收方案"
	old.Counterpart, newer.Counterpart = "张总", "张总"
	st := &xinlifixture.State{ReminderIDs: []string{"C-0002"}, Commitments: []xinlifixture.Commitment{old, newer}}
	labels := map[string]string{"s/1.0": "plan", "s/2.0": "plan"}
	got, detail := b.score(st, labels)
	// 提醒选择来自夹具；评分只核对选中承诺，重复承诺仍使修订正确率为零。
	assertMetrics(t, got, map[string]float64{"reminder_precision": 1, "reminder_recall": 1, "revision_accuracy": 0}, detail)
	st.Commitments[1].DueDate = "2026-10-29"
	got, detail = b.score(st, labels)
	assertMetrics(t, got, map[string]float64{"reminder_precision": 0, "reminder_recall": 0}, detail)
	st.Commitments[1].Status = "done"
	st.ReminderIDs = []string{"C-0001"}
	got, detail = b.score(st, labels)
	assertMetrics(t, got, map[string]float64{"reminder_precision": 0, "reminder_recall": 0}, detail)
}
