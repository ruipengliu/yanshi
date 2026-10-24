package xinlimock

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/memory"
)

// 每周回顾（docs/design/captures-weekly-review.md）：归档按规则进行，不调用模型；xinli-week 的 Run 提议合并相似的项目、
// 交出本周觉察；本周有新交谈的项目由各自的 xinli-project 依据完整时间线重写现状。

// WeekAgent 是每周回顾的 Session 的 AgentDef。
const WeekAgent = "xinli-week"

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

func init() {
	tools = append(tools, weekTools...)
}

var weekTools = []struct {
	tool    *mcp.Tool
	handler func(c *call) string
}{
	{&mcp.Tool{Name: "week_overview", Description: "返回最近 7 天每天的总览、交谈的标题与归属，以及每天对用户沟通方式的觉察。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		func(c *call) string {
			var b strings.Builder
			for _, d := range c.u.weekDays(c.x.now()) {
				fmt.Fprintf(&b, "%s：%s\n", d.Date, d.Headline)
				for _, e := range c.u.exchanges {
					if e.Date == d.Date {
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
		}},
	{&mcp.Tool{Name: "similar_projects", Description: "找出至多 5 对可能重复的项目（相似度从高到低），附各自的现状与最近几段交谈的标题，供判断是否合并。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		func(c *call) string {
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
					pairs = append(pairs, pair{a, b, memory.TextSimilarity(a.Name+" "+a.Status, b.Name+" "+b.Status)})
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
						if e.Project == x.ID {
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
		}},
	{&mcp.Tool{Name: "propose_merge", Description: "提议把项目 from 并入 into（两者指同一件事）。提议进业务线的\"待确认\"，由用户确认后才合并。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"into":{"type":"string","description":"保留的项目 ID"},` +
			`"from":{"type":"string","description":"并入的项目 ID"},"reason":{"type":"string","description":"为什么是同一件事"}},"required":["into","from","reason"]}`)},
		func(c *call) string {
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
		}},
	{&mcp.Tool{Name: "file_week", Description: "交出本周的觉察，每次回顾调用一次：habits 是本周至少在两天出现过的沟通习惯（days 写出现的日期，example 附一个例子），" +
		"suggestions 是建议。只出现一天的不算习惯。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"habits":{"type":"array","items":{"type":"object","properties":{` +
			`"habit":{"type":"string"},"days":{"type":"array","items":{"type":"string"},"description":"出现的日期 YYYY-MM-DD，至少两天"},` +
			`"example":{"type":"string"}},"required":["habit","days"]}},"suggestions":{"type":"array","items":{"type":"string"}}},"required":["habits"]}`)},
		func(c *call) string {
			var in struct {
				Habits      []Habit
				Suggestions []string
			}
			_ = json.Unmarshal(c.args, &in)
			// 业务线按规则收下习惯：出现不到两个不同日期的不收（captures-weekly-review.md §4）。
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
			c.u.weeks = append(c.u.weeks, Week{Session: c.session, FiledAt: c.x.now(), Habits: kept, Suggestions: in.Suggestions})
			out := fmt.Sprintf("已收到本周的觉察（%d 个习惯）", len(kept))
			if len(single) > 0 {
				out += "；只出现一天的不算习惯，没有收录：" + strings.Join(single, "、")
			}
			return out
		}},
}

// weekDays 返回最近 7 天的每日整理（从旧到新）。
func (u *user) weekDays(now time.Time) []Day {
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
func (u *user) lastActive(id string) string {
	last := ""
	for _, e := range u.exchanges {
		if e.Project == id && e.Date > last {
			last = e.Date
		}
	}
	return last
}

// Archive 按规则归档（captures-weekly-review.md §3）：30 天没有新交谈、也没有未完成承诺的项目。没有交谈的项目
// （刚建立或预置的）不归档。返回归档的项目 ID。
func (x *Server) Archive(businessLine, endUser string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	cutoff := x.now().Add(-ArchiveAfter).Format(time.DateOnly)
	var out []string
	for i := range u.projects {
		p := &u.projects[i]
		last := u.lastActive(p.ID)
		if !p.Active() || last == "" || last >= cutoff {
			continue
		}
		if slices.ContainsFunc(u.commitments, func(m Commitment) bool { return m.Project == p.ID && m.Status == "open" }) {
			continue
		}
		p.Archived = true
		out = append(out, p.ID)
	}
	return out
}

// ResolveMerge 是用户对合并提议的选择：接受时把 from 的交谈与承诺移到 into，from 标为已并入，以后的归集都交给 into。
// 业务线还应关闭 from 的 xinli-project Session；模拟服务不关闭，调试控制台还要查看它。
func (x *Server) ResolveMerge(businessLine, endUser, id string, accept bool) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
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
	for j := range u.exchanges {
		if u.exchanges[j].Project == from.ID {
			u.exchanges[j].Project = into.ID
		}
	}
	for j := range u.commitments {
		if u.commitments[j].Project == from.ID {
			u.commitments[j].Project = into.ID
		}
	}
	from.MergedInto, m.Status = into.ID, "accepted"
	return nil
}

// Week 扮演业务线服务端做每周回顾（captures-weekly-review.md §2）：归档，为本周创建 xinli-week Session 并提交"回顾本周"，
// 再向本周有新交谈的项目提交"重写现状"。输入 ID 由本周的第一天派生：同一周重复触发只生效一次。返回回顾的 Session。
func (x *Server) Week(ctx context.Context, businessLine, endUser string) (string, error) {
	if x.Business == nil {
		return "", fmt.Errorf("xinli mock: no business line service to submit to")
	}
	x.Archive(businessLine, endUser)
	now := x.now()
	week := now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly)
	x.mu.Lock()
	sid := x.user(businessLine, endUser).weekSessions[week]
	x.mu.Unlock()
	if sid == "" {
		var err error
		if sid, err = x.Business.CreateSession(ctx, businessLine, endUser, WeekAgent); err != nil {
			return "", err
		}
		x.mu.Lock()
		x.user(businessLine, endUser).weekSessions[week] = sid
		x.mu.Unlock()
	}
	if err := x.Business.Submit(ctx, sid, "xinli_week_"+week, "回顾本周"); err != nil {
		return "", err
	}
	x.mu.Lock()
	u := x.user(businessLine, endUser)
	var rewrite []Project
	for _, p := range u.projects {
		if p.Active() && p.Session != "" && u.lastActive(p.ID) >= week {
			rewrite = append(rewrite, p)
		}
	}
	x.mu.Unlock()
	for _, p := range rewrite {
		if err := x.Business.Submit(ctx, p.Session, "xinli_rewrite_"+week+"_"+p.ID, "重写现状"); err != nil {
			x.logger().Warn("xinli mock: submit rewrite", "project", p.ID, "session_id", p.Session, "err", err)
		}
	}
	return sid, nil
}
