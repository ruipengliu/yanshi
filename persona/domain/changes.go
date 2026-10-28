package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// file_changes 与 update_commitments 交出的变更集，及业务线对它的整体检查（xinli-model.md §5）：不通过就整体
// 拒绝、不记下任何内容，结果写明原因让模型改正后用同一个来源重交。

// strs 是字符串列表，也接受单个字符串：模型偶尔把只有一项的列表写成字符串。
type strs []string

func (s *strs) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*s = strs{one}
		return nil
	}
	var many []string
	err := json.Unmarshal(b, &many)
	*s = many
	return err
}

// decisionIn 是定下的一件事，也接受只有文字的字符串。
type decisionIn Decision

func (d *decisionIn) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil {
		*d = decisionIn{Text: text}
		return nil
	}
	return json.Unmarshal(b, (*Decision)(d))
}

type linkIn struct{ Match, ID, Name string }

type changeIn struct {
	ID          string
	Rev         int
	Status      string
	DueDate     string `json:"due_date"`
	DueText     string `json:"due_text"`
	Content     string
	Counterpart string
	Project     string
	Refs        strs
	Exchanges   strs
	Reason      string
}

type changesIn struct {
	Source struct {
		From, To string
		Day      bool
	}
	Exchanges   exchangeOps
	Commitments commitmentOps
}

type exchangeOps struct {
	Create []struct {
		From, To, Title, Summary string
		Projects                 []linkIn
		Decisions                []decisionIn
		Risks, Awareness         strs
	}
	Revise []struct {
		ID, Title, Summary string
		Projects           []linkIn
	}
	Merge []struct {
		Into           string
		From           strs
		Title, Summary string
		Projects       []linkIn
	}
	// Commitments 是误写进 exchanges 里的承诺：模型常把 commitments 嵌在 exchanges 之内，解析时提到外层，
	// 不然它们会被悄悄丢掉、结果却说没有承诺。
	Commitments *commitmentOps
}

type commitmentOps struct {
	Create []struct {
		ID, Exchange, Kind, Content, Counterpart, Project string
		DueDate                                           string `json:"due_date"`
		DueText                                           string `json:"due_text"`
		Refs                                              strs
	}
	Revise, Close []changeIn
	Merge         []struct {
		Into   string
		From   strs
		Reason string
	}
}

// UnmarshalJSON 也接受只有新建的列表：模型偶尔把 {"create": [...]} 直接写成 [...]。
func (o *exchangeOps) UnmarshalJSON(b []byte) error {
	type plain exchangeOps
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, &o.Create)
	}
	return json.Unmarshal(b, (*plain)(o))
}

func (o *commitmentOps) UnmarshalJSON(b []byte) error {
	type plain commitmentOps
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, &o.Create)
	}
	return json.Unmarshal(b, (*plain)(o))
}

// review 是对一个变更集的检查：base 是不含被取代的那个变更集的状态，问题记在 problems，被忽略的操作记在 notes。
type review struct {
	c        *Call
	author   string
	day      bool // 当天整理："整理今天"中 source 为 {day: true} 的那次
	from, to Ref
	base     ledger
	problems []string
	notes    []string
	// news 是本变更集中新建的项目（名称 → 占位 ID），检查通过后才真正新建。
	news []string
}

func (k *review) fail(format string, args ...any) {
	k.problems = append(k.problems, fmt.Sprintf(format, args...))
}

// replacedNote 说明找不到的交谈或承诺是被重交取代的那个变更集里的：模型常以为重交只是补上几项，而重交是整体取代。
const replacedNote = "：它是这个 source 上次交出的，同一个 source 重交会整体取代上次的变更集，上次的交谈与承诺要一并写上"

func parseRef(s string) (Ref, bool) {
	r, ok := strings.CutPrefix(s, "#")
	if !ok {
		return Ref{}, false
	}
	return ParseRef(r)
}

func refLE(a, b Ref) bool { return a.Seq < b.Seq || (a.Seq == b.Seq && a.Index <= b.Index) }

