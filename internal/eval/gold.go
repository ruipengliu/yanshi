package eval

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"yanshi/internal/xinlimock"
)

// 金标（m4-eval.md §13）：旁听数据中每段可以标上它所关于的承诺（CaptureSegment.Gold），用例的金标文件给出这些承诺
// 应有的样子与之后的改动。账本中的承诺按它引用的原话对应到金标，与措辞无关；据此计算承诺的精确率与召回率、修订与
// 期限的正确率、提醒的精确率与召回率。断言只判定通过与否，这些指标记下多次运行的均值与标准差，用于比较。

// Gold 是一个用例的金标文件（Case.GoldFile）。
type Gold struct {
	Commitments []GoldCommitment `yaml:"commitments"`
}

// GoldCommitment 是一条应当记下的承诺。第一段标了它的旁听提交之后才计入（"已经听到"），Revisions 按说话时间生效。
type GoldCommitment struct {
	ID   string `yaml:"id"`
	Kind string `yaml:"kind"` // mine | theirs
	// What 说明是什么事，只用于诊断。
	What string `yaml:"what"`
	// Who 是对象或责任人的别名；Terms 每组是同义词，各组都须命中。缺省时不核对。
	Who   []string   `yaml:"who"`
	Terms [][]string `yaml:"terms"`
	// Due 是期限的日期（YYYY-MM-DD），没有期限时为空。
	Due string `yaml:"due"`
	// Optional 为 true 时记不记都可以（琐事、是否算答应拿不准）：不计入召回，记下了也不算误报。
	Optional  bool           `yaml:"optional"`
	Revisions []GoldRevision `yaml:"revisions"`
}

// GoldRevision 是之后的一次改动：说话时间 At（格式同旁听的 at）起，期限改为 Due，或以 Status（done、cancelled）关闭。
type GoldRevision struct {
	At     string `yaml:"at"`
	Due    string `yaml:"due"`
	Status string `yaml:"status"`
}

// GoldEdit 是用户在 App 中改一条承诺（本轮的旁听与输入之前）：Gold 是金标 ID，评测找到对应的那条未完成的承诺，
// Status 为 done 或 cancelled 时关闭它，否则把期限改为 Due。金标随之改变，之后的整理不应改回去。
type GoldEdit struct {
	Gold   string `yaml:"gold"`
	Status string `yaml:"status"`
	Due    string `yaml:"due"`
}

// goldMetrics 是金标指标的名称，expect.xinli.gold 中可以给出它们的下限。
var goldMetrics = []string{"commitment_precision", "commitment_recall", "revision_accuracy", "revision_in_place", "due_accuracy",
	"reminder_precision", "reminder_recall", "content_accuracy", "counterpart_accuracy", "revision_recall"}

// validate 检查金标文件本身：ID 唯一、kind 与日期合法。
func (g *Gold) validate() error {
	seen := map[string]bool{}
	for _, m := range g.Commitments {
		if m.ID == "" || strings.ContainsAny(m.ID, " \t") || seen[m.ID] {
			return fmt.Errorf("gold commitment id %q missing, invalid or duplicated", m.ID)
		}
		seen[m.ID] = true
		if m.Kind != "mine" && m.Kind != "theirs" {
			return fmt.Errorf("gold %s: kind %q must be mine or theirs", m.ID, m.Kind)
		}
		if m.Who != nil {
			if len(m.Who) == 0 {
				return fmt.Errorf("gold %s: who must not be empty", m.ID)
			}
			for _, alias := range m.Who {
				if strings.TrimSpace(alias) == "" {
					return fmt.Errorf("gold %s: who must contain non-empty strings", m.ID)
				}
			}
		}
		if m.Terms != nil {
			if len(m.Terms) == 0 {
				return fmt.Errorf("gold %s: terms must not be empty", m.ID)
			}
			for _, group := range m.Terms {
				if len(group) == 0 {
					return fmt.Errorf("gold %s: terms group must not be empty", m.ID)
				}
				for _, term := range group {
					if strings.TrimSpace(term) == "" {
						return fmt.Errorf("gold %s: terms must contain non-empty strings", m.ID)
					}
				}
			}
		}
		if err := goldDate(m.Due); err != nil {
			return fmt.Errorf("gold %s: %w", m.ID, err)
		}
		for _, r := range m.Revisions {
			if _, err := captureTime(r.At); err != nil {
				return fmt.Errorf("gold %s: revision at %q: %w", m.ID, r.At, err)
			}
			if err := goldDate(r.Due); err != nil {
				return fmt.Errorf("gold %s: %w", m.ID, err)
			}
			if (r.Due == "") == (r.Status == "") || (r.Status != "" && r.Status != "done" && r.Status != "cancelled") {
				return fmt.Errorf("gold %s: a revision sets either due or status (done, cancelled)", m.ID)
			}
		}
	}
	return nil
}

