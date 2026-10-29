// Package query 是 Persona 面向应用的只读 API，进程内与 HTTP 共用同一事务入口。
package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"persona/domain"
	"persona/paging"
	"persona/store"
)

type Reader interface {
	Read(context.Context, store.Owner, Request) (json.RawMessage, error)
}

type Service struct {
	Store store.Store
	Now   func() time.Time
}

type Request struct {
	Resource string
	ID       string
	Date     string
	Limit    int
	Cursor   string
	Filter   domain.CommitmentFilter
}

var ErrInvalid = errors.New("invalid query")

// Parse 拒绝重复及未知参数，尤其不接受客户端自报 owner。
func Parse(resource, id string, values url.Values) (Request, error) {
	q := Request{Resource: resource, ID: id}
	allowed := map[string]bool{"limit": true, "cursor": true}
	switch resource {
	case "access", "projects", "project", "exchanges", "history", "deletions":
	case "day", "captures", "week":
		allowed["date"] = true
	case "commitments":
		for _, name := range []string{"kind", "status", "counterpart", "query", "project", "due_from", "due_to"} {
			allowed[name] = true
		}
	default:
		return q, ErrInvalid
	}
	for k, v := range values {
		if !allowed[k] || len(v) != 1 || len(v[0]) > 4096 {
			return q, ErrInvalid
		}
	}
	var err error
	q.Limit, err = paging.Limit(values)
	if err != nil {
		return q, ErrInvalid
	}
	q.Cursor, q.Date = values.Get("cursor"), values.Get("date")
	q.Filter = domain.CommitmentFilter{Kind: values.Get("kind"), Status: values.Get("status"), Project: values.Get("project"), Counterpart: values.Get("counterpart"), Query: values.Get("query"), DueFrom: values.Get("due_from"), DueTo: values.Get("due_to")}
	if q.Resource == "day" || q.Resource == "captures" || q.Resource == "week" {
		if _, err := time.Parse(time.DateOnly, q.Date); err != nil {
			return q, ErrInvalid
		}
	}
	if (resource == "project" || resource == "exchanges" || resource == "history") && (id == "" || len(id) > 256) {
		return q, ErrInvalid
	}
	if (resource == "project" || resource == "day" || resource == "week") && (values.Has("limit") || values.Has("cursor")) {
		return q, ErrInvalid
	}
	return q, nil
}

