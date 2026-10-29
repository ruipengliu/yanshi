package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"xinli/kernel"
	"xinli/task"
)

type Status string

const (
	Active   Status = "active"
	Disabled Status = "disabled"
	Deleting Status = "deleting"
	Deleted  Status = "deleted"
)

var (
	ErrNotFound     = errors.New("account: not found")
	ErrInactive     = errors.New("account: inactive")
	ErrUnauthorized = errors.New("account: unauthorized")
	ErrInvalid      = errors.New("account: invalid request")
)

type Account struct {
	task.Owner
	Status             Status          `json:"status"`
	TimeZone           string          `json:"time_zone"`
	Preferences        json.RawMessage `json:"preferences,omitempty"`
	Entitlement        string          `json:"entitlement"`
	EntitlementVersion uint64          `json:"entitlement_version"`
	SyncedVersion      uint64          `json:"synced_version"`
	EffectiveTier      kernel.Tier     `json:"effective_tier"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type Device struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform"`
	BoundAt   time.Time `json:"bound_at"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// Session 只保存随机凭据的 SHA-256，不保存能直接登录的明文。
type Session struct {
	ID               string
	DeviceID         string
	AccessHash       string
	RefreshHash      string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	CreatedAt        time.Time
	RevokedAt        time.Time
}

type Deletion struct {
	ReceiptHash     string    `json:"receipt_hash,omitempty"`
	Stage           string    `json:"stage"`
	KernelRequestID string    `json:"kernel_request_id,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at,omitzero"`
}

// Record 是一个 owner 的短事务快照；Enqueue 与账号变更一起提交。
// Cleanup 清掉所有应用任务和凭据，留下账号终态与注销记录阻止旧请求复活。
type Record struct {
	Account  Account
	Devices  map[string]Device
	Sessions map[string]Session
	Deletion Deletion
	Enqueue  []task.Task
	Cleanup  bool
}

func Clone(r Record) Record {
	r.Account.Preferences = append(json.RawMessage(nil), r.Account.Preferences...)
	devices := make(map[string]Device, len(r.Devices))
	for k, v := range r.Devices {
		devices[k] = v
	}
	sessions := make(map[string]Session, len(r.Sessions))
	for k, v := range r.Sessions {
		sessions[k] = v
	}
	r.Devices, r.Sessions, r.Enqueue, r.Cleanup = devices, sessions, nil, false
	return r
}

// WithOwner 跨进程串行化账号操作与在途动作，但不在网络调用期间持有数据库事务。
// 回调 context 必须继续传给存储；允许同 owner 嵌套，禁止跨 owner 嵌套。
type Store interface {
	WithOwner(context.Context, task.Owner, func(context.Context) error) error
	Register(context.Context, string, Account) (Account, error)
	ReadAccount(context.Context, task.Owner) (Record, error)
	UpdateAccount(context.Context, task.Owner, func(*Record) error) error
	TokenOwner(context.Context, string) (task.Owner, error)
	DeletionOwner(context.Context, string) (task.Owner, error)
}

type Guard interface {
	WithActive(context.Context, task.Owner, func(context.Context) error) error
}

// Enumerator 按不可变 EndUser ID 翻页，包含禁用、注销中和注销终态，恢复不能只扫描可登录账号。
type Enumerator interface {
	AccountOwners(context.Context, string, string, int) ([]task.Owner, error)
}

func ValidChange(old, next Record) bool {
	if old.Account.Owner != next.Account.Owner || next.Account.SyncedVersion > next.Account.EntitlementVersion {
		return false
	}
	switch old.Account.Status {
	case Active:
		if next.Account.Status != Active && next.Account.Status != Disabled && next.Account.Status != Deleting {
			return false
		}
	case Disabled:
		if next.Account.Status != Disabled && next.Account.Status != Deleting {
			return false
		}
	case Deleting:
		if next.Account.Status != Deleting && next.Account.Status != Deleted {
			return false
		}
	case Deleted:
		if next.Account.Status != Deleted {
			return false
		}
	default:
		return false
	}
	if next.Cleanup && (next.Account.Status != Deleted || next.Deletion.Stage != "completed") {
		return false
	}
	if next.Account.Status == Deleted && (len(next.Sessions) > 0 || len(next.Devices) > 0 || len(next.Account.Preferences) > 0 || len(next.Enqueue) > 0) {
		return false
	}
	for _, t := range next.Enqueue {
		if t.Owner != next.Account.Owner || !IsTask(t) || !task.ValidSource(t.Source, []task.Spec{t.Spec}, t.RetryAt) {
			return false
		}
		if t.Operation == TierOperation && next.Account.Status != Active || t.Operation == DeleteOperation && next.Account.Status != Deleting {
			return false
		}
	}
	return true
}