func goldDate(s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return fmt.Errorf("date %q must be YYYY-MM-DD", s)
	}
	return nil
}

// goldBook 是运行到某一轮时金标的状态：哪些承诺已经听到，按已听到的说话时间生效的改动，以及用户在 App 中的改动。
type goldBook struct {
	items      []*goldItem
	byID       map[string]*goldItem
	heardUntil time.Time
}

type goldItem struct {
	*GoldCommitment
	due, status string
	heard       bool
	// unjudgeable 为 true 时标了它的旁听有说话人识别错误（噪声）：平台以说话人字段为准（ADR-0036），无从要求记对，
	// 不计入任何指标。
	unjudgeable bool
	// applied 是已生效的 Revisions 数；revised 为 true 时有过改动（含用户的）。
	applied int
	revised bool
}

func newGoldBook(g *Gold) *goldBook {
	b := &goldBook{byID: map[string]*goldItem{}}
	for i := range g.Commitments {
		m := &g.Commitments[i]
		it := &goldItem{GoldCommitment: m, due: m.Due, status: "open"}
		b.items = append(b.items, it)
		b.byID[m.ID] = it
	}
	return b
}

// hear 记下本轮提交的旁听：标了的承诺计为已经听到；说话时间不晚于已听到的最晚时刻的改动生效。noisy 为 true 时
// 提交的是识别有误的版本（Case.Noise）。
func (b *goldBook) hear(segs []CaptureSegment, noisy bool) {
	for _, g := range segs {
		for _, id := range strings.Fields(g.Gold) {
			if it := b.byID[id]; it != nil {
				it.heard = true
				it.unjudgeable = it.unjudgeable || (noisy && g.Heard != "" && g.Heard != g.Speaker)
			}
		}
		if at, err := captureTime(g.At); g.At != "" && err == nil && at.After(b.heardUntil) {
			b.heardUntil = at
		}
	}
	for _, it := range b.items {
		for it.applied < len(it.Revisions) {
			r := it.Revisions[it.applied]
			if at, _ := captureTime(r.At); at.After(b.heardUntil) {
				break
			}
			it.set(r.Due, r.Status)
			it.applied++
		}
	}
}

// edit 记下用户在 App 中的改动。
func (b *goldBook) edit(e *GoldEdit) {
	if it := b.byID[e.Gold]; it != nil {
		it.set(e.Due, e.Status)
	}
}

func (it *goldItem) set(due, status string) {
	if due != "" {
		it.due = due
	}
	if status != "" {
		it.status = status
	}
	it.revised = true
}

// counted 报告这条金标是否计入召回与正确率：已经听到、不是可选的、不因噪声无从判断。
func (it *goldItem) counted() bool { return it.heard && !it.Optional && !it.unjudgeable }

