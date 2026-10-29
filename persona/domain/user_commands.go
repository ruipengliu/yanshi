package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

var ErrUserConflict = errors.New("revision conflict")
var ErrUserInvalid = errors.New("invalid user command")
var ErrDeletionPending = errors.New("deletion pending")

// 直接事实与交谈没有承诺的修订序号；内容版本覆盖模型改动和重新归集，避免另存易漂移的计数。
func contentRevision(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type ProjectQuery struct {
	Project
	Rev string `json:"rev"`
}

type ExchangeQuery struct {
	Exchange
	Rev string `json:"rev"`
}

func ProjectForUser(p Project) ProjectQuery { return ProjectQuery{Project: p, Rev: contentRevision(p)} }
func ExchangeForUser(e Exchange) ExchangeQuery {
	return ExchangeQuery{Exchange: e, Rev: contentRevision(e)}
}

// UserEditCommitment 在 owner 事务中检查客户端所见修订；继续使用用户变更集及既有字段锁。
func (u *Book) UserEditCommitment(id string, rev int, status string, fields map[string]string, now time.Time) (Commitment, error) {
	m := u.commitment(id)
	if m == nil {
		return Commitment{}, ErrQueryNotFound
	}
	if m.Rev() != rev || m.MergedInto != "" {
		return Commitment{}, ErrUserConflict
	}
	if status != "" {
		if len(fields) != 0 || (status != "done" && status != "cancelled") {
			return Commitment{}, ErrUserInvalid
		}
		if err := u.Edit(id, status, "", now); err != nil {
			return Commitment{}, err
		}
		return u.QueryCommitment(id)
	}
	if len(fields) == 0 {
		return Commitment{}, ErrUserInvalid
	}
	fields = maps.Clone(fields)
	for name, value := range fields {
		switch name {
		case "content":
			if strings.TrimSpace(value) == "" {
				return Commitment{}, ErrUserInvalid
			}
		case "counterpart", "due_text":
		case "project":
			if value != "" && u.project(value) == nil {
				return Commitment{}, ErrQueryNotFound
			}
		case "due_date":
			if value != "" {
				if _, err := time.Parse(time.DateOnly, value); err != nil {
					return Commitment{}, ErrUserInvalid
				}
			}
		default:
			return Commitment{}, ErrUserInvalid
		}
	}
	if _, ok := fields["due_date"]; ok {
		fields["due_text"] = ""
	}
	cs := Changeset{Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: now,
		ReviseCommitments: []CommitmentChange{{ID: id, Rev: rev, Fields: fields}}}
	u.commit(&cs, -1, nil)
	return u.QueryCommitment(id)
}

func (u *Book) UserResolve(date, id, rev, project string, now time.Time) (ExchangeQuery, error) {
	var found *Exchange
	for i := range u.exchanges {
		e := &u.exchanges[i]
		if e.Date == date && e.ID == id && e.Live() {
			if found != nil {
				return ExchangeQuery{}, ErrUserConflict
			}
			found = e
		}
	}
	if found == nil {
		return ExchangeQuery{}, ErrQueryNotFound
	}
	if rev == "" || contentRevision(*found) != rev || !found.Pending() {
		return ExchangeQuery{}, ErrUserConflict
	}
	if project != "" && u.project(project) == nil {
		return ExchangeQuery{}, ErrQueryNotFound
	}
	session := found.Session
	if err := u.Resolve(session, id, project, now); err != nil {
		return ExchangeQuery{}, err
	}
	return ExchangeForUser(*u.exchange(session, id)), nil
}

// ProjectStatusChange 保存用户修改的出处；项目仍是直接事实，不参与交谈与承诺的 refold。
type ProjectStatusChange struct {
	Project       string   `json:"project"`
	Status        string   `json:"status"`
	NextSteps     []string `json:"next_steps"`
	OpenQuestions []string `json:"open_questions"`
}

func (u *Book) UserSetStatus(id, rev string, change ProjectStatusChange, now time.Time) (ProjectQuery, error) {
	p := u.project(id)
	if p == nil {
		return ProjectQuery{}, ErrQueryNotFound
	}
	if rev == "" || contentRevision(*p) != rev || !p.Active() {
		return ProjectQuery{}, ErrUserConflict
	}
	change.Project = id
	change.NextSteps, change.OpenQuestions = slices.Clone(change.NextSteps), slices.Clone(change.OpenQuestions)
	cs := Changeset{Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: now, ProjectStatus: &change}
	u.commit(&cs, -1, nil)
	p.Status, p.NextSteps, p.OpenQuestions, p.StatusAt = change.Status, change.NextSteps, change.OpenQuestions, now
	// 同值修改也推进版本，避免重试与后来的用户写入共用旧版本。
	p.UserChangeset = cs.ID
	return ProjectForUser(*p), nil
}

// ConfirmMemoryCandidate 只确认完成待办中的候选资格，实际删除必须调用内核的 owner 受限接口。
func (u *Book) ConfirmMemoryCandidate(taskID, memoryID string) error {
	for _, task := range u.deletionTasks {
		if task.ID != taskID {
			continue
		}
		if task.Status != "completed" {
			return ErrDeletionPending
		}
		for _, candidate := range task.Candidates {
			if candidate.ID == memoryID {
				return nil
			}
		}
		return ErrQueryNotFound
	}
	return ErrQueryNotFound
}

func (u *Book) UserErase(date, from, to string, whole bool, now time.Time) (DeletionTask, error) {
	if u.captureDays[date] == nil {
		return DeletionTask{}, ErrQueryNotFound
	}
	if whole {
		return u.EraseDay(date, now)
	}
	f, ok1 := parseCaptureRef(strings.TrimPrefix(from, "#"))
	t, ok2 := parseCaptureRef(strings.TrimPrefix(to, "#"))
	if !ok1 || !ok2 || !refLE(f, t) {
		return DeletionTask{}, ErrUserInvalid
	}
	for _, ref := range []Ref{f, t} {
		found, err := u.containsRef(date, ref)
		if err != nil {
			return DeletionTask{}, err
		}
		if !found {
			return DeletionTask{}, ErrUserInvalid
		}
	}
	return u.Erase(date, from, to, now)
}
