// Package command 提供受信应用调用的用户命令，领域校验与版本检查都在同一 owner 事务内。
package command

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"persona/domain"
	"persona/query"
	"persona/store"
)

type Writer interface {
	Execute(context.Context, store.Owner, Request) (json.RawMessage, error)
}

type Request struct {
	Operation     string            `json:"-"`
	ID            string            `json:"id,omitempty"`
	Date          string            `json:"date,omitempty"`
	Rev           json.RawMessage   `json:"rev,omitempty"`
	Status        string            `json:"status,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
	Project       string            `json:"project,omitempty"`
	NextSteps     []string          `json:"next_steps,omitempty"`
	OpenQuestions []string          `json:"open_questions,omitempty"`
	From          string            `json:"from,omitempty"`
	To            string            `json:"to,omitempty"`
	MemoryID      string            `json:"memory_id,omitempty"`
}

type Service struct {
	Store store.Store
	Now   func() time.Time
}

func (s *Service) Execute(ctx context.Context, owner store.Owner, in Request) (json.RawMessage, error) {
	if owner.BusinessLine == "" || owner.EndUser == "" {
		return nil, domain.ErrUserInvalid
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	var out any
	apply := func(book *domain.Book) error {
		book.SetCaptureClock(func() time.Time { return now })
		var err error
		switch in.Operation {
		case "commitment":
			var rev int
			if json.Unmarshal(in.Rev, &rev) != nil || rev <= 0 {
				return domain.ErrUserInvalid
			}
			var m domain.Commitment
			m, err = book.UserEditCommitment(in.ID, rev, in.Status, in.Fields, now)
			out = query.CommitmentView{Commitment: m, Rev: m.Rev()}
		case "resolve", "project_status":
			var rev string
			if json.Unmarshal(in.Rev, &rev) != nil || rev == "" {
				return domain.ErrUserInvalid
			}
			if in.Operation == "resolve" {
				out, err = book.UserResolve(in.Date, in.ID, rev, in.Project, now)
			} else {
				out, err = book.UserSetStatus(in.ID, rev, domain.ProjectStatusChange{Status: in.Status, NextSteps: in.NextSteps, OpenQuestions: in.OpenQuestions}, now)
			}
		case "erase", "erase_day":
			if _, err := time.Parse(time.DateOnly, in.Date); err != nil {
				return domain.ErrUserInvalid
			}
			var task domain.DeletionTask
			task, err = book.UserErase(in.Date, in.From, in.To, in.Operation == "erase_day", now)
			// 未完成的待办绝不提前返回 Memory 候选或宣称已删除。
			status := "deleting"
			if task.Status == "completed" {
				status = "completed"
			}
			out = map[string]string{"id": task.ID, "status": status}
		case "confirm_memory":
			err = book.ConfirmMemoryCandidate(in.ID, in.MemoryID)
			out = struct{}{}
		default:
			return domain.ErrUserInvalid
		}
		return err
	}
	transact := s.Store.Update
	if in.Operation == "confirm_memory" {
		transact = s.Store.View
	}
	if err := transact(ctx, owner.BusinessLine, owner.EndUser, apply); err != nil {
		return nil, err
	}
	data, err := json.Marshal(out)
	return data, err
}

func WriteError(w http.ResponseWriter, err error) {
	status, code := 503, "unavailable"
	var remote *query.Error
	switch {
	case errors.As(err, &remote):
		status, code = remote.Status, remote.Code
	case errors.Is(err, domain.ErrUserInvalid):
		status, code = 400, "invalid_request"
	case errors.Is(err, domain.ErrDeletionPending):
		status, code = 409, "deletion_pending"
	case errors.Is(err, domain.ErrUserConflict):
		status, code = 409, "revision_conflict"
	case errors.Is(err, domain.ErrQueryNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, store.ErrOwnerDeleting):
		status, code = 403, "account_deleting"
	case errors.Is(err, domain.ErrDayDeleted):
		status, code = 410, "deleted"
	case errors.Is(err, domain.ErrCapturesExpired):
		status, code = 410, "expired"
	}
	query.WriteJSON(w, status, map[string]string{"error": code})
}
