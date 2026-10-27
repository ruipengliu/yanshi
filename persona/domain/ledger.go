package domain

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// 业务线的知识按实体维护、以变更集写入（docs/design/xinli-model.md，ADR-0042）：变更集是日志，交谈与承诺由它
// 依次叠加得出（fold），不直接修改。重交同一个来源的变更集时原位取代旧的那个，再重新叠加：撤销上一次的全部效果、再应用
// 新的。项目不在变更集里：它的现状、归档与合并由各自的工具与用户的选择直接维护，交谈与承诺只按 ID 引用它。

// 变更集的作者。
const (
	AuthorDay     = "xinli-day"     // 依据原话的整理
	AuthorProject = "xinli-project" // 依据交谈摘要的对账
	AuthorUser    = "user"          // 用户在 App 中的改动
	AuthorSeed    = "seed"          // 评测预置的数据
)

// Changeset 是一次整理交给业务线的一组操作（xinli-model.md §5）。操作里的值在交出时已经解析好（交谈 ID、
// 新建的项目、承诺 ID、核对结果、说话时间），叠加时不再依赖当时的状态。
type Changeset struct {
	ID string `json:"id"`
	// Session 是交出它的 Session：旁听 Session（xinli-day、用户对交谈的确认）或项目 Session（xinli-project）。
	Session string `json:"session,omitempty"`
	// Key 是去重键：同一 Session 中同一个键再交一次，整体取代上一次。
	Key        string    `json:"key"`
	Author     string    `json:"author"`
	Source     Source    `json:"source"`
	RecordedAt time.Time `json:"recorded_at"`

	CreateExchanges []Exchange       `json:"create_exchanges,omitempty"`
	ReviseExchanges []ExchangeRevise `json:"revise_exchanges,omitempty"`
	MergeExchanges  []ExchangeMerge  `json:"merge_exchanges,omitempty"`

	CreateCommitments []Commitment       `json:"create_commitments,omitempty"`
	ReviseCommitments []CommitmentChange `json:"revise_commitments,omitempty"`
	CloseCommitments  []CommitmentChange `json:"close_commitments,omitempty"`
	MergeCommitments  []CommitmentMerge  `json:"merge_commitments,omitempty"`
}

// Source 是变更集的依据：xinli-day 读到的旁听范围 [From, To]，或当天整理（Day）；xinli-project 的对账是某一天（Date）。
type Source struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Day  bool   `json:"day,omitempty"`
	Date string `json:"date,omitempty"`
}

// Exchange 是一段交谈（xinli-model.md §4）：由整理的模型在旁听上划定范围 [From, To]，ID 取自 From。
type Exchange struct {
	ID        string     `json:"id"`
	Session   string     `json:"session"`
	Date      string     `json:"date"`
	From      string     `json:"from,omitempty"`
	To        string     `json:"to,omitempty"`
	FiledAt   time.Time  `json:"filed_at"`
	Title     string     `json:"title"`
	Summary   string     `json:"summary"`
	Projects  []Link     `json:"projects"`
	Decisions []Decision `json:"decisions"`
	Risks     []string   `json:"risks"`
	Awareness []string   `json:"awareness"`
	// MergedInto 非空时这段交谈已被当天整理并入另一段（同一 Session 中的交谈 ID）。
	MergedInto string `json:"merged_into,omitempty"`
	Changeset  string `json:"changeset,omitempty"`
}

// Link 是交谈与一个项目的关联，四档归属见 xinli-captures.md §5。Project 是归入的项目；unsure 时为空直到用户确认，
// Candidate 是可能属于的项目，Proposed 是用户不认同候选时新建的项目名。
type Link struct {
	Match     string `json:"match"`
	Project   string `json:"project,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Proposed  string `json:"proposed,omitempty"`
}

// Decision 是定下的一件事；Project 缺省时随交谈。
type Decision struct {
	Text    string `json:"text"`
	Project string `json:"project,omitempty"`
}

// Live 报告交谈是否仍然有效（没有被并入另一段）。
func (e *Exchange) Live() bool { return e.MergedInto == "" }

// Pending 报告这段交谈是否在"待确认"中：有归属拿不准的关联，用户还没有确认。
func (e *Exchange) Pending() bool {
	return slices.ContainsFunc(e.Projects, func(l Link) bool { return l.Match == "unsure" && l.Project == "" })
}

// InProjects 返回交谈归入的项目 ID（按关联的顺序，不含待确认的）。
func (e *Exchange) InProjects() []string {
	var out []string
	for _, l := range e.Projects {
		if l.Project != "" && !slices.Contains(out, l.Project) {
			out = append(out, l.Project)
		}
	}
	return out
}

// ExchangeRevise 改正一段交谈的标题、摘要或归属（只在当天整理中，以及用户确认待定的归属）；空的字段不改。
type ExchangeRevise struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Projects []Link `json:"projects,omitempty"`
}

// ExchangeMerge 把被停顿切开的几段交谈并入 Into（只在当天整理中）。
type ExchangeMerge struct {
	Into     string   `json:"into"`
	From     []string `json:"from"`
	Title    string   `json:"title,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Projects []Link   `json:"projects,omitempty"`
}

