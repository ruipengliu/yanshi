package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

func (c *Call) ListCommitments() string {
	var in struct {
		CommitmentFilter
		After string
	}
	if json.Unmarshal(c.args, &in) != nil {
		return "参数格式不正确"
	}
	found, err := c.u.QueryCommitments(in.CommitmentFilter)
	if err != nil {
		return err.Error()
	}
	start := 0
	if in.After != "" {
		i := slices.IndexFunc(found, func(m *Commitment) bool { return m.ID == in.After })
		if i < 0 {
			return "after 无效：请从第一页重新查询"
		}
		start = i + 1
	}
	var b strings.Builder
	end := min(start+30, len(found))
	for _, m := range found[start:end] {
		fmt.Fprintln(&b, c.u.queryCommitmentLine(m))
	}
	if len(found) == 0 {
		b.WriteString("没有符合条件的承诺\n")
	}
	next := "无"
	if end < len(found) {
		next = found[end-1].ID
	}
	fmt.Fprintf(&b, "共 %d 条；下一页 after：%s", len(found), next)
	return b.String()
}

func queryDate(s string) string {
	if s == "" {
		return "无"
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return s + "（" + weekdayNames[t.Weekday()] + "）"
	}
	return s
}

func (u *Book) queryCommitmentLine(m *Commitment) string {
	s := fmt.Sprintf("%s | %s | %s | %s | 对方：%s | 期限：%s", m.ID, m.Kind, m.Status, m.Content, emptyValue(m.Counterpart), queryDate(m.DueDate))
	if m.DueText != "" {
		s += "（" + m.DueText + "）"
	}
	if m.Project != "" {
		s += " | 项目：" + m.Project
		if p := u.project(m.Project); p != nil {
			s += " " + p.Name
		}
	}
	if m.Unverified != "" {
		s += " | 未核对：" + m.Unverified
	}
	return fmt.Sprintf("%s | 修订 %d 次", s, m.Rev())
}

func emptyValue(s string) string {
	if s == "" {
		return "无"
	}
	return s
}
func historyTime(t time.Time) string {
	if t.IsZero() {
		return "未记录"
	}
	return t.Format(time.RFC3339)
}
func revisionTime(r Revision) time.Time {
	if !r.SpokenAt.IsZero() {
		return r.SpokenAt
	}
	return r.RecordedAt
}

func (c *Call) CommitmentHistory() string {
	var in struct{ ID string }
	if json.Unmarshal(c.args, &in) != nil {
		return "参数格式不正确"
	}
	m := c.u.commitment(in.ID)
	if m == nil {
		return "没有这个承诺"
	}
	var b strings.Builder
	seen := map[string]bool{}
	var write func(*Commitment)
	write = func(m *Commitment) {
		if seen[m.ID] {
			return
		}
		seen[m.ID] = true
		fmt.Fprintf(&b, "当前值：%s\n", c.u.queryCommitmentLine(m))
		if m.MergedInto != "" {
			fmt.Fprintf(&b, "已合并到 %s\n", m.MergedInto)
		}
		c.writeCommitmentRevisions(&b, m)
		for i := range c.u.commitments {
			other := &c.u.commitments[i]
			if other.MergedInto == m.ID {
				fmt.Fprintf(&b, "被合并者 %s 的历史：\n", other.ID)
				write(other)
			}
		}
	}
	write(m)
	return b.String()
}

