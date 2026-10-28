package domain

import "encoding/json"

// bookData 只序列化事实与分配位置；投影从变更集重建，避免两份当前值漂移。
// 它是存储格式，与对外 State 的 JSON 形状分开。
type bookData struct {
	CaptureDays         map[string]*captureDay
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
	return json.Marshal(bookData{u.captureDays, u.projects, u.changesets, u.days, u.weeks, u.merges,
		u.weekSessions, u.submitted, u.next, u.nextC, u.nextCS})
}

func (u *Book) UnmarshalJSON(data []byte) error {
	var d bookData
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	next := NewBook()
	next.captureDays = d.CaptureDays
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
