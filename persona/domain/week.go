package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// 每周回顾（docs/design/xinli-captures.md §7）：归档按规则进行，不调用模型；xinli-week 的 Run 提议合并相似的项目、
// 交出本周觉察；本周有新交谈的项目由各自的 xinli-project 依据完整时间线重写现状。

const (
	// ArchiveAfter 是归档的期限：这么久没有新交谈、也没有未完成承诺的项目归档（§3）。
	ArchiveAfter = 30 * 24 * time.Hour
	// weekDays 是"本周"的天数：回顾看最近 7 天。
	weekDays = 7
)

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

// weekDays 返回最近 7 天的每日整理（从旧到新）。
func (u *Book) weekDays(now time.Time) []Day {
	from := now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly)
	var out []Day
	for _, d := range u.sortedDays() {
		if d.Date >= from {
			out = append(out, d)
		}
	}
	return out
}

// lastActive 返回项目最近一段交谈的日期，没有交谈时为空。
func (u *Book) lastActive(id string) string {
	last := ""
	for _, e := range u.exchanges {
		if e.Live() && slices.Contains(e.InProjects(), id) && e.Date > last {
			last = e.Date
		}
	}
	return last
}

// Archive 按规则归档（xinli-captures.md §7）：30 天没有新交谈、也没有未完成承诺的项目。没有交谈的项目
// （刚建立或预置的）不归档。返回归档的项目 ID。
func (u *Book) Archive(now time.Time) []string {
	cutoff := now.Add(-ArchiveAfter).Format(time.DateOnly)
	var out []string
	for i := range u.projects {
		p := &u.projects[i]
		last := u.lastActive(p.ID)
		if !p.Active() || last == "" || last >= cutoff {
			continue
		}
		if slices.ContainsFunc(u.commitments, func(m Commitment) bool { return m.Project == p.ID && m.Open() }) {
			continue
		}
		p.Archived = true
		out = append(out, p.ID)
	}
	return out
}

// ResolveMerge 是用户对合并提议的选择：接受时 from 的交谈与承诺归到 into，from 标为已并入，以后的归集都交给 into。
// 是否关闭 from 的 xinli-project Session 由应用层决定，领域操作不调用内核。
func (u *Book) ResolveMerge(id string, accept bool) error {
	i := slices.IndexFunc(u.merges, func(m Merge) bool { return m.ID == id })
	if i < 0 || u.merges[i].Status != "pending" {
		return fmt.Errorf("merge %s is not pending", id)
	}
	m := &u.merges[i]
	if !accept {
		m.Status = "rejected"
		return nil
	}
	into, from := u.project(m.Into), u.project(m.From)
	if into == nil || from == nil || !into.Active() || !from.Active() {
		return fmt.Errorf("merge %s: a project is gone", id)
	}
	// 交谈与承诺只按 ID 引用项目：叠加时把 from 换成 into（Book.canon），不改写变更集。
	from.MergedInto, m.Status = into.ID, "accepted"
	u.refold()
	return nil
}

func (c *Call) WeekOverview() string {
	var b strings.Builder
	for _, d := range c.u.weekDays(c.now) {
		fmt.Fprintf(&b, "%s：%s\n", d.Date, d.Headline)
		for _, e := range c.u.exchanges {
			if e.Date == d.Date && e.Live() {
				fmt.Fprintf(&b, "  [%s] %s\n", c.u.where(&e), e.Title)
			}
		}
		list(&b, "  沟通习惯", d.Awareness.Habits)
		list(&b, "  建议", d.Awareness.Suggestions)
	}
	if b.Len() == 0 {
		return "最近 7 天没有整理过的内容"
	}
	return b.String()
}