// Commitment 是承诺账本中的一条（xinli-model.md §4）：当前状态由它的修订依次叠加得出。
type Commitment struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // mine：佩戴者答应别人；theirs：别人答应佩戴者
	Content     string   `json:"content"`
	Counterpart string   `json:"counterpart,omitempty"`
	DueDate     string   `json:"due_date,omitempty"` // 期限是截止时刻，不是生效时刻
	DueText     string   `json:"due_text,omitempty"`
	Project     string   `json:"project,omitempty"`
	Refs        []string `json:"refs,omitempty"`
	// Unverified 非空时是没有通过核对的原因：照常展示，但不按期限提醒。
	Unverified string `json:"unverified,omitempty"`
	// Status 是 open、done 或 cancelled；MergedInto 非空时被判定为重复，指向保留的那条，不参与提醒。
	Status     string `json:"status"`
	MergedInto string `json:"merged_into,omitempty"`
	// Exchange 与 Session 是出处（交谈 ID 与它所在的旁听 Session），只用于展示。
	Exchange  string    `json:"exchange,omitempty"`
	Session   string    `json:"session,omitempty"`
	SpokenAt  time.Time `json:"spoken_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// Revisions 是它的修订，第一条是新建；Rev 即修订数。
	Revisions []Revision `json:"revisions"`
	// Locked 是用户在 App 中改过的字段（status、due、content、counterpart）：模型不再改动它们。
	Locked []string `json:"locked,omitempty"`
}

// Rev 是这条承诺的修订序号：模型修订或关闭时带上它看到的值，账本在此之后又被改过就拒绝。
func (m *Commitment) Rev() int { return len(m.Revisions) }

// Open 报告承诺是否未完成且有效（没有被合并）：提醒与召回只看这些。
func (m *Commitment) Open() bool { return m.Status == "open" && m.MergedInto == "" }

// reminderEligible 只检查单条条件；实际会提醒哪些承诺必须由 State.Reminders 进行集合级选择。
func (m *Commitment) reminderEligible() bool {
	return m.Open() && m.Unverified == "" && m.DueDate != ""
}

// possibleDuplicate 与当天整理的提示共用规则；称呼为空时无法确定是同一对方。
func possibleDuplicate(a, b *Commitment) bool {
	return a.Kind == b.Kind && a.Counterpart != "" && a.Counterpart == b.Counterpart && textSimilarity(a.Content, b.Content) >= similarContent
}

// Reminders 返回会按期限提醒的承诺，保持账本顺序，不修改账本或自动合并。
// 可能重复的关系按成对传递分组：即使 A 与 C 不相近，只要 A–B、B–C 相近，也只保留组中最新的一条。
func (s *State) Reminders() []Commitment {
	seen := make([]bool, len(s.Commitments))
	selected := make([]bool, len(s.Commitments))
	for i := range s.Commitments {
		if seen[i] || !s.Commitments[i].reminderEligible() {
			continue
		}
		seen[i] = true
		group := []int{i}
		best := i
		for n := 0; n < len(group); n++ {
			a := group[n]
			if newerCommitment(&s.Commitments[a], &s.Commitments[best]) {
				best = a
			}
			for j := range s.Commitments {
				if !seen[j] && s.Commitments[j].reminderEligible() && possibleDuplicate(&s.Commitments[a], &s.Commitments[j]) {
					seen[j] = true
					group = append(group, j)
				}
			}
		}
		selected[best] = true
	}
	var out []Commitment
	for i, m := range s.Commitments {
		if selected[i] {
			out = append(out, m)
		}
	}
	return out
}

// 用户改过的承诺优先，避免模型后建的重复项使提醒回到旧期限；用户修订没有说话时间，不能因此被视为最旧。
// 同等用户优先级下，按最后修订的说话时间、记录时间、ID 比较；没有说话时间时用记录时间，
// 让用户修改与依据摘要的修订也能按实际发生的时刻比较。没有修订历史的预置值沿用说话时间与更新时间。
func newerCommitment(a, b *Commitment) bool {
	userEdited := func(m *Commitment) bool {
		return len(m.Locked) > 0 || slices.ContainsFunc(m.Revisions, func(r Revision) bool { return r.Author == AuthorUser })
	}
	if au, bu := userEdited(a), userEdited(b); au != bu {
		return au
	}
	latest := func(m *Commitment) (time.Time, time.Time) {
		spoken, recorded := m.SpokenAt, m.UpdatedAt
		if len(m.Revisions) > 0 {
			r := m.Revisions[len(m.Revisions)-1]
			spoken, recorded = r.SpokenAt, r.RecordedAt
		}
		if spoken.IsZero() {
			spoken = recorded
		}
		return spoken, recorded
	}
	as, ar := latest(a)
	bs, br := latest(b)
	if !as.Equal(bs) {
		return as.After(bs)
	}
	if !ar.Equal(br) {
		return ar.After(br)
	}
	return a.ID > b.ID
}

// Revision 是承诺的一次改动（xinli-model.md §3、§7）：说话时间 SpokenAt 起成立，到下一条修订的说话时间为止；
// RecordedAt 是交出的时刻。Basis 是依据：原话（refs）、交谈摘要（exchanges，xinli-project）或用户本人。
type Revision struct {
	Changeset  string            `json:"changeset"`
	Author     string            `json:"author"`
	Op         string            `json:"op"` // create、revise、close、merge
	Fields     map[string]string `json:"fields,omitempty"`
	Refs       []string          `json:"refs,omitempty"`
	Exchanges  []string          `json:"exchanges,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	SpokenAt   time.Time         `json:"spoken_at,omitzero"`
	RecordedAt time.Time         `json:"recorded_at"`
}