// match 把账本中由整理记下的承诺（预置的与被合并的不算）对应到金标，键是承诺 ID，对应不上的为 nil：新建时引用的原话
// （没有时取各次修订引用的）标了哪条 kind 相同的金标就是哪条；无从判断的金标不比较 kind（说话人识别错了，记反是噪声
// 所致）。标了几条时取引用次数最多的；仍相同时（一句话里答应了两件事）先取还没有承诺对应上的，再取计入的，再按金标文件
// 中的顺序。
// 对应只看引用、不核对内容：内容与对象由 score 的独立指标核对。labels 的键是"Session/事件序号.段序号"，值是那段
// 旁听的 Gold；修订引用的原话在交出那次修订的变更集的 Session 中。
func (b *goldBook) match(st *xinlimock.State, labels map[string]string) map[string]*goldItem {
	sessions := map[string]string{}
	for _, cs := range st.Changesets {
		sessions[cs.ID] = cs.Session
	}
	out := map[string]*goldItem{}
	taken := map[*goldItem]int{}
	for _, m := range st.Commitments {
		if m.Session == "" || m.MergedInto != "" {
			continue
		}
		hits := map[*goldItem]int{}
		count := func(sid string, refs []string) {
			for _, r := range refs {
				for _, id := range strings.Fields(labels[sid+"/"+strings.TrimPrefix(r, "#")]) {
					if it := b.byID[id]; it != nil && (it.Kind == m.Kind || it.unjudgeable) {
						hits[it]++
					}
				}
			}
		}
		count(m.Session, m.Refs)
		if len(hits) == 0 {
			for _, r := range m.Revisions {
				if r.Op != "create" {
					count(sessions[r.Changeset], r.Refs)
				}
			}
		}
		var best *goldItem
		for _, it := range b.items {
			if better(it, best, hits, taken) {
				best = it
			}
		}
		out[m.ID] = best
		if best != nil {
			taken[best]++
		}
	}
	return out
}

// better 报告 it 是否比 best 更像这条承诺所对应的金标：引用次数多的；相同时还没有承诺对应上的，再是计入的（而不是可选的）。
func better(it, best *goldItem, hits, taken map[*goldItem]int) bool {
	if hits[it] != hits[best] || best == nil {
		return hits[it] > hits[best]
	}
	if (taken[it] == 0) != (taken[best] == 0) {
		return taken[it] == 0
	}
	return !it.Optional && best.Optional
}

// tally 是一个比例：ok / n。
type tally struct{ n, ok int }

func (t *tally) add(ok bool) {
	t.n++
	if ok {
		t.ok++
	}
}