// refs 检查引用：都要存在于本 Session 的旁听中；白天的变更集只能引用本次读到的范围（作用域，§5）。
func (k *review) refs(what string, refs []string, need bool) {
	if need && len(refs) == 0 {
		k.fail("%s须用 refs 引用依据的原话", what)
	}
	for _, r := range refs {
		ct, ok := k.c.cites[r]
		if !ok || ct.Missing {
			k.fail("%s引用的 %s 不存在：用旁听每行开头的编号", what, r)
			continue
		}
		if ref, ok := parseRef(r); ok && !k.day && (!refLE(k.from, ref) || !refLE(ref, k.to)) {
			k.fail("%s引用的 %s 不在本次读到的范围 #%s–#%s 内：白天的整理只能依据本次读到的原话", what, r, k.from, k.to)
		}
	}
}

// spoken 返回被引用原话中最晚的说话时间（当地时间），没有时为零值。
func (k *review) spoken(refs []string) time.Time {
	var t time.Time
	for _, r := range refs {
		ct := k.c.cites[r]
		if ct.SpokenAt.IsZero() {
			continue
		}
		at := ct.SpokenAt
		if at.After(t) {
			t = at
		}
	}
	return t
}

// due 按说话那天换算期限的说法（§7）：换算出唯一的日期而 due_date 为空时补上；与 due_date 不一致时拒绝并给出换算结果。
func (k *review) due(what, text, date string, at time.Time) string {
	if text == "" || at.IsZero() {
		return date
	}
	want := dueDates(text, at)
	switch {
	case len(want) == 0 || slices.Contains(want, date):
	case date == "" && len(want) == 1:
		return want[0]
	case date != "":
		k.fail("%s的期限「%s」按说话那天（%s %s）换算是 %s，不是 due_date %s", what, text, at.Format(time.DateOnly), weekdayNames[at.Weekday()],
			strings.Join(want, " 或 "), date)
	}
	return date
}

