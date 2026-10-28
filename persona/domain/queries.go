package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

func (c *Call) ListProjects() string {
	var b strings.Builder
	for i := range c.u.projects {
		if c.u.projects[i].Active() {
			b.WriteString(c.u.projectLine(&c.u.projects[i]))
		}
	}
	if b.Len() == 0 {
		return "还没有项目"
	}
	return b.String()
}

func (c *Call) ProjectDigest() string {
	var in struct{ Project string }
	_ = json.Unmarshal(c.args, &in)
	p := c.u.project(in.Project)
	if in.Project == "" {
		p = c.u.projectOfSession(c.session)
	}
	if p == nil {
		return "没有这个项目"
	}
	var b strings.Builder
	b.WriteString(c.u.projectLine(p))
	list(&b, "下一步", p.NextSteps)
	list(&b, "待解决", p.OpenQuestions)
	for _, e := range c.u.exchanges {
		if e.Live() && slices.Contains(e.InProjects(), p.ID) {
			fmt.Fprintf(&b, "\n%s %s %s\n%s", e.Date, e.ID, e.Title, exchangeText(&e, p.ID))
		}
	}
	c.u.openCommitments(&b, p.ID)
	return b.String()
}

func (c *Call) RecentDays() string {
	var b strings.Builder
	for _, d := range c.u.sortedDays() {
		fmt.Fprintf(&b, "%s：%s\n", d.Date, d.Headline)
		for _, h := range d.Highlights {
			fmt.Fprintf(&b, "- %s\n", h)
		}
		for _, e := range c.u.exchanges {
			if c.u.sameDay(e.Session, d.Session) && e.Live() {
				fmt.Fprintf(&b, "  [%s] %s：%s\n", c.u.where(&e), e.Title, e.Summary)
			}
		}
	}
	for _, e := range c.u.exchanges {
		if e.Session == seedDays {
			fmt.Fprintf(&b, "%s：%s\n", e.Date, e.Summary)
		}
	}
	if b.Len() == 0 {
		return "最近没有整理过的内容"
	}
	return b.String()
}

func (c *Call) DaySummary() string {
	var in struct{ Date string }
	_ = json.Unmarshal(c.args, &in)
	p := c.u.projectOfSession(c.session)
	var b strings.Builder
	if p != nil {
		for _, e := range c.u.exchanges {
			if e.Live() && e.Date == in.Date && slices.Contains(e.InProjects(), p.ID) {
				fmt.Fprintf(&b, "交谈 %s %s\n%s", e.ID, e.Title, exchangeText(&e, p.ID))
			}
		}
	}
	if b.Len() == 0 {
		for _, e := range c.u.exchanges {
			if e.Session == seedDays && e.Date == in.Date {
				fmt.Fprintf(&b, "交谈 %s\n%s\n", e.ID, e.Summary)
			}
		}
	}
	if b.Len() == 0 {
		b.WriteString("这一天没有与本项目有关的交谈\n")
	}
	if p != nil {
		c.u.openCommitments(&b, p.ID)
	}
	return b.String()
}

func (c *Call) SetStatus() string {
	var in struct {
		Status        string
		NextSteps     []string `json:"next_steps"`
		OpenQuestions []string `json:"open_questions"`
	}
	_ = json.Unmarshal(c.args, &in)
	p := c.u.projectOfSession(c.session)
	if p == nil {
		return "这个会话不对应任何项目"
	}
	p.Status, p.NextSteps, p.OpenQuestions, p.StatusAt = in.Status, in.NextSteps, in.OpenQuestions, c.now
	return "已更新 " + p.ID + " 的现状"
}