// score 计算一轮的金标指标（名称见 goldMetrics；分母为 0 的不记），并给出诊断：误报、漏记、状态不对的与错的提醒。
//
//   - 承诺精确率：整理记下的承诺中对应得上金标的比例；对应到可选的或无从判断的金标的不计。
//   - 承诺召回率：计入的金标中至少有一条承诺对应上的比例。
//   - 修订正确率：有过改动且已记下的金标中，当前状态正确的比例：仍未完成的恰有一条对应的未完成承诺、期限相同；已关闭的
//     没有对应的未完成承诺，且有一条以同样的状态关闭。重复的未完成承诺与改期后留下的旧承诺都算错（F1）。
//   - 原位修订率：改动后仍未完成、状态正确的金标中，那条未完成的承诺是修订得来的（修订数大于 1）而不是新建的比例：
//     改期、换人应修订原来的那条（captures-xinli-model.md），新建再关闭旧的虽然终态正确，丢了修订的依据。
//   - 期限正确率：已记下、未完成、有期限的金标中，有对应的未完成承诺期限相同的比例。
//   - 提醒：会按期限提醒的承诺（State.Reminders）中，对应的金标未完成、期限相同（金标没有期限时不比较，如"下周给
//     您答复"）的比例，同一条金标只算第一条；召回率的分母是计入的、未完成、有期限的金标。
func (b *goldBook) score(st *xinlimock.State, labels map[string]string) (map[string]float64, string) {
	matched := b.match(st, labels)
	var precision, recall, revision, revisionRecall, content, counterpart, inPlace, due, remindP, remindR tally
	var badContent, badCounterpart []string
	var falses, missed, wrong, badReminders []string
	byGold := map[*goldItem][]*xinlimock.Commitment{}
	for i := range st.Commitments {
		m := &st.Commitments[i]
		it, ok := matched[m.ID]
		if !ok {
			continue
		}
		if it == nil {
			precision.add(false)
			falses = append(falses, clip(m.Kind+" "+m.Content, 40))
			continue
		}
		byGold[it] = append(byGold[it], m)
		if it.counted() {
			precision.add(true)
		}
	}
	for _, it := range b.items {
		if !it.counted() {
			continue
		}
		ms := byGold[it]
		recall.add(len(ms) > 0)
		if it.status == "open" && it.due != "" {
			remindR.n++
		}
		if len(ms) == 0 {
			missed = append(missed, it.ID)
			if it.revised {
				revisionRecall.add(false)
			}
			continue
		}
		if len(it.Terms) > 0 {
			right := slices.ContainsFunc(ms, func(m *xinlimock.Commitment) bool {
				for _, group := range it.Terms {
					if !containsAlias(m.Content, group) {
						return false
					}
				}
				return true
			})
			content.add(right)
			if !right {
				badContent = append(badContent, semanticDetail(it.ID, ms))
			}
		}
		if len(it.Who) > 0 {
			right := slices.ContainsFunc(ms, func(m *xinlimock.Commitment) bool { return containsAlias(m.Counterpart, it.Who) })
			counterpart.add(right)
			if !right {
				badCounterpart = append(badCounterpart, semanticDetail(it.ID, ms))
			}
		}
		var open []*xinlimock.Commitment
		closed := false
		for _, m := range ms {
			if m.Open() {
				open = append(open, m)
			} else if m.Status == it.status {
				closed = true
			}
		}
		right := len(open) == 0 && closed
		if it.status == "open" {
			right = len(open) == 1 && (it.due == "" || open[0].DueDate == it.due)
		}
		if it.revised && right && it.status == "open" {
			inPlace.add(open[0].Rev() > 1)
		}
		if it.revised {
			revision.add(right)
			revisionRecall.add(right)
			if !right {
				wrong = append(wrong, fmt.Sprintf("%s(want %s %s, got %s)", it.ID, it.status, it.due, states(ms)))
			}
		}
		if it.status == "open" && it.due != "" {
			due.add(slices.ContainsFunc(open, func(m *xinlimock.Commitment) bool { return m.DueDate == it.due }))
		}
	}
	reminded := map[*goldItem]bool{}
	for _, m := range st.Reminders() {
		it, ok := matched[m.ID]
		if !ok || (it != nil && !it.counted()) {
			continue
		}
		right := it != nil && it.status == "open" && (it.due == "" || it.due == m.DueDate) && !reminded[it]
		remindP.add(right)
		if right {
			reminded[it] = true
			if it.due != "" {
				remindR.ok++
			}
		} else {
			badReminders = append(badReminders, clip(m.DueDate+" "+m.Content, 40))
		}
	}
	metrics := map[string]float64{}
	for name, t := range map[string]tally{"commitment_precision": precision, "commitment_recall": recall, "revision_accuracy": revision,
		"content_accuracy": content, "counterpart_accuracy": counterpart, "revision_recall": revisionRecall,
		"revision_in_place": inPlace, "due_accuracy": due, "reminder_precision": remindP, "reminder_recall": remindR} {
		if t.n > 0 {
			metrics[name] = float64(t.ok) / float64(t.n)
		}
	}
	detail := fmt.Sprintf("precision %d/%d (unmatched %q); recall %d/%d (missed %v); revision %d/%d (wrong %v); due %d/%d; reminders %d/%d of %d (wrong %q)",
		precision.ok, precision.n, falses, recall.ok, recall.n, missed, revision.ok, revision.n, wrong, due.ok, due.n, remindP.ok, remindP.n, remindR.n, badReminders)
	detail += fmt.Sprintf("; revision_recall %d/%d; content %d/%d (wrong %v); counterpart %d/%d (wrong %v)",
		revisionRecall.ok, revisionRecall.n, content.ok, content.n, badContent, counterpart.ok, counterpart.n, badCounterpart)
	return metrics, detail
}

// states 列出几条承诺的状态与期限，用于诊断。
func states(ms []*xinlimock.Commitment) string {
	var out []string
	for _, m := range ms {
		s := m.Status
		if !m.Open() && m.Status == "open" {
			s = "merged"
		}
		out = append(out, s+" "+m.DueDate)
	}
	return strings.Join(out, ", ")
}

// containsAlias 只核对确定性的子串，不引入评分模型或措辞推断。
func containsAlias(text string, aliases []string) bool {
	return slices.ContainsFunc(aliases, func(alias string) bool { return strings.Contains(text, alias) })
}

func semanticDetail(id string, ms []*xinlimock.Commitment) string {
	var actual []string
	for _, m := range ms {
		actual = append(actual, fmt.Sprintf("counterpart=%q content=%q", clip(m.Counterpart, 40), clip(m.Content, 40)))
	}
	return id + "(got " + strings.Join(actual, "; ") + ")"
}
