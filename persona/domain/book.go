package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

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

// State 是一个 EndUser 在业务线中的整理数据投影，供应用页面展示与评测断言；旁听原文不进入它。
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

// Book 保存一个 owner 的非原文账本；原文只经所属事务绑定的接缝访问。
type Book struct {
	captures      CaptureAccess
	captureClock  func() time.Time
	captureDays   map[string]*captureDay
	deletionTasks []DeletionTask
	projects      []Project
	// changesets 是变更集的日志，ledger 是由它叠加出的交谈与承诺（每次改动之后重新叠加）。
	changesets []Changeset
	ledger
	days   []Day
	weeks  []Week
	merges []Merge
	// weekSessions 是每周回顾的 Session，按最近七天的起点日期。
	weekSessions map[string]string
	// submitted 是业务线向每个旁听 Session 最近提交的整理输入（"整理交谈"或"整理今天"）：业务线据此只接受"整理今天"
	// 那次 Run 的 file_day（F7）。
	submitted map[string]string
	// next 是新建项目的编号：从 101 开始，与用例预置的项目 ID 区分；nextC 是承诺的编号，nextCS 是变更集的编号。
	next, nextC, nextCS int
}

// NewProject 是新建的第一个项目的 ID；评测用例据此断言之后的整理归入了同一项目。
const NewProject = "P-0101"

const (
	// relatedWindow 是召回的范围：最后一次修订在这么久之内的未完成承诺（xinli-model.md §12）。
	relatedWindow = 30 * 24 * time.Hour
	// maxRelated 是 find_related 一次至多返回的承诺数。
	maxRelated = 10
	// similarContent 是"内容相近"的阈值（字符二元组的 Dice 系数）：当天整理的重复候选。
	similarContent = 0.3
	// relatedTopic 是 find_related 只凭主题（对方不同）召回一条承诺的相似度下限。
	relatedTopic = 0.15
)

// verify 按平台给出的被引用原话的说话人核对承诺人（xinli-captures.md §6）：mine 须引用佩戴者的话，theirs 须引用
// 他人的话。返回未通过的原因，通过时为空。
func (c *Call) verify(kind string, refs []string) string {
	want, who := "wearer", "用户本人"
	if kind == "theirs" {
		want, who = "other", "对方"
	}
	found := false
	for _, r := range refs {
		ct, ok := c.cites[r]
		if ok && !ct.Missing {
			found = true
			if ct.Speaker == want {
				return ""
			}
		}
	}
	if !found {
		return "引用的原话不存在"
	}
	return "引用的原话里没有" + who + "说的话"
}

// findRelated 是 find_related：候选项目按主题召回（生产中用嵌入或全文检索，模拟服务用字符二元组相似度，相同时按
// 项目顺序；归档与已合并的不召回），相关的承诺按对方与主题召回（xinli-model.md §6、§12）。
func (c *Call) FindRelated() string {
	var in struct {
		Query        string
		Counterparts strs
	}
	_ = json.Unmarshal(c.args, &in)
	u := c.u
	var b strings.Builder
	var idx []int
	score := make([]float64, len(u.projects))
	for i := range u.projects {
		if p := &u.projects[i]; p.Active() {
			idx, score[i] = append(idx, i), textSimilarity(in.Query, p.Name+" "+p.Status)
		}
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmpDesc(score[a], score[b]) })
	b.WriteString("候选项目：\n")
	if len(idx) == 0 {
		b.WriteString("（还没有项目）\n")
	}
	for _, i := range idx[:min(5, len(idx))] {
		b.WriteString(u.projectLine(&u.projects[i]))
	}
	// 对方相同的都召回；只是主题相近的要相似到一定程度：字符二元组几乎总有一点重合，不设下限就会把账本整个搬进上下文。
	related := u.related(c.now, func(m *Commitment) float64 {
		s := textSimilarity(in.Query, m.Content+" "+m.Counterpart)
		if counterpartIn(m.Counterpart, in.Counterparts) {
			return s + 1
		}
		if s < relatedTopic {
			return 0
		}
		return s
	}, maxRelated)
	if len(related) == 0 {
		b.WriteString("相关的未完成承诺：没有\n")
		return b.String()
	}
	b.WriteString("相关的未完成承诺（这段里的原话改动了其中某条时，在 file_changes 中 revise 或 close 它，不要新建）：\n")
	for _, m := range related {
		fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
	}
	return b.String()
}