// Basis 是给人看的依据："原话"、"摘要"或"用户"。
func (r *Revision) Basis() string {
	switch {
	case r.Author == AuthorUser:
		return "用户"
	case len(r.Exchanges) > 0:
		return "摘要"
	case r.Author == AuthorSeed:
		return "预置"
	}
	return "原话"
}

// CommitmentChange 是对一条承诺的修订（Status 为空）或关闭（Status 为 done、cancelled）：Fields 中是改动的字段
// （due_date、due_text、content、counterpart、project）。
type CommitmentChange struct {
	ID        string            `json:"id"`
	Rev       int               `json:"rev"`
	Status    string            `json:"status,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	Refs      []string          `json:"refs,omitempty"`
	Exchanges []string          `json:"exchanges,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	SpokenAt  time.Time         `json:"spoken_at,omitzero"`
}

// CommitmentMerge 把重复的承诺 From 并入 Into（只在当天整理中）。
type CommitmentMerge struct {
	Into   string   `json:"into"`
	From   []string `json:"from"`
	Reason string   `json:"reason,omitempty"`
}

// ledger 是叠加出的实体。
type ledger struct {
	exchanges   []Exchange
	commitments []Commitment
}

// fold 依次叠加变更集，得出交谈与承诺的当前状态。引用了不存在的实体的操作跳过（被取代的变更集带走了它）；canon 把
// 已合并的项目换成保留的那个。
func fold(css []Changeset, canon func(string) string) ledger {
	var l ledger
	for i := range css {
		l.apply(&css[i])
	}
	for i := range l.exchanges {
		for j := range l.exchanges[i].Projects {
			p := &l.exchanges[i].Projects[j]
			p.Project, p.Candidate = canon(p.Project), canon(p.Candidate)
		}
		for j := range l.exchanges[i].Decisions {
			d := &l.exchanges[i].Decisions[j]
			d.Project = canon(d.Project)
		}
	}
	for i := range l.commitments {
		l.commitments[i].Project = canon(l.commitments[i].Project)
	}
	return l
}

