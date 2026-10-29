package domain

import (
	"encoding/json"
	"fmt"
	"slices"
)

// bookData 只序列化事实与分配位置；投影从变更集重建，避免两份当前值漂移。
// 它是存储格式，与对外 State 的 JSON 形状分开；CaptureDays 只含元数据，不含原文或事务接缝。
type bookData struct {
	CaptureStreams      map[string]*captureStream
	WorkBindings        map[string]workBinding
	WorkSessions        map[string]workSession
	CaptureDays         map[string]*captureDay
	DeletionTasks       []DeletionTask
	AcceptedFacts       map[string]acceptedFact
	DomainChanges       []DomainChange
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
		CaptureStreams: u.captureStreams,
		WorkBindings:   u.workBindings, WorkSessions: u.workSessions,
		CaptureDays: u.captureDays, DeletionTasks: u.deletionTasks,
		AcceptedFacts: u.acceptedFacts, DomainChanges: u.domainChanges,
		Projects: u.projects, Changesets: u.changesets, Days: u.days, Weeks: u.weeks, Merges: u.merges,
		WeekSessions: u.weekSessions, Submitted: u.submitted,
		Next: u.next, NextC: u.nextC, NextCS: u.nextCS,
	})
}

func (u *Book) UnmarshalJSON(data []byte) error {
	// 旧内存快照没有独立原文库，不能静默丢掉正文后伪装成成功恢复。
	var legacy struct {
		CaptureDays map[string]struct {
			Batches json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	for _, day := range legacy.CaptureDays {
		if len(day.Batches) > 0 && string(day.Batches) != "null" && string(day.Batches) != "[]" {
			return fmt.Errorf("legacy capture payload requires separate import")
		}
	}
	var d bookData
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	next := NewBook()
	next.captureStreams = d.CaptureStreams
	next.workBindings, next.workSessions = d.WorkBindings, d.WorkSessions
	next.captureDays = d.CaptureDays
	next.deletionTasks = d.DeletionTasks
	next.acceptedFacts, next.domainChanges = d.AcceptedFacts, d.DomainChanges
	// 旧快照只有当前 Session，至少补齐这个已知绑定；无法推断更早的换绑历史。
	for date, day := range next.captureDays {
		// A2 已保存旧推导的最后时区；装载时一次性升级，之后不再随片段改变。
		if day.ExpiresAt.IsZero() && (day.Next > 0 || day.TimeZone != "") {
			if day.TimeZone == "" {
				day.TimeZone = DefaultCaptureTimeZone
			}
			var err error
			day.ExpiresAt, err = CaptureExpiresAt(date, day.TimeZone)
			if err != nil {
				return err
			}
			day.DeleteAt = day.ExpiresAt
		}
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

// Clone 只复制账本与编号；存储须另行绑定同一事务的原文接缝，快照本身不能读原文。
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
