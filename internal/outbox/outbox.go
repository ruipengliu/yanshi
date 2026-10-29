// Package outbox 保存与 Event 或删除记录同事务提交的信封（ADR-0046）。
package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

type Kind string

const (
	RunCompleted      Kind = "run_completed"
	RunFailed         Kind = "run_failed"
	Question          Kind = "question"
	ApprovalRequested Kind = "approval_requested"
	BackgroundSettled Kind = "background_settled"
	SessionClosed     Kind = "session_closed"
	SessionDeleted    Kind = "session_deleted"
)

// Envelope 不含内容；删除信封的 Seq 为零、EndUser 与 Agent 为空。
// Agent 与 AgentVersion 分开保存，避免拼接名称与版本产生歧义。
type Envelope struct {
	EnvelopeID        string `json:"envelope_id"`
	BusinessLine      string `json:"business_line"`
	EndUser           string `json:"end_user,omitempty"`
	SessionID         string `json:"session_id"`
	Seq               uint64 `json:"seq,omitempty"`
	Kind              Kind   `json:"kind"`
	RunID             string `json:"run_id,omitempty"`
	CallID            string `json:"call_id,omitempty"`
	Agent             string `json:"agent,omitempty"`
	AgentVersion      string `json:"agent_version,omitempty"`
	Reason            string `json:"reason,omitempty"`
	DeletionRequestID string `json:"deletion_request_id,omitempty"`
}

// ID 不消耗执行路径的 ID 生成器，故新增出站存储不改变模拟种子的轨迹。
// 删除使用独立命名空间，不冒充 seq=0 的 Event。
func ID(sessionID string, seq uint64, kind Kind) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d:%s", len(sessionID), sessionID, seq, kind)))
	return "out_" + hex.EncodeToString(h[:])
}

func Deleted(businessLine, sessionID, reason, requestID string) Envelope {
	return Envelope{EnvelopeID: ID(sessionID, 0, SessionDeleted), BusinessLine: businessLine,
		SessionID: sessionID, Kind: SessionDeleted, Reason: reason, DeletionRequestID: requestID}
}

// Store 的写入仅由日志与删除记录的事务实现提供；不能在 Update 返回后补写。
type Store interface {
	Reader
	Maintainer
	// List 按插入行号列出本存储中指定业务线的可见行。它不提供消费游标或安全水位，
	// 仅用于诊断与不变量检查；消费方必须使用 Read 的安全水位游标。
	List(ctx context.Context, businessLine string) ([]Envelope, error)
	// DeleteSession 只清除普通信封，删除信封独立保留。
	DeleteSession(ctx context.Context, sessionID string) error
}

func Validate(e Envelope) error {
	if e.SessionID == "" || e.EnvelopeID != ID(e.SessionID, e.Seq, e.Kind) {
		return fmt.Errorf("outbox: invalid envelope identity")
	}
	for _, s := range []string{e.BusinessLine, e.EndUser, e.SessionID, e.RunID, e.CallID, e.Agent, e.AgentVersion, e.Reason, e.DeletionRequestID} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return fmt.Errorf("outbox: invalid text encoding")
		}
	}
	switch e.Kind {
	case SessionDeleted:
		if e.Seq != 0 || e.EndUser != "" || e.RunID != "" || e.CallID != "" || e.Agent != "" || e.AgentVersion != "" {
			return fmt.Errorf("outbox: deletion contains event identity")
		}
	case RunCompleted, RunFailed, Question, ApprovalRequested, BackgroundSettled, SessionClosed:
		if e.Seq == 0 || e.Reason != "" || e.DeletionRequestID != "" {
			return fmt.Errorf("outbox: invalid ordinary envelope")
		}
	default:
		return fmt.Errorf("outbox: unknown kind")
	}
	return nil
}

// ValidateAppend 防止事务错误地把其他 Session 或批次的信封带入。
func ValidateAppend(sessionID string, expectedSeq uint64, count int, envelopes []Envelope) error {
	for _, e := range envelopes {
		if err := Validate(e); err != nil {
			return err
		}
		if e.SessionID != sessionID || e.Kind == SessionDeleted || e.Seq <= expectedSeq || e.Seq > expectedSeq+uint64(count) {
			return fmt.Errorf("outbox: envelope outside event batch")
		}
	}
	return nil
}