// related 返回最后一次修订在召回范围内的未完成承诺中得分为正的，按得分从高到低，至多 n 条。
func (u *Book) related(now time.Time, score func(*Commitment) float64, n int) []*Commitment {
	var out []*Commitment
	scores := map[string]float64{}
	for i := range u.commitments {
		m := &u.commitments[i]
		if !m.Open() || now.Sub(m.UpdatedAt) > relatedWindow {
			continue
		}
		if s := score(m); s > 0 {
			scores[m.ID] = s
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b *Commitment) int { return cmpDesc(scores[a.ID], scores[b.ID]) })
	return out[:min(n, len(out))]
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// counterpartIn 报告称呼 name 是否与 names 中的某一个相同或互相包含（"黄总"与"黄总监"）。
func counterpartIn(name string, names []string) bool {
	if name == "" {
		return false
	}
	return slices.ContainsFunc(names, func(n string) bool { return n != "" && (strings.Contains(n, name) || strings.Contains(name, n)) })
}

// dayLedger 是 day_ledger：今天（本旁听 Session）的交谈与承诺，之前未完成、与今天的对方或项目相关的承诺，以及确定性的
// 提示：同一对方、内容相近的承诺对，相邻且归属相同的交谈（xinli-model.md §6）。
func (c *Call) DayLedger() string {
	u := c.u
	var b strings.Builder
	var today []Exchange
	for _, e := range u.exchanges {
		if u.sameDay(e.Session, c.session) && e.Live() {
			today = append(today, e)
		}
	}
	slices.SortStableFunc(today, func(a, b Exchange) int {
		switch {
		case refBefore(a.From, b.From):
			return -1
		case refBefore(b.From, a.From):
			return 1
		}
		return 0
	})
	b.WriteString("今天的交谈：\n")
	if len(today) == 0 {
		b.WriteString("（还没有）\n")
	}
	projects, counterparts := map[string]bool{}, []string{}
	for _, e := range today {
		fmt.Fprintf(&b, "%s（%s–%s）%s［%s］：%s\n", e.ID, e.From, e.To, e.Title, u.where(&e), e.Summary)
		var ds []string
		for _, d := range e.Decisions {
			ds = append(ds, d.Text)
		}
		list(&b, "  已定", ds)
		list(&b, "  风险", e.Risks)
		list(&b, "  沟通方式", e.Awareness)
		for _, p := range e.InProjects() {
			projects[p] = true
		}
	}
	var mine []*Commitment
	for i := range u.commitments {
		m := &u.commitments[i]
		if u.sameDay(m.Session, c.session) || slices.ContainsFunc(m.Revisions, func(r Revision) bool { return u.sameDay(u.changesetSession(r.Changeset), c.session) }) {
			mine = append(mine, m)
			if m.Counterpart != "" {
				counterparts = append(counterparts, m.Counterpart)
			}
			if m.Project != "" {
				projects[m.Project] = true
			}
		}
	}
	b.WriteString("今天的承诺：\n")
	if len(mine) == 0 {
		b.WriteString("（还没有）\n")
	}
	for _, m := range mine {
		fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
	}
	earlier := u.related(c.now, func(m *Commitment) float64 {
		if slices.Contains(mine, m) {
			return 0
		}
		if counterpartIn(m.Counterpart, counterparts) || (m.Project != "" && projects[m.Project]) {
			return 1
		}
		return 0
	}, len(u.commitments))
	if len(earlier) > 0 {
		b.WriteString("之前未完成、与今天的对方或项目相关的承诺：\n")
		for _, m := range earlier {
			fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
		}
	}
	var hints []string
	open := slices.DeleteFunc(append(slices.Clone(mine), earlier...), func(m *Commitment) bool { return !m.Open() })
	for i, a := range open {
		for _, d := range open[i+1:] {
			if possibleDuplicate(a, d) {
				hints = append(hints, fmt.Sprintf("可能重复的承诺：%s 与 %s（同一对方「%s」、内容相近）", a.ID, d.ID, a.Counterpart))
			}
		}
	}
	for i := 1; i < len(today); i++ {
		a, d := today[i-1].InProjects(), today[i].InProjects()
		if len(a) > 0 && slices.Equal(a, d) {
			hints = append(hints, fmt.Sprintf("相邻且归属相同的交谈：%s 与 %s（都归入 %s）", today[i-1].ID, today[i].ID, strings.Join(a, "、")))
		}
	}
	if len(hints) > 0 {
		b.WriteString("提示（按规则找出的候选，判断之后再决定；拿不准就不动作）：\n")
		for _, h := range hints {
			fmt.Fprintf(&b, "- %s\n", h)
		}
	}
	return b.String()
}

func (c *Call) FileDay() string {
	if err := c.u.checkWorkingSession(c.session); err != nil {
		return err.Error()
	}
	if strings.HasPrefix(c.u.submitted[c.session], "整理交谈") {
		return "没有收下：白天的\"整理交谈\"不交当天总览，当天总览只在\"整理今天\"中交出"
	}
	var in struct {
		Day struct {
			Headline   string
			Highlights []string
		}
		Awareness Awareness
	}
	_ = json.Unmarshal(c.args, &in)
	if ws := healthIn(append(append([]string{in.Day.Headline}, in.Day.Highlights...), append(in.Awareness.Habits, in.Awareness.Suggestions...)...)...); len(ws) > 0 {
		return fmt.Sprintf("没有收下：当天总览与觉察含健康信息（「%s」）。这里只写事务与沟通方式，删去关于身心健康的整句（包括他人的），不要换个说法，再交一次", strings.Join(ws, "」「"))
	}
	now := c.now
	date := now.Format(time.DateOnly)
	// 整理可能拖到次日，业务日由工作 Session 的绑定决定，不随提交时钟漂移。
	if bound, _ := c.u.boundDay(c.session); bound != "" {
		date = bound
	}
	day := Day{Date: date, Session: c.session, FiledAt: now, Headline: in.Day.Headline,
		Highlights: in.Day.Highlights, Awareness: in.Awareness}
	// 重建工作 Session 后仍取代同一业务日的总览，未绑定时沿用 Session。
	if c.session != "" {
		c.u.days = slices.DeleteFunc(c.u.days, func(d Day) bool { return c.u.sameDay(d.Session, c.session) })
	}
	c.u.days = append(c.u.days, day)
	var touched []string
	n := 0
	for _, e := range c.u.exchanges {
		if c.u.sameDay(e.Session, c.session) && e.Live() {
			n++
			for _, p := range e.InProjects() {
				if !slices.Contains(touched, p) {
					touched = append(touched, p)
				}
			}
		}
	}
	c.Filed = &DayFiled{Session: c.session, Date: day.Date, Projects: touched}
	// 总要有确认：没有交谈（当天只有闲聊）时返回空文字，模型会以为没有交上、一再重交。
	return fmt.Sprintf("已收到 %s 的整理（%d 段交谈）", day.Date, n)
}

// Submitted 记下业务线向旁听 Session 提交了哪种整理输入（"整理交谈"或"整理今天"）：业务线清楚自己提交的是什么，只接受
// "整理今天"那次 Run 的 file_day（xinli-model.md §6，F7）。没有记下时不限制（调试控制台中随手提交的输入）。
func (u *Book) Submitted(session, input string) {
	u.submitted[session] = input
}

// Resolve 是用户在 App 的"待确认"中的选择（xinli-captures.md §5）：把旁听 Session daySession 中拿不准归属的交谈
// exchange 归入项目 project；project 为空时按交谈给出的名称新建。交谈中还没有归属的承诺随之归入项目。它记为用户的变更集。
func (u *Book) Resolve(daySession, exchange, project string, now time.Time) error {
	e := u.exchange(daySession, exchange)
	if e == nil || !e.Pending() {
		return fmt.Errorf("exchange %s of %s is not pending", exchange, daySession)
	}
	links := slices.Clone(e.Projects)
	i := slices.IndexFunc(links, func(l Link) bool { return l.Match == "unsure" && l.Project == "" })
	if project == "" {
		name := links[i].Proposed
		if name == "" {
			name = e.Title
		}
		project = u.newProject(name)
	} else if u.project(project) == nil {
		return fmt.Errorf("no project %s", project)
	}
	links[i].Project = project
	cs := Changeset{Session: daySession, Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: now,
		ReviseExchanges: []ExchangeRevise{{ID: exchange, Projects: links}}}
	for _, m := range u.commitments {
		if u.sameDay(m.Session, daySession) && m.Exchange == exchange && m.Project == "" {
			cs.ReviseCommitments = append(cs.ReviseCommitments, CommitmentChange{ID: m.ID, Rev: m.Rev(), Fields: map[string]string{"project": project}})
		}
	}
	u.commit(&cs, -1, nil)
	return nil
}

// Edit 是用户在 App 中改一条承诺：status 非空时标为完成或取消，否则把期限改为 dueDate。它记为用户的变更集，改过的
// 字段之后以用户的为准，模型不再改动（xinli-model.md §5"用户优先"）。
func (u *Book) Edit(id, status, dueDate string, now time.Time) error {
	m := u.commitment(id)
	if m == nil {
		return fmt.Errorf("no commitment %s", id)
	}
	cs := Changeset{Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: now}
	switch {
	case status == "done" || status == "cancelled":
		cs.CloseCommitments = []CommitmentChange{{ID: id, Rev: m.Rev(), Status: status}}
	case status != "":
		return fmt.Errorf("status must be done or cancelled")
	case dueDate != "":
		if _, err := time.Parse(time.DateOnly, dueDate); err != nil {
			return fmt.Errorf("due_date must be YYYY-MM-DD")
		}
		cs.ReviseCommitments = []CommitmentChange{{ID: id, Rev: m.Rev(), Fields: map[string]string{"due_date": dueDate, "due_text": ""}}}
	default:
		return fmt.Errorf("nothing to change")
	}
	u.commit(&cs, -1, nil)
	return nil
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

// seedDays 是预置的按日期摘要所在的（虚构的）Session。
const seedDays = "seed_days"

// Seed 为业务线中的 endUser 预置数据：交谈与承诺记为一个预置的变更集。
func (u *Book) Seed(d SeedData, now time.Time) {
	u.projects = append(u.projects, d.Projects...)
	cs := Changeset{Key: fmt.Sprintf("seed:%d", u.nextCS+1), Author: AuthorSeed, RecordedAt: now}
	for _, date := range sortedKeys(d.Days) {
		cs.CreateExchanges = append(cs.CreateExchanges, Exchange{ID: "x" + date, Session: seedDays, Date: date, Title: "当天摘要", Summary: d.Days[date],
			Projects: []Link{{Match: "none"}}})
	}
	for i, e := range d.Exchanges {
		if e.ID == "" {
			e.ID = fmt.Sprintf("x%d.0", i+1)
		}
		link := Link{Match: "none"}
		if e.Project != "" {
			link = Link{Match: "existing", Project: e.Project}
		}
		ex := Exchange{ID: e.ID, Session: "seed_" + e.Date, Date: e.Date, Title: e.Title, Summary: e.Summary, Projects: []Link{link}, Risks: e.Risks}
		for _, t := range e.Decisions {
			ex.Decisions = append(ex.Decisions, Decision{Text: t})
		}
		cs.CreateExchanges = append(cs.CreateExchanges, ex)
	}
	d.Commitments = slices.Clone(d.Commitments)
	for i := range d.Commitments {
		m := &d.Commitments[i]
		if m.ID == "" {
			u.nextC++
			m.ID = fmt.Sprintf("C-%04d", u.nextC)
		}
		cs.CreateCommitments = append(cs.CreateCommitments, Commitment{ID: m.ID, Kind: m.Kind, Content: m.Content, Counterpart: m.Counterpart,
			DueDate: m.DueDate, DueText: m.DueText, Project: m.Project, Status: m.Status, SpokenAt: m.SpokenAt, Refs: m.Refs, Unverified: m.Unverified})
	}
	if len(cs.CreateExchanges) > 0 || len(cs.CreateCommitments) > 0 {
		u.commit(&cs, -1, nil)
	}
	for _, m := range d.Commitments {
		for _, r := range m.Revisions {
			at := r.RecordedAt
			if at.IsZero() {
				at = now
			}
			author := r.Author
			if author == "" {
				author = AuthorSeed
			}
			next := Changeset{Key: fmt.Sprintf("seed:%d", u.nextCS+1), Author: author, Session: r.Session, RecordedAt: at}
			change := CommitmentChange{ID: m.ID, Fields: r.Fields, Refs: r.Refs, Exchanges: r.Exchanges, Reason: r.Reason, SpokenAt: r.SpokenAt}
			switch r.Op {
			case "close":
				change.Status = r.Fields["status"]
				next.CloseCommitments = []CommitmentChange{change}
			case "merge":
				next.MergeCommitments = []CommitmentMerge{{Into: r.Fields["merged_into"], From: []string{m.ID}, Reason: r.Reason}}
			default:
				next.ReviseCommitments = []CommitmentChange{change}
			}
			u.commit(&next, -1, nil)
		}
	}
	for i, day := range d.Filed {
		day.Session = fmt.Sprintf("seed_day_%d", i)
		u.days = append(u.days, day)
	}
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Bind 记下项目 project 的 xinli-project Session 是 sessionID（评测中用例的 Session 扮演归集时创建的项目 Session）。
func (u *Book) Bind(project, sessionID string) {
	if p := u.project(project); p != nil {
		p.Session = sessionID
	}
}

// State 返回供夹具和应用展示的投影，整理结果按日期从新到旧。
// 嵌套字段仍属于本 Book；存储的 View 提供隔离快照，离开事务后不得用它修改存储。
func (u *Book) State() State {
	l := fold(u.changesets, u.canon, u.sessionDays()) // 重新叠加得到的是新的切片，调用方可以随意修改
	st := State{Projects: slices.Clone(u.projects), Exchanges: l.exchanges, Commitments: l.commitments, Changesets: slices.Clone(u.changesets),
		Days: u.sortedDays(), Weeks: slices.Clone(u.weeks), Merges: slices.Clone(u.merges)}
	slices.Reverse(st.Days)
	st.Projects, st.Exchanges, st.Commitments, st.Changesets = orEmpty(st.Projects), orEmpty(st.Exchanges), orEmpty(st.Commitments), orEmpty(st.Changesets)
	st.Days, st.Weeks, st.Merges = orEmpty(st.Days), orEmpty(st.Weeks), orEmpty(st.Merges)
	st.ReminderIDs = []string{}
	for _, m := range st.Reminders() {
		st.ReminderIDs = append(st.ReminderIDs, m.ID)
	}
	return st
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// refold 由变更集重新叠加出交谈与承诺。
func (u *Book) refold() { u.ledger = fold(u.changesets, u.canon, u.sessionDays()) }

// foldWithout 是去掉第 i 个变更集（将被取代的那个）之后的状态；i < 0 时就是当前状态。新的变更集据此检查：重交时它的
// rev 是对着没有旧变更集的账本取的。
func (u *Book) foldWithout(i int) ledger {
	if i < 0 {
		return u.ledger
	}
	return fold(slices.Delete(slices.Clone(u.changesets), i, i+1), u.canon, u.sessionDays())
}

// canon 把已合并的项目换成保留的那个。
func (u *Book) canon(id string) string {
	for range len(u.projects) {
		p := u.project(id)
		if p == nil || p.MergedInto == "" {
			break
		}
		id = p.MergedInto
	}
	return id
}

func (u *Book) changesetSession(id string) string {
	if i := slices.IndexFunc(u.changesets, func(cs Changeset) bool { return cs.ID == id }); i >= 0 {
		return u.changesets[i].Session
	}
	return ""
}

func (u *Book) project(id string) *Project {
	if id == "" {
		return nil
	}
	if i := slices.IndexFunc(u.projects, func(p Project) bool { return p.ID == id }); i >= 0 {
		return &u.projects[i]
	}
	return nil
}

func (u *Book) newProject(name string) string {
	id := fmt.Sprintf("P-%04d", u.next)
	u.next++
	u.projects = append(u.projects, Project{ID: id, Name: name})
	return id
}

func (u *Book) projectOfSession(sid string) *Project {
	if sid == "" {
		return nil
	}
	if i := slices.IndexFunc(u.projects, func(p Project) bool { return p.Session == sid }); i >= 0 {
		return &u.projects[i]
	}
	return nil
}

// where 呈现一段交谈的归属。
func (u *Book) where(e *Exchange) string {
	var parts []string
	for _, l := range e.Projects {
		switch {
		case l.Project != "":
			name := ""
			if p := u.project(l.Project); p != nil {
				name = " " + p.Name
			}
			parts = append(parts, "归入 "+l.Project+name)
		case l.Match == "unsure":
			parts = append(parts, "待用户确认：可能属于 "+l.Candidate)
		}
	}
	if len(parts) == 0 {
		return "不归项目"
	}
	return strings.Join(parts, "；")
}

func (u *Book) projectLine(p *Project) string {
	open := 0
	for _, m := range u.commitments {
		if m.Project == p.ID && m.Open() {
			open++
		}
	}
	status := p.Status
	if status == "" {
		status = "（还没有现状）"
	}
	return fmt.Sprintf("%s %s：%s（未完成的承诺 %d 条）\n", p.ID, p.Name, status, open)
}

// commitmentLine 呈现一条承诺给模型：ID、rev、状态、内容与期限、归属、出处、引用与之后的修订（说话时间与依据）。
func (u *Book) commitmentLine(m *Commitment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s（rev %d", m.ID, m.Rev())
	if m.MergedInto != "" {
		fmt.Fprintf(&b, "，已并入 %s", m.MergedInto)
	} else if m.Status != "open" {
		b.WriteString("，" + m.Status)
	}
	b.WriteString("）" + commitmentText(m))
	if m.Project != "" {
		b.WriteString("；归入 " + m.Project)
	}
	if m.Exchange != "" {
		b.WriteString("；出处 " + m.Exchange)
	}
	if len(m.Refs) > 0 {
		b.WriteString("；引用 " + strings.Join(m.Refs, "、"))
	}
	for _, r := range m.Revisions[1:] {
		at := r.RecordedAt
		if !r.SpokenAt.IsZero() {
			at = r.SpokenAt
		}
		fmt.Fprintf(&b, "；%s %s（依据%s）", at.Format("01-02 15:04"), revisionText(&r), r.Basis())
	}
	return b.String()
}

func revisionText(r *Revision) string {
	switch {
	case r.Op == "merge":
		return "并入 " + r.Fields["merged_into"]
	case r.Fields["status"] != "":
		return "标为 " + r.Fields["status"]
	case r.Fields["due_date"] != "":
		return "期限改为 " + r.Fields["due_date"]
	}
	return "改了 " + strings.Join(sortedKeys(r.Fields), "、")
}

// openCommitments 列出项目未完成的承诺（带 rev），供 Project 的 Run 对账。
func (u *Book) openCommitments(b *strings.Builder, project string) {
	first := true
	for i := range u.commitments {
		m := &u.commitments[i]
		if m.Project != project || !m.Open() {
			continue
		}
		if first {
			b.WriteString("\n未完成的承诺：\n")
			first = false
		}
		fmt.Fprintf(b, "- %s（rev %d）%s\n", m.ID, m.Rev(), commitmentText(m))
	}
}

// sortedDays 返回按日期、整理时刻从旧到新的整理结果（拷贝）。
func (u *Book) sortedDays() []Day {
	days := slices.Clone(u.days)
	slices.SortStableFunc(days, func(a, b Day) int {
		if c := strings.Compare(a.Date, b.Date); c != 0 {
			return c
		}
		return a.FiledAt.Compare(b.FiledAt)
	})
	return days
}

// exchangeText 把一段交谈呈现为给 Project 的 Run 与助手看的文字；project 非空时只列属于它（或没有标明项目）的决定。
func exchangeText(e *Exchange, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", e.Summary)
	var ds []string
	for _, d := range e.Decisions {
		if project == "" || d.Project == "" || d.Project == project {
			ds = append(ds, d.Text)
		}
	}
	list(&b, "已定", ds)
	list(&b, "风险", e.Risks)
	return b.String()
}

func list(b *strings.Builder, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(b, "%s：%s\n", label, strings.Join(items, "；"))
	}
}

func commitmentText(m *Commitment) string {
	s := m.Content
	switch {
	case m.Kind == "theirs" && m.Counterpart != "":
		s = m.Counterpart + "答应用户：" + s
	case m.Kind == "theirs":
		s = "对方答应用户：" + s
	case m.Counterpart != "":
		s = "用户答应" + m.Counterpart + "：" + s
	default:
		s = "用户答应：" + s
	}
	switch {
	case m.DueDate != "" && m.DueText != "":
		s += "，期限 " + m.DueDate + "（" + m.DueText + "）"
	case m.DueDate != "":
		s += "，期限 " + m.DueDate
	case m.DueText != "":
		s += "，期限 " + m.DueText
	}
	if m.Unverified != "" {
		s += "（未核实：" + m.Unverified + "）"
	}
	return s
}