var weekdayNames = [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// health 拒绝健康信息（包括他人的）：它们不写进交谈、决定、风险、觉察与项目名（F3 的兜底）。一次列出全部用词。
func (k *review) health(what string, texts ...string) {
	if ws := healthIn(texts...); len(ws) > 0 {
		k.fail("%s含健康信息（「%s」）：这些字段只写事务，删去关于身心健康的整句（包括他人的），不要换个说法", what, strings.Join(ws, "」「"))
	}
}

// links 解析交谈的归属；new 的项目先记为占位 "new:<名称>"，检查通过后才新建。
func (k *review) links(what string, in []linkIn, title string) []Link {
	u := k.c.u
	var out []Link
	for _, l := range in {
		switch l.Match {
		case "existing":
			if u.project(l.ID) == nil {
				k.fail("%s没有项目 %s：用 find_related 召回的 ID，或改为 new、unsure", what, l.ID)
				continue
			}
			out = append(out, Link{Match: "existing", Project: u.canon(l.ID)})
		case "new":
			name := l.Name
			if name == "" {
				name = title
			}
			k.health(what+"的项目名", name)
			if !slices.Contains(k.news, name) {
				k.news = append(k.news, name)
			}
			out = append(out, Link{Match: "new", Project: "new:" + name})
		case "unsure":
			if u.project(l.ID) == nil {
				k.fail("%s没有项目 %s：unsure 的 id 须是 find_related 召回的候选", what, l.ID)
				continue
			}
			k.health(what+"的项目名", l.Name)
			out = append(out, Link{Match: "unsure", Candidate: u.canon(l.ID), Proposed: l.Name})
		case "none":
			out = append(out, Link{Match: "none"})
		default:
			k.fail("%s的 projects[].match 须是 existing、new、unsure 或 none", what)
		}
	}
	if len(out) == 0 && len(in) == 0 {
		k.fail("%s须写 projects（一次性的事写 [{\"match\":\"none\"}]）", what)
	}
	return out
}

// project 解析承诺或决定所属的项目：项目 ID，或交谈关联的新项目的名称。
func (k *review) project(what, p string, e *Exchange) string {
	u := k.c.u
	if p == "" || p == "none" {
		return ""
	}
	if u.project(p) != nil {
		return u.canon(p)
	}
	if e != nil && slices.ContainsFunc(e.Projects, func(l Link) bool { return l.Project == "new:"+p }) {
		return "new:" + p
	}
	if i := slices.IndexFunc(u.projects, func(q Project) bool { return q.Name == p }); i >= 0 {
		return u.canon(u.projects[i].ID)
	}
	var choices []string
	if e != nil {
		for _, l := range e.Projects {
			if id, ok := strings.CutPrefix(l.Project, "new:"); ok {
				choices = append(choices, "新项目名「"+id+"」")
			} else if l.Project != "" {
				choices = append(choices, l.Project)
			}
		}
	}
	choices = append(choices, "none")
	k.fail("%s的 project %s 不是已有的项目，也不是这段交谈关联的新项目：写 %s 之一", what, p, strings.Join(choices, "、"))
	return ""
}

// change 检查一条修订或关闭：版本、依据、用户优先（§5）。返回 false 时这个操作不记下。
func (k *review) change(in changeIn, closing bool, inProject string) (CommitmentChange, bool) {
	verb := "修订"
	if closing {
		verb = "关闭"
	}
	what := verb + " " + in.ID + " "
	m := k.base.commitment(in.ID)
	switch {
	case m == nil:
		if k.c.u.ledger.commitment(in.ID) != nil {
			k.fail("没有承诺 %s%s", in.ID, replacedNote)
		} else {
			k.fail("没有承诺 %s：用 find_related 或 day_ledger 给出的 ID", in.ID)
		}
		return CommitmentChange{}, false
	case inProject != "" && m.Project != inProject:
		k.fail("%s 不是本项目的承诺", in.ID)
		return CommitmentChange{}, false
	case m.MergedInto != "":
		k.fail("%s 已并入 %s，改动保留的那条", in.ID, m.MergedInto)
		return CommitmentChange{}, false
	case in.Rev != m.Rev():
		k.fail("%s 的 rev 是 %d 而不是 %d（之后又被改过）：现在是「%s」，%s；按现在的状态重新判断", in.ID, m.Rev(), in.Rev,
			commitmentText(m), m.Status)
		return CommitmentChange{}, false
	}
	out := CommitmentChange{ID: in.ID, Rev: in.Rev, Refs: in.Refs, Exchanges: in.Exchanges, Reason: in.Reason, SpokenAt: k.spoken(in.Refs)}
	if k.author == AuthorProject {
		if len(in.Exchanges) == 0 {
			k.fail("%s须用 exchanges 写明依据的交谈 ID", what)
		}
		for _, id := range in.Exchanges {
			if !slices.ContainsFunc(k.base.exchanges, func(e Exchange) bool { return e.ID == id }) {
				k.fail("%s依据的交谈 %s 不存在：用 day_summary 给出的交谈 ID", what, id)
			}
		}
	} else {
		k.refs(what, in.Refs, true)
	}
	locked := func(f string) bool {
		if slices.Contains(m.Locked, f) {
			k.notes = append(k.notes, fmt.Sprintf("%s：用户在 App 中改过%s，这一项以用户的为准，没有改动", in.ID, fieldNames[f]))
			return true
		}
		return false
	}
	if closing {
		if in.Status != "done" && in.Status != "cancelled" {
			k.fail("%s的 status 须是 done 或 cancelled", what)
			return out, false
		}
		if locked("status") {
			return out, false
		}
		if !m.Open() {
			k.fail("%s 已经是 %s", in.ID, m.Status)
			return out, false
		}
		out.Status = in.Status
		return out, true
	}
	if !m.Open() {
		k.fail("%s 已经是 %s，不能修订", in.ID, m.Status)
		return out, false
	}
	f, noted := map[string]string{}, len(k.notes)
	if (in.DueDate != "" || in.DueText != "") && !locked("due") {
		date := in.DueDate
		if date == "" && in.DueText == m.DueText {
			date = m.DueDate
		}
		if date = k.due(what, in.DueText, date, out.SpokenAt); date != "" {
			f["due_date"] = date
		}
		if in.DueText != "" {
			f["due_text"] = in.DueText
		}
	}
	if in.Content != "" && !locked("content") {
		f["content"] = in.Content
	}
	if in.Counterpart != "" && !locked("counterpart") {
		f["counterpart"] = in.Counterpart
	}
	if in.Project != "" {
		if p := k.project(what, in.Project, nil); p != "" && !strings.HasPrefix(p, "new:") {
			f["project"] = p
		}
	}
	// 与现在相同的字段不算改动：对账时照抄了已经改过的期限，不必多出一条修订。
	for key, v := range f {
		if (key == "due_date" && v == m.DueDate) || (key == "due_text" && v == m.DueText) || (key == "content" && v == m.Content) ||
			(key == "counterpart" && v == m.Counterpart) || (key == "project" && v == m.Project) {
			delete(f, key)
		}
	}
	if len(f) == 0 && len(k.notes) == noted && (in.DueDate != "" || in.DueText != "" || in.Content != "" || in.Counterpart != "" || in.Project != "") {
		k.notes = append(k.notes, fmt.Sprintf("%s：与现在相同，没有改动", in.ID))
	}
	if len(f) == 0 {
		if len(k.notes) == noted {
			k.fail("%s没有要改的字段（due_date、due_text、content、counterpart、project）", what)
		}
		return out, false
	}
	out.Fields = f
	return out, true
}

var fieldNames = map[string]string{"status": "状态", "due": "期限", "content": "内容", "counterpart": "对方"}

// fileChanges 是 file_changes：xinli-day 交出一个变更集。
func (c *Call) FileChanges() string {
	var in changesIn
	if err := json.Unmarshal(c.args, &in); err != nil {
		return "参数无效：" + parseErrorText(err)
	}
	if m := in.Exchanges.Commitments; m != nil {
		in.Commitments.Create = append(in.Commitments.Create, m.Create...)
		in.Commitments.Revise = append(in.Commitments.Revise, m.Revise...)
		in.Commitments.Close = append(in.Commitments.Close, m.Close...)
		in.Commitments.Merge = append(in.Commitments.Merge, m.Merge...)
	}
	u := c.u
	// 写了范围的就是按范围交出的：模型偶尔在白天的变更集里也写上 day: true。
	k := &review{c: c, author: AuthorDay, day: in.Source.Day && in.Source.From == ""}
	key := "day"
	src := Source{Day: true}
	if !k.day {
		var ok1, ok2 bool
		k.from, ok1 = parseRef(in.Source.From)
		k.to, ok2 = parseRef(in.Source.To)
		if !ok1 || !ok2 || !refLE(k.from, k.to) {
			return "参数无效：source 须是本次读到的旁听范围 {from, to}（如 {\"from\":\"#57.0\",\"to\":\"#80.2\"}），当天整理为 {\"day\": true}"
		}
		key, src = "from:"+in.Source.From, Source{From: in.Source.From, To: in.Source.To}
	}
	old := slices.IndexFunc(u.changesets, func(cs Changeset) bool { return cs.Session == c.session && cs.Key == key && cs.Author == AuthorDay })
	k.base = u.foldWithout(old)
	now := c.now
	date := now.Format(time.DateOnly)
	if bound, _ := u.boundDay(c.session); bound != "" {
		date = bound
	}
	cs := Changeset{Session: c.session, Key: key, Author: AuthorDay, Source: src, RecordedAt: now}

	if !k.day && (len(in.Exchanges.Merge) > 0 || len(in.Exchanges.Revise) > 0 || len(in.Commitments.Merge) > 0) {
		k.fail("合并与改正交谈只在「整理今天」的当天整理中做（source 为 {\"day\": true}）")
	}
	// 交谈：范围在 source 之内、互不重叠，也不与已交出的交谈重叠。
	type span struct{ from, to Ref }
	var taken []span
	for _, e := range k.base.exchanges {
		f, ok1 := parseRef(e.From)
		t, ok2 := parseRef(e.To)
		if e.Session == c.session && e.Live() && ok1 && ok2 {
			taken = append(taken, span{f, t})
		}
	}
	for _, e := range in.Exchanges.Create {
		id := exchangeID(e.From)
		what := "交谈 " + id + " "
		f, ok1 := parseRef(e.From)
		t, ok2 := parseRef(e.To)
		if !ok1 || !ok2 || !refLE(f, t) {
			k.fail("交谈的范围 {from, to} 须是两个引用编号，from 不晚于 to（如 \"#57.0\"、\"#63.4\"）")
			continue
		}
		k.refs(what+"的范围", []string{e.From, e.To}, false)
		for _, s := range taken {
			if refLE(s.from, t) && refLE(f, s.to) {
				k.fail("%s的范围 %s–%s 与已交出的交谈 x%s 重叠", what, e.From, e.To, s.from)
				break
			}
		}
		taken = append(taken, span{f, t})
		k.health(what, append([]string{e.Title, e.Summary}, append(e.Risks, e.Awareness...)...)...)
		x := Exchange{ID: id, Session: c.session, Date: date, From: e.From, To: e.To, FiledAt: now, Title: e.Title,
			Summary: e.Summary, Risks: e.Risks, Awareness: e.Awareness}
		x.Projects = k.links(what, e.Projects, e.Title)
		for _, d := range e.Decisions {
			k.health(what+"的决定", d.Text)
			x.Decisions = append(x.Decisions, Decision{Text: d.Text, Project: k.project(what+"的决定", d.Project, &x)})
		}
		cs.CreateExchanges = append(cs.CreateExchanges, x)
	}
	exchangeOf := func(id string) *Exchange {
		if i := slices.IndexFunc(cs.CreateExchanges, func(e Exchange) bool { return e.ID == id }); i >= 0 {
			return &cs.CreateExchanges[i]
		}
		if e := k.base.exchange(c.session, id); e != nil && e.Live() {
			return e
		}
		return nil
	}
	for _, r := range in.Exchanges.Revise {
		what := "改正交谈 " + r.ID + " "
		if exchangeOf(r.ID) == nil {
			k.fail("%s：没有这段交谈", what)
			continue
		}
		k.health(what, r.Title, r.Summary)
		rv := ExchangeRevise{ID: r.ID, Title: r.Title, Summary: r.Summary}
		if len(r.Projects) > 0 {
			rv.Projects = k.links(what, r.Projects, r.Title)
		}
		cs.ReviseExchanges = append(cs.ReviseExchanges, rv)
	}
	for _, m := range in.Exchanges.Merge {
		what := "合并交谈到 " + m.Into + " "
		if exchangeOf(m.Into) == nil || len(m.From) == 0 || slices.ContainsFunc(m.From, func(id string) bool { return id == m.Into || exchangeOf(id) == nil }) {
			k.fail("%s：into 与 from 须是今天已交出的不同交谈", what)
			continue
		}
		k.health(what, m.Title, m.Summary)
		mg := ExchangeMerge{Into: m.Into, From: m.From, Title: m.Title, Summary: m.Summary}
		if len(m.Projects) > 0 {
			mg.Projects = k.links(what, m.Projects, m.Title)
		}
		cs.MergeExchanges = append(cs.MergeExchanges, mg)
	}
	// 承诺：新建的引用原话并按说话人核对；项目缺省随交谈，交谈关联了多个项目时必填。
	for i, m := range in.Commitments.Create {
		what := fmt.Sprintf("承诺「%s」", m.Content)
		e := exchangeOf(m.Exchange)
		if e == nil {
			if e := k.c.u.ledger.exchange(c.session, m.Exchange); e != nil && e.Live() {
				k.fail("%s的 exchange %s 不在本变更集中%s", what, m.Exchange, replacedNote)
			} else {
				k.fail("%s的 exchange %s 不是本变更集或今天已交出的交谈", what, m.Exchange)
			}
			continue
		}
		if m.Kind != "mine" && m.Kind != "theirs" {
			k.fail("%s的 kind 须是 mine 或 theirs", what)
		}
		k.refs(what, m.Refs, true)
		cm := Commitment{Kind: m.Kind, Content: m.Content, Counterpart: m.Counterpart, DueText: m.DueText, Refs: m.Refs, Exchange: e.ID,
			Session: c.session, SpokenAt: k.spoken(m.Refs), Unverified: c.verify(m.Kind, m.Refs)}
		cm.DueDate = k.due(what, m.DueText, m.DueDate, cm.SpokenAt)
		if m.Project != "" {
			cm.Project = k.project(what, m.Project, e)
		} else {
			var linked []Link
			for _, l := range e.Projects {
				if l.Match != "none" {
					linked = append(linked, l)
				}
			}
			switch {
			case len(linked) > 1:
				k.fail("%s：交谈 %s 关联了多个项目，承诺须写 project", what, e.ID)
			case len(linked) == 1:
				cm.Project = linked[0].Project // unsure 时为空，用户确认后随交谈归入
			}
		}
		cm.ID = m.ID
		if !isCommitmentID(cm.ID) {
			cm.ID = fmt.Sprintf("#%d", i)
		} // 检查通过后分配
		cs.CreateCommitments = append(cs.CreateCommitments, cm)
	}
	for _, r := range in.Commitments.Revise {
		if ch, ok := k.change(r, false, ""); ok {
			cs.ReviseCommitments = append(cs.ReviseCommitments, ch)
		}
	}
	for _, r := range in.Commitments.Close {
		if ch, ok := k.change(r, true, ""); ok {
			cs.CloseCommitments = append(cs.CloseCommitments, ch)
		}
	}
	for _, m := range in.Commitments.Merge {
		into := k.base.commitment(m.Into)
		ok := into != nil && into.Open() && len(m.From) > 0
		for _, id := range m.From {
			f := k.base.commitment(id)
			ok = ok && f != nil && f.Open() && id != m.Into && f.Kind == into.Kind
		}
		if !ok {
			k.fail("合并承诺到 %s：into 与 from 须是不同的、都未完成的、kind 相同的承诺", m.Into)
			continue
		}
		cs.MergeCommitments = append(cs.MergeCommitments, CommitmentMerge{Into: m.Into, From: m.From, Reason: m.Reason})
	}
	k.matchCommitments(&cs, old)
	if len(k.problems) > 0 {
		return rejected(k.problems)
	}
	u.commit(&cs, old, k.news)
	if !k.day {
		c.advanceCaptures(k.from, k.to)
		c.ack = map[string]string{"stream": "transcript", "from": k.from.String(), "through": k.to.String()}
	}
	return c.receipt(&cs, k.notes)
}

// isCommitmentID 识别业务线分配的 ID；模型自拟的名称只作临时 ID，不要求重交。
func isCommitmentID(id string) bool {
	digits, ok := strings.CutPrefix(id, "C-")
	if !ok || digits == "" {
		return false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// matchCommitments 只沿用双向唯一的对应，避免之后按 ID 重放的修订落到另一件事上。
func (k *review) matchCommitments(cs *Changeset, old int) {
	var prev []Commitment
	if old >= 0 {
		prev = k.c.u.changesets[old].CreateCommitments
	}
	explicit := make([]bool, len(cs.CreateCommitments))
	reserved := make([]bool, len(prev))
	matched := make([]bool, len(prev))
	// 显式旧 ID 优先，不能借创建项引用其他来源的承诺或自行分配 ID。
	for i, m := range cs.CreateCommitments {
		if !isCommitmentID(m.ID) {
			continue
		}
		explicit[i] = true
		j := slices.IndexFunc(prev, func(p Commitment) bool { return p.ID == m.ID })
		if j < 0 {
			k.fail("承诺「%s」的 id %s 不是这个 source 上次新建的承诺：新承诺省略 id，重交时只写上次的 ID", m.Content, m.ID)
			continue
		}
		if reserved[j] {
			matched[j] = false
			k.fail("承诺 %s 的 id 被多个创建项使用：一个旧承诺只能对应一个创建项", m.ID)
			continue
		}
		reserved[j], matched[j] = true, true
	}
	candidates := make([][]int, len(cs.CreateCommitments))
	counts := make([]int, len(prev))
	for i, m := range cs.CreateCommitments {
		if explicit[i] {
			continue
		}
		for j, p := range prev {
			if !reserved[j] && m.Kind == p.Kind && slices.ContainsFunc(m.Refs, func(r string) bool { return slices.Contains(p.Refs, r) }) {
				candidates[i] = append(candidates[i], j)
				counts[j]++
			}
		}
	}
	for i, js := range candidates {
		if len(js) == 1 && counts[js[0]] == 1 {
			cs.CreateCommitments[i].ID = prev[js[0]].ID
			matched[js[0]] = true
		}
	}
	var blocked, previous []string
	for j, p := range prev {
		previous = append(previous, fmt.Sprintf("%s「%s」（%s，refs：%s）", p.ID, p.Content, p.Kind, strings.Join(p.Refs, "、")))
		if !matched[j] && k.c.u.commitmentReferenced(p.ID, old) {
			blocked = append(blocked, p.ID)
		}
	}
	if len(blocked) > 0 {
		k.fail("承诺 %s 已被其他变更集引用，本次无法唯一对应，不能撤去。上次创建项对应：%s。保留这些承诺，以相同 kind 引用同样的原话，或在创建项写明旧 id 后重交", strings.Join(blocked, "、"), strings.Join(previous, "；"))
	}
}

// commitmentReferenced 从变更集查引用，合并的 into 不一定有修订，不能只查当前承诺的修订数。
func (u *Book) commitmentReferenced(id string, except int) bool {
	for i, cs := range u.changesets {
		if i == except {
			continue
		}
		for _, ch := range cs.ReviseCommitments {
			if ch.ID == id {
				return true
			}
		}
		for _, ch := range cs.CloseCommitments {
			if ch.ID == id {
				return true
			}
		}
		for _, mg := range cs.MergeCommitments {
			if mg.Into == id || slices.Contains(mg.From, id) {
				return true
			}
		}
	}
	return false
}

func rejected(problems []string) string {
	return "变更集没有被接受，什么都没有记下：\n- " + strings.Join(problems, "\n- ") + "\n改正后用同一个 source 重交整个变更集。"
}

// commit 分配新建的项目与承诺的 ID，取代同一个键的旧变更集（old ≥ 0 时）或追加，再重新叠加。重交时沿用旧变更集中
// 同名新项目的 ID；承诺的对应在检查时确定，其他变更集可能已经引用了它们。
func (u *Book) commit(cs *Changeset, old int, news []string) {
	ids := map[string]string{}
	if old >= 0 {
		prev := &u.changesets[old]
		for _, e := range prev.CreateExchanges {
			for _, l := range e.Projects {
				if l.Match == "new" {
					if p := u.project(l.Project); p != nil {
						ids["new:"+p.Name] = l.Project
					}
				}
			}
		}
	}
	for _, name := range news {
		if ids["new:"+name] == "" {
			ids["new:"+name] = u.newProject(name)
		}
	}
	resolve := func(p string) string {
		if id, ok := ids[p]; ok {
			return id
		}
		return p
	}
	for i := range cs.CreateExchanges {
		e := &cs.CreateExchanges[i]
		for j := range e.Projects {
			e.Projects[j].Project = resolve(e.Projects[j].Project)
		}
		for j := range e.Decisions {
			e.Decisions[j].Project = resolve(e.Decisions[j].Project)
		}
	}
	for i := range cs.ReviseExchanges {
		for j := range cs.ReviseExchanges[i].Projects {
			cs.ReviseExchanges[i].Projects[j].Project = resolve(cs.ReviseExchanges[i].Projects[j].Project)
		}
	}
	for i := range cs.MergeExchanges {
		for j := range cs.MergeExchanges[i].Projects {
			cs.MergeExchanges[i].Projects[j].Project = resolve(cs.MergeExchanges[i].Projects[j].Project)
		}
	}
	for i := range cs.CreateCommitments {
		m := &cs.CreateCommitments[i]
		m.Project = resolve(m.Project)
		if m.ID == "" || strings.HasPrefix(m.ID, "#") {
			u.nextC++
			m.ID = fmt.Sprintf("C-%04d", u.nextC)
		}
	}
	u.nextCS++
	cs.ID = fmt.Sprintf("CS-%04d", u.nextCS)
	if old >= 0 {
		u.changesets[old] = *cs
	} else {
		u.changesets = append(u.changesets, *cs)
	}
	u.refold()
}

// receipt 是变更集被接受后的结果：逐项写明记下了什么，承诺是否通过核对。
func (c *Call) receipt(cs *Changeset, notes []string) string {
	u := c.u
	var b strings.Builder
	fmt.Fprintf(&b, "已收下变更集 %s", cs.ID)
	switch {
	case cs.Source.Day:
		b.WriteString("（当天整理）")
	case cs.Source.From != "":
		fmt.Fprintf(&b, "（来源 %s–%s）", cs.Source.From, cs.Source.To)
	case cs.Source.Date != "":
		fmt.Fprintf(&b, "（%s 的对账）", cs.Source.Date)
	}
	b.WriteString("：\n")
	for _, e := range cs.CreateExchanges {
		fmt.Fprintf(&b, "- 交谈 %s「%s」：%s\n", e.ID, e.Title, u.where(&e))
	}
	for _, r := range cs.ReviseExchanges {
		if e := u.exchange(cs.Session, r.ID); e != nil {
			fmt.Fprintf(&b, "- 改正交谈 %s：%s\n", r.ID, u.where(e))
		}
	}
	for _, m := range cs.MergeExchanges {
		fmt.Fprintf(&b, "- 交谈 %s 并入 %s\n", strings.Join(m.From, "、"), m.Into)
	}
	unverified := 0
	for _, m := range cs.CreateCommitments {
		state := "已核实"
		if m.Unverified != "" {
			unverified++
			state = "未核实：" + m.Unverified + "，不会按期限提醒"
		}
		fmt.Fprintf(&b, "- 承诺 %s（%s）%s\n", m.ID, state, commitmentText(&m))
	}
	for _, ch := range cs.ReviseCommitments {
		if m := u.commitment(ch.ID); m != nil {
			fmt.Fprintf(&b, "- 修订 %s，现在 rev %d：%s\n", ch.ID, m.Rev(), commitmentText(m))
		}
	}
	for _, ch := range cs.CloseCommitments {
		fmt.Fprintf(&b, "- 关闭 %s（%s）\n", ch.ID, ch.Status)
	}
	for _, m := range cs.MergeCommitments {
		fmt.Fprintf(&b, "- 承诺 %s 并入 %s\n", strings.Join(m.From, "、"), m.Into)
	}
	if n := len(cs.CreateCommitments); n > 0 && unverified == 0 {
		fmt.Fprintf(&b, "新建承诺 %d 条，均已核实。\n", n)
	}
	if len(cs.CreateCommitments)+len(cs.ReviseCommitments)+len(cs.CloseCommitments)+len(cs.MergeCommitments) == 0 {
		b.WriteString("这个变更集没有新建、修订或关闭任何承诺。交谈里用户答应了别人、或别人答应了用户要做的事时，用同一个 source 重交，交谈与承诺写在同一个变更集里。\n")
	}
	if unverified > 0 {
		fmt.Fprintf(&b, "%d 条承诺未核实：检查 kind 与 refs，写错了就改正后用同一个 source 重交整个变更集。\n", unverified)
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "注意：%s\n", n)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// updateCommitments 是 update_commitments：xinli-project 依据交谈摘要对账本项目的承诺，作为一个变更集，键是（项目
// Session，日期），重交时整体取代；受同样的 rev 检查，所以不会用过时的视图覆盖白天依据原话做的修订（§6）。
func (c *Call) UpdateCommitments() string {
	var in struct {
		Date          string
		Revise, Close []changeIn
	}
	if err := json.Unmarshal(c.args, &in); err != nil {
		return "参数无效：" + parseErrorText(err)
	}
	p := c.u.projectOfSession(c.session)
	if p == nil {
		return "这个会话不对应任何项目"
	}
	if _, err := time.Parse(time.DateOnly, in.Date); err != nil {
		return "参数无效：date 须是对账的那一天（YYYY-MM-DD，与 day_summary 相同）"
	}
	u := c.u
	key := "project:" + in.Date
	old := slices.IndexFunc(u.changesets, func(cs Changeset) bool { return cs.Session == c.session && cs.Key == key })
	k := &review{c: c, author: AuthorProject, day: true, base: u.foldWithout(old)}
	cs := Changeset{Session: c.session, Key: key, Author: AuthorProject, Source: Source{Date: in.Date}, RecordedAt: c.now}
	for _, r := range in.Revise {
		if ch, ok := k.change(r, false, p.ID); ok {
			cs.ReviseCommitments = append(cs.ReviseCommitments, ch)
		}
	}
	for _, r := range in.Close {
		if ch, ok := k.change(r, true, p.ID); ok {
			cs.CloseCommitments = append(cs.CloseCommitments, ch)
		}
	}
	if len(k.problems) > 0 {
		return rejected(k.problems)
	}
	u.commit(&cs, old, nil)
	return c.receipt(&cs, k.notes)
}

// parseErrorText 保留模型改正参数所需的类型信息，但不暴露实现的 Go 类型名。
func parseErrorText(err error) string {
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		path := strings.TrimPrefix(mismatch.Field, ".")
		if path == "" {
			path = "某个参数"
		} else {
			path += " "
		}
		return path + "应为" + expectedJSONType(mismatch.Type) + "，收到的是" + actualJSONType(mismatch.Value)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "JSON 格式不正确"
	}
	return "无法解析 JSON 参数"
}

func expectedJSONType(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "另一种类型"
	}
	switch t.Kind() {
	case reflect.String:
		return "字符串"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return "数字"
	case reflect.Bool:
		return "布尔值"
	case reflect.Slice, reflect.Array:
		return "数组"
	case reflect.Map, reflect.Struct:
		return "对象"
	default:
		return "另一种类型"
	}
}

func actualJSONType(value string) string {
	switch value {
	case "object":
		return "对象"
	case "array":
		return "数组"
	case "string":
		return "字符串"
	case "bool":
		return "布尔值"
	default:
		if strings.HasPrefix(value, "number") {
			return "数字"
		}
		return "另一种类型"
	}
}
