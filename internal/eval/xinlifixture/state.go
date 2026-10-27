// Package xinlifixture 是评测的外部夹具客户端。
// 内核不能 import Persona（ADR-0043），所以在此镜像 JSON 与用例 YAML 的数据形状。
// 类型以 persona/domain 为准；领域规则由外部进程执行，评测只读取结果。
package xinlifixture

import (
	"slices"
	"time"
)

const NewProject = "P-0101"

// Project 是业务线的一个项目。ID、Name、Status 可由评测预置；现状之后由 xinli-project 的 set_status 更新。
type Project struct {
	ID            string    `yaml:"id" json:"id"`
	Name          string    `yaml:"name" json:"name"`
	Status        string    `yaml:"status" json:"status"`
	NextSteps     []string  `yaml:"-" json:"next_steps"`
	OpenQuestions []string  `yaml:"-" json:"open_questions"`
	StatusAt      time.Time `yaml:"-" json:"status_at,omitzero"`
	// Session 是项目的 xinli-project Session（归集时创建）。
	Session string `yaml:"-" json:"session,omitempty"`
	// Archived 为 true 时项目已归档：不再被召回，App 中可以取消归档（xinli-captures.md §7）。
	Archived bool `yaml:"-" json:"archived,omitempty"`
	// MergedInto 非空时项目已并入另一个项目（用户确认了合并提议）：交谈与承诺叠加时换成保留的那个。
	MergedInto string `yaml:"-" json:"merged_into,omitempty"`
}

// Active 报告项目是否参与召回：没有归档，也没有被合并。
func (p *Project) Active() bool { return !p.Archived && p.MergedInto == "" }

// Day 是一次每日整理（file_day）的结果。
type Day struct {
	Date       string    `yaml:"date" json:"date"`
	Session    string    `yaml:"-" json:"session"`
	FiledAt    time.Time `yaml:"-" json:"filed_at"`
	Headline   string    `yaml:"headline" json:"headline"`
	Highlights []string  `yaml:"highlights" json:"highlights"`
	Awareness  Awareness `yaml:"awareness" json:"awareness"`
}

// Awareness 是对用户本人沟通方式的观察（"觉察"页）：不含情绪与身心状态，它们只以 health 类别写入 Memory。
type Awareness struct {
	Habits      []string `yaml:"habits" json:"habits"`
	Suggestions []string `yaml:"suggestions" json:"suggestions"`
}

// State 是一个 EndUser 在业务线中的全部数据，供应用页面展示与评测断言。
type State struct {
	Projects    []Project    `json:"projects"`
	Exchanges   []Exchange   `json:"exchanges"`
	Commitments []Commitment `json:"commitments"`
	Changesets  []Changeset  `json:"changesets"`
	Days        []Day        `json:"days"`
	Weeks       []Week       `json:"weeks"`
	Merges      []Merge      `json:"merges"`
	// ReminderIDs 由 Persona 判定，快照消费者只读取结果；按账本顺序排列。
	ReminderIDs []string `json:"reminders"`
}

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

// Reminders 按账本顺序展开 Persona 给出的提醒 ID，评测不重新判定提醒资格或去重。
func (s *State) Reminders() []Commitment {
	var out []Commitment
	for _, m := range s.Commitments {
		if slices.Contains(s.ReminderIDs, m.ID) {
			out = append(out, m)
		}
	}
	return out
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

// Week 是一次每周回顾交出的觉察（file_week）。
type Week struct {
	Session     string    `json:"session"`
	FiledAt     time.Time `json:"filed_at"`
	Habits      []Habit   `json:"habits"`
	Suggestions []string  `json:"suggestions"`
}

// Habit 是本周至少在两天出现过的沟通习惯。
type Habit struct {
	Habit   string   `json:"habit"`
	Days    []string `json:"days"`
	Example string   `json:"example"`
}

// Merge 是一条合并提议（propose_merge），进"待确认"，由用户确认（§4）。
type Merge struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Into    string `json:"into"`
	From    string `json:"from"`
	Reason  string `json:"reason"`
	// Status 是 pending、accepted 或 rejected。
	Status string `json:"status"`
}

// SeedData 是预置的数据（评测用例的 setup.xinli）。
type SeedData struct {
	Projects []Project `yaml:"projects" json:"projects"`
	// Days 是按日期的摘要：day_summary、recent_days 在没有交出的交谈时返回它，它也可以作为对账的依据（交谈 ID 为
	// "x" 加日期，如 x2026-10-24）。
	Days        map[string]string `yaml:"days" json:"days"`
	Commitments []SeedCommitment  `yaml:"commitments" json:"commitments"`
	// Filed 是已经交出的每日整理（日期、总览与觉察），Exchanges 是已经交出的交谈：每周回顾与重写现状读取它们。
	Filed     []Day          `yaml:"filed" json:"filed"`
	Exchanges []SeedExchange `yaml:"exchanges" json:"exchanges"`
}

// SeedCommitment 是预置的承诺。
type SeedCommitment struct {
	ID          string `yaml:"id" json:"id"`
	Kind        string `yaml:"kind" json:"kind"`
	Content     string `yaml:"content" json:"content"`
	Counterpart string `yaml:"counterpart" json:"counterpart"`
	DueDate     string `yaml:"due_date" json:"due_date"`
	DueText     string `yaml:"due_text" json:"due_text"`
	Project     string `yaml:"project" json:"project"`
	// Status 缺省为 open。
	Status     string    `yaml:"status" json:"status"`
	SpokenAt   time.Time `yaml:"spoken_at" json:"spoken_at"`
	Refs       []string  `yaml:"refs" json:"refs"`
	Unverified string    `yaml:"unverified" json:"unverified"`
	// Revisions 是新建之后的修订，按给定顺序写为变更集；初始字段仍取上面的值。
	Revisions []SeedRevision `yaml:"revisions" json:"revisions"`
}

// SeedRevision 让评测使用与真实写入相同的变更集重放路径，避免直接伪造当前值与历史。
type SeedRevision struct {
	Author     string            `yaml:"author" json:"author"`
	Session    string            `yaml:"session" json:"session"`
	Op         string            `yaml:"op" json:"op"`
	Fields     map[string]string `yaml:"fields" json:"fields"`
	Refs       []string          `yaml:"refs" json:"refs"`
	Exchanges  []string          `yaml:"exchanges" json:"exchanges"`
	Reason     string            `yaml:"reason" json:"reason"`
	SpokenAt   time.Time         `yaml:"spoken_at" json:"spoken_at"`
	RecordedAt time.Time         `yaml:"recorded_at" json:"recorded_at"`
}

// SeedExchange 是预置的交谈（归入一个项目）。
type SeedExchange struct {
	ID        string   `yaml:"id" json:"id"`
	Date      string   `yaml:"date" json:"date"`
	Title     string   `yaml:"title" json:"title"`
	Summary   string   `yaml:"summary" json:"summary"`
	Project   string   `yaml:"project" json:"project"`
	Decisions []string `yaml:"decisions" json:"decisions"`
	Risks     []string `yaml:"risks" json:"risks"`
}
