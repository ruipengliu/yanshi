package domain

import (
	"encoding/json"
	"slices"
)

// bookData 只序列化事实与分配位置；投影从变更集重建，避免两份当前值漂移。
// 它是存储格式，与对外 State 的 JSON 形状分开。
type bookData struct {
	CaptureDays         map[string]*captureDay
	DeletionTasks       []DeletionTask
	Projects            []Project
	Changesets          []Changeset
	Days                []Day
	Weeks               []Week
	Merges              []Merge
	WeekSessions        map[string]string
	Submitted           map[string]string
	Next, NextC, NextCS int
}

func NewBook() *Book {
	return &Book{weekSessions: map[string]string{}, submitted: map[string]string{}, next: 101}
}

func (u *Book) MarshalJSON() ([]byte, error) {
	return json.Marshal(bookData{
		CaptureDays: u.captureDays, DeletionTasks: u.deletionTasks,
		Projects: u.projects, Changesets: u.changesets, Days: u.days, Weeks: u.weeks, Merges: u.merges,
		WeekSessions: u.weekSessions, Submitted: u.submitted,
		Next: u.next, NextC: u.nextC, NextCS: u.nextCS,
	})
}

func (u *Book) UnmarshalJSON(data []byte) error {
	var d bookData
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	next := NewBook()
	next.captureDays = d.CaptureDays
	next.deletionTasks = d.DeletionTasks
	// 旧快照只有当前 Session，至少补齐这个已知绑定；无法推断更早的换绑历史。
	for _, day := range next.captureDays {
		if day.Session != "" && !slices.Contains(day.Sessions, day.Session) {
			day.Sessions = append(day.Sessions, day.Session)
		}
	}
	next.projects, next.changesets, next.days, next.weeks, next.merges = d.Projects, d.Changesets, d.Days, d.Weeks, d.Merges
	if d.WeekSessions != nil {
		next.weekSessions = d.WeekSessions
	}
	if d.Submitted != nil {
		next.submitted = d.Submitted
	}
	if d.Next != 0 {
		next.next = d.Next
	}
	next.nextC, next.nextCS = d.NextC, d.NextCS
	for i := range next.changesets {
		if next.changesets[i].Day == "" {
			next.changesets[i].Day, _ = next.sessionDay(next.changesets[i].Session)
		}
	}
	next.refold()
	*u = *next
	return nil
}

// Clone 连同嵌套字段和编号一起复制；回调持有的引用不能绕过存储事务修改事实。
func (u *Book) Clone() (*Book, error) {
	data, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}
	var out Book
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