func (c *Call) SimilarProjects() string {
	type pair struct {
		a, b  *Project
		score float64
	}
	var active []*Project
	for i := range c.u.projects {
		if c.u.projects[i].Active() {
			active = append(active, &c.u.projects[i])
		}
	}
	var pairs []pair
	for i := range active {
		for j := i + 1; j < len(active); j++ {
			a, b := active[i], active[j]
			pairs = append(pairs, pair{a, b, textSimilarity(a.Name+" "+a.Status, b.Name+" "+b.Status)})
		}
	}
	slices.SortStableFunc(pairs, func(x, y pair) int {
		switch {
		case x.score > y.score:
			return -1
		case x.score < y.score:
			return 1
		}
		return 0
	})
	if len(pairs) == 0 {
		return "没有可以比较的项目"
	}
	var b strings.Builder
	for i, p := range pairs[:min(5, len(pairs))] {
		fmt.Fprintf(&b, "第 %d 对：\n", i+1)
		for _, x := range []*Project{p.a, p.b} {
			b.WriteString("  " + c.u.projectLine(x))
			var titles []string
			for _, e := range c.u.exchanges {
				if e.Live() && slices.Contains(e.InProjects(), x.ID) {
					titles = append(titles, e.Date+" "+e.Title)
				}
			}
			if len(titles) > 3 {
				titles = titles[len(titles)-3:]
			}
			list(&b, "    最近的交谈", titles)
		}
	}
	return b.String()
}

func (c *Call) ProposeMerge() string {
	var in struct{ Into, From, Reason string }
	_ = json.Unmarshal(c.args, &in)
	a, b := c.u.project(in.Into), c.u.project(in.From)
	switch {
	case a == nil || b == nil:
		return "没有这个项目：into 与 from 须是 similar_projects 给出的 ID"
	case a.ID == b.ID:
		return "into 与 from 不能是同一个项目"
	case !a.Active() || !b.Active():
		return "项目已归档或已合并"
	}
	// 同一会话中对同一对项目的提议只保留一条（重试、接管）。
	for _, m := range c.u.merges {
		if m.Session == c.session && m.Status == "pending" && ((m.Into == a.ID && m.From == b.ID) || (m.Into == b.ID && m.From == a.ID)) {
			return "已经提议过合并 " + m.From + " 与 " + m.Into
		}
	}
	m := Merge{ID: fmt.Sprintf("M-%04d", len(c.u.merges)+1), Session: c.session, Into: a.ID, From: b.ID, Reason: in.Reason, Status: "pending"}
	c.u.merges = append(c.u.merges, m)
	return fmt.Sprintf("已提议把 %s %s 并入 %s %s，等用户确认（%s）", b.ID, b.Name, a.ID, a.Name, m.ID)
}

func (c *Call) FileWeek() string {
	var in struct {
		Habits      []Habit
		Suggestions []string
	}
	_ = json.Unmarshal(c.args, &in)
	// 业务线按规则收下习惯：出现不到两个不同日期的不收（xinli-captures.md §7）。
	var kept []Habit
	var single []string
	for _, h := range in.Habits {
		if days := slices.Compact(slices.Sorted(slices.Values(h.Days))); len(days) >= 2 {
			kept = append(kept, h)
		} else {
			single = append(single, h.Habit)
		}
	}
	c.u.weeks = slices.DeleteFunc(c.u.weeks, func(w Week) bool { return w.Session == c.session })
	c.u.weeks = append(c.u.weeks, Week{Session: c.session, FiledAt: c.now, Habits: kept, Suggestions: in.Suggestions})
	out := fmt.Sprintf("已收到本周的觉察（%d 个习惯）", len(kept))
	if len(single) > 0 {
		out += "；只出现一天的不算习惯，没有收录：" + strings.Join(single, "、")
	}
	return out
}

// WeekPlan 是编排器需要的知识；From 是最近七天的起点，不是日历周的星期一。
type WeekPlan struct {
	From    string    `json:"from"`
	Session string    `json:"session"`
	Rewrite []Project `json:"rewrite"`
}

// ReviewWeek 只查询，不归档、不创建 Session；编排器先调用 Archive，再按此结果提交输入。
func (u *Book) ReviewWeek(now time.Time) WeekPlan {
	from := now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly)
	plan := WeekPlan{From: from, Session: u.weekSessions[from], Rewrite: []Project{}}
	for _, p := range u.projects {
		if p.Active() && p.Session != "" && u.lastActive(p.ID) >= from {
			plan.Rewrite = append(plan.Rewrite, p)
		}
	}
	return plan
}

// BindWeek 记下编排器已经创建的回顾 Session；创建与输入去重属于应用层。
func (u *Book) BindWeek(from, session string) { u.weekSessions[from] = session }
