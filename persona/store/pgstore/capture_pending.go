package pgstore

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"persona/domain"
)

type pendingKey struct {
	date, session   string
	seq, generation uint64
}

func (t *captureTx) Pending(date, session string, seq, generation uint64) ([]domain.CaptureSegment, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	k := pendingKey{date, session, seq, generation}
	if v, ok := t.pendingWrites[k]; ok {
		return slices.Clone(v), nil
	}
	if t.pendingCleared[date] {
		return nil, nil
	}
	var data []byte
	err := t.tx.QueryRow(t.ctx, `SELECT payload FROM capture_pending
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3
		AND session_id=$4 AND seq=$5::text::numeric AND generation=$6::text::numeric`,
		t.bl, t.eu, date, []byte(session), number(seq), number(generation)).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, contextError(t.ctx, err)
	}
	var out []domain.CaptureSegment
	if err := unmarshalPayload(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *captureTx) PutPending(date, session string, seq, generation uint64, segments []domain.CaptureSegment) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.readOnly {
		if t.pendingWrites == nil {
			t.pendingWrites = map[pendingKey][]domain.CaptureSegment{}
		}
		t.pendingWrites[pendingKey{date, session, seq, generation}] = slices.Clone(segments)
		return nil
	}
	args := []any{t.bl, t.eu, date, []byte(session), number(seq), number(generation)}
	if segments == nil {
		_, err := t.tx.Exec(t.ctx, `DELETE FROM capture_pending
			WHERE business_line=$1 AND end_user=$2 AND business_day=$3
			AND session_id=$4 AND seq=$5::text::numeric AND generation=$6::text::numeric`, args...)
		return contextError(t.ctx, err)
	}
	data, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	args = append(args, encodePayload(data))
	_, err = t.tx.Exec(t.ctx, `INSERT INTO capture_pending
		(business_line,end_user,business_day,session_id,seq,generation,payload)
		VALUES ($1,$2,$3,$4,$5::text::numeric,$6::text::numeric,$7::jsonb)
		ON CONFLICT (business_line,end_user,business_day,session_id,seq,generation)
		DO UPDATE SET payload=excluded.payload`, args...)
	return contextError(t.ctx, err)
}

func (t *captureTx) ClearPending(date string) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.readOnly {
		if t.pendingCleared == nil {
			t.pendingCleared = map[string]bool{}
		}
		t.pendingCleared[date] = true
		for k := range t.pendingWrites {
			if k.date == date {
				delete(t.pendingWrites, k)
			}
		}
		return nil
	}
	_, err := t.tx.Exec(t.ctx, `DELETE FROM capture_pending
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3`, t.bl, t.eu, date)
	return contextError(t.ctx, err)
}