// 进程内调用同样校验参数，不能因省去 HTTP 就放宽查询契约。
func (q Request) validate() error {
	if q.Limit < 1 || q.Limit > paging.MaxLimit || len(q.Cursor) > 4096 || len(q.ID) > 256 {
		return ErrInvalid
	}
	switch q.Resource {
	case "access", "projects", "commitments", "deletions":
	case "project", "exchanges", "history":
		if q.ID == "" {
			return ErrInvalid
		}
	case "day", "captures", "week":
		if _, err := time.Parse(time.DateOnly, q.Date); err != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if (q.Resource == "project" || q.Resource == "day" || q.Resource == "week") && q.Cursor != "" {
		return ErrInvalid
	}
	return nil
}

func (q Request) scope(owner store.Owner) string {
	filter, _ := json.Marshal(q.Filter)
	return paging.Scope(owner.BusinessLine, owner.EndUser, q.Resource, q.ID, q.Date, string(filter))
}

func (s *Service) Read(ctx context.Context, owner store.Owner, q Request) (json.RawMessage, error) {
	if owner.BusinessLine == "" || owner.EndUser == "" || q.validate() != nil {
		return nil, ErrInvalid
	}
	scope := q.scope(owner)
	after, err := paging.Decode(q.Cursor, scope)
	if err != nil {
		return nil, ErrInvalid
	}
	var data json.RawMessage
	err = s.Store.View(ctx, owner.BusinessLine, owner.EndUser, func(book *domain.Book) error {
		if s.Now != nil {
			book.SetCaptureClock(s.Now)
		}
		out, err := readBook(book, q, after, scope)
		if err != nil {
			return err
		}
		data, err = json.Marshal(out)
		return err
	})
	// 不能让回调内暂存的数据越过 View 返回时的注销检查。
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readBook(book *domain.Book, q Request, after, scope string) (any, error) {
	switch q.Resource {
	case "access":
		return struct{}{}, nil
	case "projects":
		items := []domain.ProjectQuery{}
		for _, p := range book.QueryProjects() {
			items = append(items, domain.ProjectForUser(p))
		}
		return paging.Slice(items, q.Limit, after, scope, func(p domain.ProjectQuery) string { return p.ID }), nil
	case "project":
		p, err := book.QueryProject(q.ID)
		return domain.ProjectForUser(p), err
	case "exchanges":
		items, err := book.QueryProjectExchanges(q.ID)
		if err != nil {
			return nil, err
		}
		out := []domain.ExchangeQuery{}
		for _, e := range items {
			out = append(out, domain.ExchangeForUser(e))
		}
		return paging.Slice(out, q.Limit, after, scope, func(e domain.ExchangeQuery) string { return domain.ExchangeQueryKey(e.Exchange) }), nil
	case "commitments":
		items, err := book.QueryCommitments(q.Filter)
		if err != nil {
			return nil, ErrInvalid
		}
		// 键与领域排序一致：无期限排最后，说话时间按 UTC 固定宽度编码。
		out := []CommitmentView{}
		for _, m := range items {
			out = append(out, CommitmentView{Commitment: *m, Rev: m.Rev()})
		}
		return paging.Slice(out, q.Limit, after, scope, func(m CommitmentView) string { return commitmentKey(&m.Commitment) }), nil
	case "history":
		m, err := book.QueryCommitment(q.ID)
		if err != nil {
			return nil, err
		}
		type revision struct {
			Number int `json:"rev"`
			domain.Revision
		}
		items := []revision{}
		for i, r := range m.Revisions {
			items = append(items, revision{Number: i + 1, Revision: r})
		}
		page := paging.Slice(items, q.Limit, after, scope, func(r revision) string { return numberKey(r.Number) })
		return struct {
			ID         string `json:"id"`
			MergedInto string `json:"merged_into,omitempty"`
			paging.Page[revision]
		}{ID: m.ID, MergedInto: m.MergedInto, Page: page}, nil
	case "day":
		return book.QueryDay(q.Date)
	case "week":
		return book.QueryWeek(q.Date)
	case "captures":
		items, more, err := book.QueryCaptures(q.Date, after, q.Limit)
		if err != nil {
			return nil, err
		}
		page := paging.Page[domain.PageSegment]{Items: items}
		if more {
			page.NextCursor = paging.Encode(scope, items[len(items)-1].Ref)
		}
		return struct {
			CaptureRevision uint64 `json:"capture_revision"`
			paging.Page[domain.PageSegment]
		}{CaptureRevision: book.BusinessDay(q.Date).CaptureRevision, Page: page}, nil
	case "deletions":
		items := book.DeletionTasks()
		slices.SortFunc(items, func(a, b domain.DeletionTask) int { return strings.Compare(a.ID, b.ID) })
		// 删除未完成时只展示进度，不提前交付候选 Memory 或内部故障信息。
		type deletion struct {
			ID          string                   `json:"id"`
			Date        string                   `json:"date"`
			Kind        string                   `json:"kind"`
			Status      string                   `json:"status"`
			CreatedAt   time.Time                `json:"created_at"`
			CompletedAt time.Time                `json:"completed_at,omitzero"`
			Candidates  []domain.MemoryCandidate `json:"candidates,omitempty"`
		}
		out := []deletion{}
		for _, item := range items {
			d := deletion{ID: item.ID, Date: item.Date, Kind: item.Kind, Status: "deleting", CreatedAt: item.CreatedAt}
			if item.Status == "completed" {
				d.Status, d.CompletedAt, d.Candidates = "completed", item.CompletedAt, item.Candidates
			}
			out = append(out, d)
		}
		return paging.Slice(out, q.Limit, after, scope, func(d deletion) string { return d.ID }), nil
	}
	return nil, ErrInvalid
}

func numberKey(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 20-len(s)) + s
}

func commitmentKey(m *domain.Commitment) string {
	due := m.DueDate
	if due == "" {
		due = "9999-99-99"
	}
	return due + "\x00" + m.SpokenAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "\x00" + m.ID
}

// CommitmentView 把既有修订序号交给 App，用于下一次用户修改的条件。
type CommitmentView struct {
	domain.Commitment
	Rev int `json:"rev"`
}