func (l *ledger) apply(cs *Changeset) {
	rev := func(op string, fields map[string]string, refs, exchanges []string, reason string, spoken time.Time) Revision {
		return Revision{Changeset: cs.ID, Author: cs.Author, Op: op, Fields: fields, Refs: refs, Exchanges: exchanges, Reason: reason,
			SpokenAt: spoken, RecordedAt: cs.RecordedAt}
	}
	for _, e := range cs.CreateExchanges {
		e.Changeset = cs.ID
		e.Projects, e.Decisions = slices.Clone(e.Projects), slices.Clone(e.Decisions)
		l.exchanges = append(l.exchanges, e)
	}
	for _, r := range cs.ReviseExchanges {
		if e := l.exchange(cs.Session, r.ID); e != nil {
			e.reviseWith(r.Title, r.Summary, r.Projects)
		}
	}
	for _, m := range cs.MergeExchanges {
		into := l.exchange(cs.Session, m.Into)
		if into == nil || !into.Live() {
			continue
		}
		for _, id := range m.From {
			from := l.exchange(cs.Session, id)
			if from == nil || !from.Live() || from == into {
				continue
			}
			from.MergedInto = into.ID
			if refBefore(from.From, into.From) {
				into.From = from.From
			}
			if refBefore(into.To, from.To) {
				into.To = from.To
			}
			for _, p := range from.Projects {
				if !slices.ContainsFunc(into.Projects, func(q Link) bool { return q == p }) {
					into.Projects = append(into.Projects, p)
				}
			}
			into.Decisions = append(into.Decisions, from.Decisions...)
			into.Risks = append(into.Risks, from.Risks...)
			into.Awareness = append(into.Awareness, from.Awareness...)
		}
		into.reviseWith(m.Title, m.Summary, m.Projects)
	}
	for _, m := range cs.CreateCommitments {
		m.Refs, m.Locked = slices.Clone(m.Refs), nil
		if m.Status == "" {
			m.Status = "open"
		}
		m.UpdatedAt = cs.RecordedAt
		m.Revisions = []Revision{rev("create", nil, m.Refs, nil, "", m.SpokenAt)}
		l.commitments = append(l.commitments, m)
	}
	change := func(c CommitmentChange, op string) {
		m := l.commitment(c.ID)
		if m == nil {
			return
		}
		fields := maps.Clone(c.Fields)
		for k, v := range c.Fields {
			switch k {
			case "due_date":
				m.DueDate = v
			case "due_text":
				m.DueText = v
			case "content":
				m.Content = v
			case "counterpart":
				m.Counterpart = v
			case "project":
				m.Project = v
			}
		}
		if c.Status != "" {
			m.Status = c.Status
			if fields == nil {
				fields = map[string]string{}
			}
			fields["status"] = c.Status
		}
		// 用户改过的状态、期限、内容与对方之后以用户的为准；归属不锁：用户确认待定的归属之后，当天整理仍可以改正它。
		if cs.Author == AuthorUser {
			for _, k := range slices.Sorted(maps.Keys(fields)) {
				switch k {
				case "due_date", "due_text":
					k = "due"
				case "project":
					continue
				}
				if !slices.Contains(m.Locked, k) {
					m.Locked = append(m.Locked, k)
				}
			}
		}
		m.UpdatedAt = cs.RecordedAt
		m.Revisions = append(m.Revisions, rev(op, fields, c.Refs, c.Exchanges, c.Reason, c.SpokenAt))
	}
	for _, c := range cs.ReviseCommitments {
		change(c, "revise")
	}
	for _, c := range cs.CloseCommitments {
		change(c, "close")
	}
	for _, mg := range cs.MergeCommitments {
		if l.commitment(mg.Into) == nil {
			continue
		}
		for _, id := range mg.From {
			if m := l.commitment(id); m != nil && m.ID != mg.Into && m.MergedInto == "" {
				m.MergedInto, m.UpdatedAt = mg.Into, cs.RecordedAt
				m.Revisions = append(m.Revisions, rev("merge", map[string]string{"merged_into": mg.Into}, nil, nil, mg.Reason, time.Time{}))
			}
		}
	}
}

func (e *Exchange) reviseWith(title, summary string, projects []Link) {
	if title != "" {
		e.Title = title
	}
	if summary != "" {
		e.Summary = summary
	}
	if len(projects) > 0 {
		e.Projects = slices.Clone(projects)
	}
}

func (l *ledger) exchange(session, id string) *Exchange {
	if i := slices.IndexFunc(l.exchanges, func(e Exchange) bool { return e.Session == session && e.ID == id }); i >= 0 {
		return &l.exchanges[i]
	}
	return nil
}

func (l *ledger) commitment(id string) *Commitment {
	if i := slices.IndexFunc(l.commitments, func(m Commitment) bool { return m.ID == id }); i >= 0 {
		return &l.commitments[i]
	}
	return nil
}

// refBefore 报告引用编号 a 是否在 b 之前（"#57.3" 先比事件序号、再比段序号）；解析不了的不比较。
func refBefore(a, b string) bool {
	x, ok1 := parseRef(a)
	y, ok2 := parseRef(b)
	return ok1 && ok2 && (x.Seq < y.Seq || (x.Seq == y.Seq && x.Index < y.Index))
}

// exchangeID 是交谈的 ID：取自范围的起点，"#57.0" 为 "x57.0"。
func exchangeID(from string) string { return "x" + strings.TrimPrefix(from, "#") }
