// Package pgoutbox 的写入只使用调用方的事务，不自行提交。
package pgoutbox

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/outbox"
	"yanshi/internal/pg"
)

type Store struct {
	Shard    int
	Pool     *pgxpool.Pool
	Notifier *pg.Notifier
}

var _ outbox.Store = Store{}

func Insert(ctx context.Context, tx pgx.Tx, envelopes []outbox.Envelope, notifiers ...*pg.Notifier) error {
	for _, e := range envelopes {
		if err := outbox.Validate(e); err != nil {
			return err
		}
		var seq, endUser, runID, callID, agent, version, reason, request any
		if e.Kind == outbox.SessionDeleted {
			reason, request = e.Reason, optional(e.DeletionRequestID)
		} else {
			seq, endUser = int64(e.Seq), e.EndUser
			runID, callID = optional(e.RunID), optional(e.CallID)
			agent, version = optional(e.Agent), optional(e.AgentVersion)
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox
			(envelope_id, business_line, session_id, seq, kind, end_user, run_id, call_id, agent, agent_version, reason, deletion_request_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			e.EnvelopeID, e.BusinessLine, e.SessionID, seq, string(e.Kind), endUser, runID, callID, agent, version, reason, request)
		if err != nil {
			return err
		}
	}
	var n *pg.Notifier
	if len(notifiers) > 0 {
		n = notifiers[0]
	}
	seen := map[string]bool{}
	for _, e := range envelopes {
		if seen[e.BusinessLine] {
			continue
		}
		seen[e.BusinessLine] = true
		// 固定附加版本值，防止业务线名称末尾的冒号数字被 Notifier 当作版本。
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1,$2)`, n.Channel(pg.TopicOutbox, e.BusinessLine), e.BusinessLine+":0"); err != nil {
			return err
		}
	}
	return nil
}

func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s Store) List(ctx context.Context, businessLine string) ([]outbox.Envelope, error) {
	rows, err := s.Pool.Query(ctx, `SELECT envelope_id, business_line, session_id, coalesce(seq,0), kind,
		coalesce(end_user,''), coalesce(run_id,''), coalesce(call_id,''), coalesce(agent,''), coalesce(agent_version,''),
		coalesce(reason,''), coalesce(deletion_request_id,'') FROM outbox WHERE business_line=$1 ORDER BY id`, businessLine)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (outbox.Envelope, error) {
		var e outbox.Envelope
		err := r.Scan(&e.EnvelopeID, &e.BusinessLine, &e.SessionID, &e.Seq, &e.Kind, &e.EndUser,
			&e.RunID, &e.CallID, &e.Agent, &e.AgentVersion, &e.Reason, &e.DeletionRequestID)
		return e, err
	})
}

func (s Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM outbox WHERE session_id=$1 AND kind <> 'session_deleted'`, sessionID)
	return err
}