func (c *Call) writeCommitmentRevisions(b *strings.Builder, m *Commitment) {
	// 新建修订不存 Fields；初值必须取自创建项，不能拿当前值倒推未曾保存的旧值。
	values := c.u.commitmentInitialValues(m.ID)
	revisions := slices.Clone(m.Revisions)
	slices.SortStableFunc(revisions, func(a, b Revision) int {
		// 创建项先建立初值；没有说话时间的用户操作与合并以记录时间排序，展示仍明确说话时间未记录。
		if a.Op == "create" && b.Op != "create" {
			return -1
		}
		if b.Op == "create" && a.Op != "create" {
			return 1
		}
		return revisionTime(a).Compare(revisionTime(b))
	})
	labels := map[string]string{"kind": "责任方", "content": "内容", "counterpart": "对方", "due_date": "期限", "due_text": "期限说法", "project": "项目", "status": "状态", "merged_into": "合并到"}
	for _, r := range revisions {
		op := "改内容"
		switch r.Op {
		case "create":
			op = "新建"
		case "merge":
			op = "合并"
		case "close":
			if r.Fields["status"] == "done" {
				op = "完成"
			} else {
				op = "取消"
			}
		default:
			if _, ok := r.Fields["due_date"]; ok {
				op = "改期"
			} else if _, ok := r.Fields["due_text"]; ok {
				op = "改期"
			}
		}
		fmt.Fprintf(b, "- 说话时间：%s；记录时间：%s；%s", historyTime(r.SpokenAt), historyTime(r.RecordedAt), op)
		fields := r.Fields
		if r.Op == "create" {
			fields = values
		}
		for _, key := range sortedKeys(fields) {
			before := values[key]
			if r.Op == "create" {
				before = ""
			}
			after := fields[key]
			label := labels[key]
			if label == "" {
				label = key
			}
			if key == "due_date" {
				before, after = queryDate(before), queryDate(after)
			} else {
				before, after = emptyValue(before), emptyValue(after)
			}
			fmt.Fprintf(b, "；%s：%s → %s", label, before, after)
			values[key] = fields[key]
		}
		basis := r.Basis()
		if basis == "用户" {
			basis = "用户本人"
		}
		fmt.Fprintf(b, "；依据：%s", basis)
		if r.Reason != "" {
			fmt.Fprintf(b, "（%s）", r.Reason)
		}
		if len(r.Refs) > 0 {
			fmt.Fprintf(b, "；引用：%s %s", c.u.changesetSession(r.Changeset), strings.Join(r.Refs, "、"))
			if !r.SpokenAt.IsZero() && c.now.Sub(r.SpokenAt) > 7*24*time.Hour {
				b.WriteString("（原话已过保留期）")
			}
		}
		if len(r.Exchanges) > 0 {
			fmt.Fprintf(b, "；交谈引用：%s %s", c.u.changesetSession(r.Changeset), strings.Join(r.Exchanges, "、"))
		}
		b.WriteByte('\n')
	}
}

// CommitmentFilter 与 MCP 查询共用筛选和排序，应用不另定义承诺语义。
type CommitmentFilter struct {
	Kind, Status, Counterpart, Query, Project string
	DueFrom                                   string `json:"due_from"`
	DueTo                                     string `json:"due_to"`
}

func (u *Book) QueryCommitments(in CommitmentFilter) ([]*Commitment, error) {
	if in.Status == "" {
		in.Status = "open"
	}
	if in.Kind != "" && in.Kind != "mine" && in.Kind != "theirs" {
		return nil, fmt.Errorf("kind 须为 mine 或 theirs")
	}
	if !slices.Contains([]string{"open", "done", "cancelled", "all"}, in.Status) {
		return nil, fmt.Errorf("status 须为 open、done、cancelled 或 all")
	}
	for _, date := range []string{in.DueFrom, in.DueTo} {
		if date != "" {
			if _, err := time.Parse(time.DateOnly, date); err != nil {
				return nil, fmt.Errorf("期限须为 YYYY-MM-DD")
			}
		}
	}
	if in.DueFrom != "" && in.DueTo != "" && in.DueFrom > in.DueTo {
		return nil, fmt.Errorf("due_from 不能晚于 due_to")
	}
	var found []*Commitment
	for i := range u.commitments {
		m := &u.commitments[i]
		if m.MergedInto != "" || (in.Kind != "" && m.Kind != in.Kind) || (in.Status != "all" && m.Status != in.Status) {
			continue
		}
		if in.Project != "" && m.Project != u.canon(in.Project) {
			continue
		}
		if !strings.Contains(m.Counterpart, in.Counterpart) {
			continue
		}
		if in.Query != "" && textSimilarity(in.Query, m.Content+" "+m.Counterpart) < relatedTopic {
			continue
		}
		if (in.DueFrom != "" || in.DueTo != "") && m.DueDate == "" {
			continue
		}
		if in.DueFrom != "" && m.DueDate < in.DueFrom || in.DueTo != "" && m.DueDate > in.DueTo {
			continue
		}
		found = append(found, m)
	}
	slices.SortFunc(found, func(a, b *Commitment) int {
		if a.DueDate != b.DueDate {
			if a.DueDate == "" {
				return 1
			}
			if b.DueDate == "" {
				return -1
			}
			return strings.Compare(a.DueDate, b.DueDate)
		}
		if n := a.SpokenAt.Compare(b.SpokenAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return found, nil
}
