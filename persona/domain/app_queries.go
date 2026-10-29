package domain

import (
	"errors"
	"slices"
	"strings"
	"time"
)

var ErrQueryNotFound = errors.New("query object not found")
var ErrQueryInvalid = errors.New("invalid query")

// QueryProjects 与模型查询共用 Active，业务规则不在 BFF 再实现一份。
func (u *Book) QueryProjects() []Project {
	out := []Project{}
	for _, p := range u.projects {
		if p.Active() {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Project) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (u *Book) QueryProject(id string) (Project, error) {
	if p := u.project(id); p != nil {
		return *p, nil
	}
	return Project{}, ErrQueryNotFound
}

func (u *Book) QueryProjectExchanges(id string) ([]Exchange, error) {
	p, err := u.QueryProject(id)
	if err != nil {
		return nil, err
	}
	out := []Exchange{}
	for _, e := range u.exchanges {
		if e.Live() && slices.Contains(e.InProjects(), p.ID) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Exchange) int { return strings.Compare(ExchangeQueryKey(a), ExchangeQueryKey(b)) })
	return out, nil
}

func ExchangeQueryKey(e Exchange) string { return e.Date + "\x00" + e.Session + "\x00" + e.ID }

// DayQuery 在没有 file_day 时也能表示尚在处理的业务日；到期只限制原文，长期总览仍可读。
type DayQuery struct {
	Date            string    `json:"date"`
	Summary         *Day      `json:"summary"`
	Progress        string    `json:"processing_position"`
	Processed       bool      `json:"processed"`
	CaptureRevision uint64    `json:"capture_revision"`
	Expired         bool      `json:"expired"`
	ExpiresAt       time.Time `json:"expires_at,omitzero"`
}

func (u *Book) QueryDay(date string) (DayQuery, error) {
	if err := u.CheckCaptures(date); err == ErrDayDeleted {
		return DayQuery{}, err
	}
	out := DayQuery{Date: date, Progress: u.CaptureProgress(date)}
	day := u.captureDays[date]
	if day != nil {
		out.CaptureRevision, out.ExpiresAt = day.CaptureRevision, day.ExpiresAt
		out.Expired = u.CheckCaptures(date) == ErrCapturesExpired
		out.Processed = day.processed()
	}
	for _, d := range u.sortedDays() {
		if d.Date == date {
			copy := d
			out.Summary = &copy
		}
	}
	if day == nil && out.Summary == nil {
		return DayQuery{}, ErrQueryNotFound
	}
	return out, nil
}

func (u *Book) QueryWeek(from string) (Week, error) {
	// 换绑后新 Run 还未接受时，仍展示上一次有效结果。
	session := u.weekSessions[from]
	if fact := u.acceptedFacts[factID(FactWeek, from)]; fact.Active {
		session = fact.Session
	}
	for i := len(u.weeks) - 1; i >= 0; i-- {
		if session != "" && u.weeks[i].Session == session {
			return u.weeks[i], nil
		}
	}
	return Week{}, ErrQueryNotFound
}

// QueryCaptures 直接分页结构化原文，不登记模型读页，不改变整理位置。
// 读后再次检查期限；调用方还必须等 Store.View 的注销复查成功才交付。
func (u *Book) QueryCaptures(date, after string, limit int) ([]PageSegment, bool, error) {
	if limit < 1 || limit > 100 {
		return nil, false, ErrQueryInvalid
	}
	if err := u.CheckCaptures(date); err != nil {
		return nil, false, err
	}
	if u.captureDays[date] == nil {
		return nil, false, ErrQueryNotFound
	}
	var from Ref
	if after != "" {
		var ok bool
		from, ok = ParseRef(after)
		if !ok {
			return nil, false, ErrQueryInvalid
		}
	}
	out := []PageSegment{}
	more := false
	for record, err := range u.captureSegments(date, from, Ref{}) {
		if err != nil {
			return nil, false, err
		}
		if after != "" && refLE(record.ref, from) {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		g := record.g
		out = append(out, PageSegment{Ref: record.ref.String(), Speaker: g.Speaker, SpeakerKey: g.SpeakerKey, SpeakerRef: g.SpeakerRef, Text: g.Text, SpokenAt: g.SpokenAt, TimeZone: g.TimeZone})
	}
	if err := u.CheckCaptures(date); err != nil {
		return nil, false, err
	}
	return out, more, nil
}

// QueryCommitment 保留完整修订；创建修订的字段来自创建项，不从当前值倒推。
func (u *Book) QueryCommitment(id string) (Commitment, error) {
	m := u.commitment(id)
	if m == nil {
		return Commitment{}, ErrQueryNotFound
	}
	out := *m
	out.Revisions = slices.Clone(m.Revisions)
	for i := range out.Revisions {
		if out.Revisions[i].Op == "create" {
			out.Revisions[i].Fields = u.commitmentInitialValues(id)
		}
	}
	return out, nil
}

func (u *Book) commitmentInitialValues(id string) map[string]string {
	values := map[string]string{}
	for _, cs := range u.changesets {
		for _, initial := range cs.CreateCommitments {
			if initial.ID == id {
				status := initial.Status
				if status == "" {
					status = "open"
				}
				values = map[string]string{"kind": initial.Kind, "content": initial.Content, "counterpart": initial.Counterpart, "due_date": initial.DueDate, "due_text": initial.DueText, "project": initial.Project, "status": status}
			}
		}
	}
	return values
}
