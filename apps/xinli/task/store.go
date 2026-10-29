// Package task 保存应用自己的编排意图，不承担 Persona 领域事实或内核执行日志的职责。
package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"xinli/kernel"
)

type Owner struct {
	BusinessLine string
	EndUser      string
}

type Source struct {
	Owner
	Kind    string
	ID      string
	Version uint64
}

// Spec 只保存固定命令所需的标识与日期，不复制领域正文。
type Spec struct {
	Operation string
	Target    string
	Date      string
}

type State string

const (
	Pending    State = "pending"
	Unknown    State = "unknown"
	Accepted   State = "accepted"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Terminated State = "terminated"
)

type Task struct {
	Source
	Spec
	TaskID           string
	InputID          string
	Sequence         int64
	State            State
	Session          string
	LeaseGeneration  int64
	LeaseUntil       time.Time
	RetryAt          time.Time
	Attempt          int
	CommandStartedAt time.Time
	Retirements      int
	Receipt          kernel.InputReceipt
	LastErrorClass   string
}

func (t Task) Terminal() bool {
	return t.State == Succeeded || t.State == Failed || t.State == Terminated
}

// Identity 包含 owner；Persona 的事实 ID 只在 owner 内唯一。数组编码避免分隔符歧义。
func Identity(source Source, spec Spec) string {
	b, _ := json.Marshal([]any{source.BusinessLine, source.EndUser, source.ID, source.Version, spec.Operation, spec.Target})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func New(source Source, spec Spec, now time.Time) Task {
	id := Identity(source, spec)
	return Task{Source: source, Spec: spec, TaskID: id, InputID: "xinli_" + id, State: Pending, RetryAt: now}
}

var (
	ErrNotFound   = errors.New("task: not found")
	ErrLeaseLost  = errors.New("task: lease lost")
	ErrTargetBusy = errors.New("task: target busy")
	ErrInvalid    = errors.New("task: invalid transition")
)

// Store 的 Receive 同事务冻结整个来源的映射（包括空映射）；成功后调用方才能 ack。
// Claim 是短事务；Save 同时校验租约代数和期限，release=false 用于调用前持久化未知意图。
// Session 槽位跨退避及租约过期保留；命令明确完成或旧 Session 已退役，才允许释放。
type Store interface {
	CreationStore
	SessionTask(context.Context, Owner, string) (Task, bool, error)
	Receive(context.Context, Source, []Spec, time.Time) error
	Get(context.Context, string) (Task, error)
	Due(context.Context, time.Time, int) ([]string, error)
	Claim(context.Context, string, time.Time, time.Duration) (Task, bool, error)
	Save(context.Context, Task, time.Time, bool) error
	FailureCount(context.Context, Owner) (int64, error)
}

func ValidSource(s Source, specs []Spec, now time.Time) bool {
	if s.BusinessLine == "" || s.EndUser == "" || s.Kind == "" || s.ID == "" || s.Version == 0 || s.Version > 1<<63-1 || now.IsZero() {
		return false
	}
	for _, spec := range specs {
		if spec.Operation == "" || spec.Target == "" {
			return false
		}
	}
	return true
}

func Ready(t Task, now time.Time) bool {
	return !t.Terminal() && !t.RetryAt.After(now) && !t.LeaseUntil.After(now)
}

// 只有明确完成的 Run 释放槽位；失败/中断后潜在的旧远端写入由 A9 的退役处理。
func HoldsSession(t Task) bool {
	if t.State == Terminated {
		return false
	}
	return t.Session != "" && (t.State == Unknown || t.State == Accepted || (t.Receipt.RunID != "" && t.Receipt.Terminal != "completed"))
}

func ValidSave(old, next Task, now time.Time) error {
	if old.LeaseGeneration != next.LeaseGeneration || !old.LeaseUntil.After(now) || old.Terminal() {
		return ErrLeaseLost
	}
	if old.Source != next.Source || old.Spec != next.Spec || old.InputID != next.InputID || old.Sequence != next.Sequence || old.Attempt != next.Attempt || old.LeaseUntil != next.LeaseUntil {
		return ErrInvalid
	}
	reset := next.Retirements == old.Retirements+1 && old.Session != "" && next.Session == "" && next.State == Pending && next.Receipt == (kernel.InputReceipt{}) && next.CommandStartedAt.IsZero()
	if next.Retirements != old.Retirements && !reset {
		return ErrInvalid
	}
	if !reset && !old.CommandStartedAt.IsZero() && !old.CommandStartedAt.Equal(next.CommandStartedAt) {
		return ErrInvalid
	}
	if old.Session != "" && next.Session != old.Session && !reset {
		return ErrInvalid
	}
	if next.State != Pending && next.State != Unknown && next.State != Accepted && !next.Terminal() {
		return ErrInvalid
	}
	if old.State == Unknown && next.State == Pending && !reset {
		return ErrInvalid
	}
	if old.State == Accepted && next.State != Accepted && !next.Terminal() && !reset {
		return ErrInvalid
	}
	if (next.State == Unknown || next.State == Accepted) && next.Session == "" {
		return ErrInvalid
	}
	if next.State == Accepted && (next.Receipt.RunID == "" || next.Receipt.InputID != next.InputID) {
		return ErrInvalid
	}
	if next.Receipt.RunID != "" && next.Receipt.InputID != next.InputID {
		return ErrInvalid
	}
	if len(next.LastErrorClass) > 64 {
		return ErrInvalid
	}
	for i, c := range next.LastErrorClass {
		if (c < 'a' || c > 'z') && (i == 0 || ((c < '0' || c > '9') && c != '_')) {
			return ErrInvalid
		}
	}
	return nil
}
