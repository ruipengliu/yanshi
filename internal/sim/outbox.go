package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/session"
)

// checkOutbox 双向核对日志与信封：既检查漏写，也检查失败提交留下的孤立信封。
// 删除期间普通行允许提前清除；完成后必须只留下删除信封。
func (w *World) checkOutbox() error {
	ctx := context.Background()
	rows, err := w.stores.Outbox.List(ctx, "bl")
	if err != nil {
		return err
	}
	bySession := map[string][]outbox.Envelope{}
	for _, sid := range w.known() {
		bySession[sid] = nil
	}
	for _, sid := range w.deleted {
		bySession[sid] = nil
	}
	for _, e := range rows {
		bySession[e.SessionID] = append(bySession[e.SessionID], e)
		w.observeEnvelope(e)
	}
	for sid, actual := range bySession {
		tomb, err := w.deletions.Get(ctx, sid)
		if err != nil && !errors.Is(err, lifecycle.ErrNotFound) {
			return err
		}
		deleted := err == nil
		events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
		if err != nil {
			return err
		}
		expected := map[string]outbox.Envelope{}
		st := &session.State{SessionID: sid}
		for _, event := range events {
			if e := session.SelectEnvelope(st, event); e != nil {
				expected[e.EnvelopeID] = *e
			}
			if err := st.Apply(event); err != nil {
				return err
			}
		}
		deletions := 0
		seen := map[string]bool{}
		for _, e := range actual {
			if seen[e.EnvelopeID] {
				return fmt.Errorf("invariant: duplicate envelope %s", e.EnvelopeID)
			}
			seen[e.EnvelopeID] = true
			if e.Kind == outbox.SessionDeleted {
				deletions++
				if !deleted || e != outbox.Deleted(tomb.BusinessLine, sid, tomb.Reason, tomb.RequestID) {
					return fmt.Errorf("invariant: deletion envelope %s differs from Mark", e.EnvelopeID)
				}
				continue
			}
			if deleted && !tomb.CompletedAt.IsZero() {
				return fmt.Errorf("invariant: deleted session %s retains envelope %s", sid, e.EnvelopeID)
			}
			if want, ok := expected[e.EnvelopeID]; !ok || want != e {
				return fmt.Errorf("invariant: envelope %s has no matching committed event", e.EnvelopeID)
			}
			delete(expected, e.EnvelopeID)
		}
		if deleted && deletions != 1 && !(deletions == 0 && w.prunedEnvelopes[outbox.ID(sid, 0, outbox.SessionDeleted)]) {
			return fmt.Errorf("invariant: session %s has %d deletion envelopes", sid, deletions)
		}
		for id := range expected {
			if w.prunedEnvelopes[id] {
				delete(expected, id)
			}
		}
		if !deleted && len(expected) != 0 {
			return fmt.Errorf("invariant: session %s lacks %d envelopes", sid, len(expected))
		}
	}
	return nil
}

func (w *World) observeEnvelope(e outbox.Envelope) {
	if w.observedEnvelopes == nil {
		w.observedEnvelopes = map[string]bool{}
	}
	if w.observedEnvelopes[e.EnvelopeID] {
		return
	}
	w.observedEnvelopes[e.EnvelopeID] = true
	switch e.Kind {
	case outbox.RunCompleted:
		w.Stats.OutboxRunCompleted++
	case outbox.RunFailed:
		w.Stats.OutboxRunFailed++
	case outbox.Question:
		w.Stats.OutboxQuestions++
	case outbox.ApprovalRequested:
		w.Stats.OutboxApprovals++
	case outbox.BackgroundSettled:
		w.Stats.OutboxBackgroundSettled++
	case outbox.SessionClosed:
		w.Stats.OutboxSessionClosed++
	case outbox.SessionDeleted:
		w.Stats.OutboxSessionDeleted++
	}
}

// 差分比较逻辑字段与插入顺序，不比较各数据库独立分配的 xid 与行号。
func (w *World) fingerprintOutbox(dst io.Writer) {
	rows, err := w.stores.Outbox.List(context.Background(), "bl")
	if err != nil {
		fmt.Fprintf(dst, "outbox error: %v", err)
		return
	}
	if w.opts.CompareShards {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].SessionID != rows[j].SessionID {
				return rows[i].SessionID < rows[j].SessionID
			}
			if rows[i].Seq != rows[j].Seq {
				return rows[i].Seq < rows[j].Seq
			}
			return rows[i].EnvelopeID < rows[j].EnvelopeID
		})
	}
	if rows == nil {
		rows = []outbox.Envelope{}
	}
	_ = json.NewEncoder(dst).Encode(rows)
}
