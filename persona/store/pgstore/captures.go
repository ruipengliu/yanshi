package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"persona/domain"
)

type batchKey struct {
	date   string
	number uint64
}

type erasedRange struct {
	date     string
	from, to domain.Ref
}

type captureTx struct {
	ctx      context.Context
	tx       pgx.Tx
	bl, eu   []byte
	closed   bool
	readOnly bool
	// View 允许演练领域命令，但数据库事务保持只读；仅保存本次追加及擦除范围。
	appended map[batchKey][]domain.CaptureSegment
	erased   []erasedRange
}

var _ domain.CaptureAccess = (*captureTx)(nil)

func newCaptureTx(ctx context.Context, tx pgx.Tx, bl, eu []byte, readOnly bool) *captureTx {
	return &captureTx{ctx: ctx, tx: tx, bl: bl, eu: eu, readOnly: readOnly, appended: map[batchKey][]domain.CaptureSegment{}}
}

func (t *captureTx) check() error {
	if t.closed {
		return domain.ErrCaptureTransactionClosed
	}
	return t.ctx.Err()
}

func (t *captureTx) close() {
	t.closed = true
	t.tx, t.ctx, t.appended, t.erased = nil, nil, nil, nil
}

func refLess(a, b domain.Ref) bool {
	return a.Seq < b.Seq || a.Seq == b.Seq && a.Index < b.Index
}

func inRange(ref, from, to domain.Ref) bool {
	return (from.Seq == 0 || !refLess(ref, from)) && (to.Seq == 0 || !refLess(to, ref))
}

func (t *captureTx) isErased(date string, ref domain.Ref) bool {
	for _, r := range t.erased {
		if r.date == date && inRange(ref, r.from, r.to) {
			return true
		}
	}
	return false
}

func number(n uint64) string { return strconv.FormatUint(n, 10) }

func (t *captureTx) Lookup(date string, ref domain.Ref) (domain.CaptureSegment, bool, error) {
	if err := t.check(); err != nil {
		return domain.CaptureSegment{}, false, err
	}
	if v, ok := t.appended[batchKey{date, ref.Seq}]; ok {
		if ref.Index < 0 || ref.Index >= len(v) || v[ref.Index].Text == "" {
			return domain.CaptureSegment{}, false, nil
		}
		return v[ref.Index], true, nil
	}
	if t.isErased(date, ref) {
		return domain.CaptureSegment{}, false, nil
	}
	var data []byte
	err := t.tx.QueryRow(t.ctx, `SELECT payload FROM capture_segments
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3
		AND batch_number=$4::text::numeric AND segment_number=$5
		AND live`, t.bl, t.eu, date, number(ref.Seq), int64(ref.Index)).Scan(&data)
	if err = contextError(t.ctx, err); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.CaptureSegment{}, false, nil
		}
		return domain.CaptureSegment{}, false, err
	}
	var segment domain.CaptureSegment
	if err := unmarshalPayload(data, &segment); err != nil {
		return domain.CaptureSegment{}, false, err
	}
	return segment, true, nil
}

func (t *captureTx) Batch(date string, seq uint64) ([]domain.CaptureSegment, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if v, ok := t.appended[batchKey{date, seq}]; ok {
		return slices.Clone(v), nil
	}
	rows, err := t.tx.Query(t.ctx, `SELECT payload FROM capture_segments
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3 AND batch_number=$4::text::numeric
		ORDER BY segment_number`, t.bl, t.eu, date, number(seq))
	if err != nil {
		return nil, contextError(t.ctx, err)
	}
	defer rows.Close()
	var result []domain.CaptureSegment
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, contextError(t.ctx, err)
		}
		var segment domain.CaptureSegment
		if data != nil && !t.isErased(date, domain.Ref{Seq: seq, Index: len(result)}) {
			if err := unmarshalPayload(data, &segment); err != nil {
				return nil, err
			}
		}
		result = append(result, segment)
	}
	if err := contextError(t.ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("capture batch does not exist")
	}
	return result, nil
}

// rangeSQL 的所有值都是参数；按联合主键做范围检索，避免 OFFSET 随页码增长。
func rangeSQL(args *[]any, from, to domain.Ref, exclusive bool) string {
	query := "TRUE"
	if from.Seq != 0 {
		op := ">="
		if exclusive {
			op = ">"
		}
		n := len(*args) + 1
		*args = append(*args, number(from.Seq), int64(from.Index))
		query += fmt.Sprintf(" AND (batch_number,segment_number) %s ($%d::text::numeric,$%d::bigint)", op, n, n+1)
	}
	if to.Seq != 0 {
		n := len(*args) + 1
		*args = append(*args, number(to.Seq), int64(to.Index))
		query += fmt.Sprintf(" AND (batch_number,segment_number) <= ($%d::text::numeric,$%d::bigint)", n, n+1)
	}
	return query
}

// 每次最多读取一个小页并关闭 Rows，再把片段交给领域代码。
// 即使 Pull2 暂停在 yield，事务结束时也没有遗留的 PG 结果集阻塞提交/回滚。
func (t *captureTx) records(date string, from, to domain.Ref, exclusive, reverse bool, limit int) ([]domain.CaptureRecord, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	args := []any{t.bl, t.eu, date}
	query := `SELECT batch_number::text,segment_number,payload FROM capture_segments
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3
		AND live AND ` + rangeSQL(&args, from, to, exclusive)
	for _, r := range t.erased {
		if r.date == date {
			query += " AND NOT (" + rangeSQL(&args, r.from, r.to, false) + ")"
		}
	}
	if reverse {
		query += " ORDER BY batch_number DESC,segment_number DESC"
	} else {
		query += " ORDER BY batch_number,segment_number"
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	rows, err := t.tx.Query(t.ctx, query, args...)
	if err != nil {
		return nil, contextError(t.ctx, err)
	}
	defer rows.Close()
	var result []domain.CaptureRecord
	for rows.Next() {
		var seq string
		var index int64
		var data []byte
		if err := rows.Scan(&seq, &index, &data); err != nil {
			return nil, contextError(t.ctx, err)
		}
		n, err := strconv.ParseUint(seq, 10, 64)
		if err != nil {
			return nil, err
		}
		r := domain.CaptureRecord{Ref: domain.Ref{Seq: n, Index: int(index)}}
		if err := unmarshalPayload(data, &r.Segment); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, contextError(t.ctx, rows.Err())
}

func (t *captureTx) overlay(date string, from, to domain.Ref) []domain.CaptureRecord {
	var result []domain.CaptureRecord
	for key, segments := range t.appended {
		if key.date != date {
			continue
		}
		for i, segment := range segments {
			ref := domain.Ref{Seq: key.number, Index: i}
			if segment.Text != "" && inRange(ref, from, to) {
				result = append(result, domain.CaptureRecord{Ref: ref, Segment: segment})
			}
		}
	}
	slices.SortFunc(result, func(a, b domain.CaptureRecord) int {
		if refLess(a.Ref, b.Ref) {
			return -1
		}
		if refLess(b.Ref, a.Ref) {
			return 1
		}
		return 0
	})
	return result
}

func (t *captureTx) Scan(date string, from, to domain.Ref) iter.Seq2[domain.CaptureRecord, error] {
	return func(yield func(domain.CaptureRecord, error) bool) {
		if err := t.check(); err != nil {
			yield(domain.CaptureRecord{}, err)
			return
		}
		virtual := t.overlay(date, from, to)
		var page []domain.CaptureRecord
		position, exclusive, exhausted := from, false, false
		const pageSize = 128
		for {
			if err := t.check(); err != nil {
				yield(domain.CaptureRecord{}, err)
				return
			}
			if len(page) == 0 && !exhausted {
				var err error
				page, err = t.records(date, position, to, exclusive, false, pageSize)
				if err != nil {
					yield(domain.CaptureRecord{}, err)
					return
				}
				exhausted = len(page) < pageSize
				if len(page) > 0 {
					position, exclusive = page[len(page)-1].Ref, true
				}
			}
			if len(page) == 0 && len(virtual) == 0 {
				return
			}
			var record domain.CaptureRecord
			if len(virtual) > 0 && (len(page) == 0 || refLess(virtual[0].Ref, page[0].Ref)) {
				record, virtual = virtual[0], virtual[1:]
			} else {
				record, page = page[0], page[1:]
			}
			if !yield(record, nil) {
				return
			}
			// 下一轮先检查寿命，再判断耗尽，覆盖暂停在最后一个片段的迭代器。
		}
	}
}

func (t *captureTx) Adjacent(date string, ref domain.Ref, before bool) (domain.CaptureRecord, bool, error) {
	if err := t.check(); err != nil {
		return domain.CaptureRecord{}, false, err
	}
	from, to := domain.Ref{}, domain.Ref{}
	if before {
		to = ref
	} else {
		from = ref
	}
	// 向前时可能先取到 ref 本身，最多读两条即可得到严格前驱。
	limit := 1
	if before && ref.Seq != 0 {
		limit = 2
	}
	base, err := t.records(date, from, to, !before, before, limit)
	if err != nil {
		return domain.CaptureRecord{}, false, err
	}
	candidates := append(base, t.overlay(date, from, to)...)
	var result domain.CaptureRecord
	found := false
	for _, r := range candidates {
		if ref.Seq != 0 && (before && !refLess(r.Ref, ref) || !before && !refLess(ref, r.Ref)) {
			continue
		}
		if !found || before && refLess(result.Ref, r.Ref) || !before && refLess(r.Ref, result.Ref) {
			result, found = r, true
		}
	}
	return result, found, nil
}

func (t *captureTx) Append(date string, seq uint64, segments []domain.CaptureSegment) error {
	if err := t.check(); err != nil {
		return err
	}
	if seq == 0 || len(segments) == 0 {
		return fmt.Errorf("invalid capture batch")
	}
	key := batchKey{date, seq}
	if _, exists := t.appended[key]; exists {
		return fmt.Errorf("capture batch already exists")
	}
	var exists bool
	if err := t.tx.QueryRow(t.ctx, `SELECT EXISTS (SELECT 1 FROM capture_segments
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3 AND batch_number=$4::text::numeric)`,
		t.bl, t.eu, date, number(seq)).Scan(&exists); err != nil {
		return contextError(t.ctx, err)
	}
	if exists {
		return fmt.Errorf("capture batch already exists")
	}
	if t.readOnly {
		t.appended[key] = slices.Clone(segments)
		return nil
	}
	for i, segment := range segments {
		data, err := json.Marshal(segment)
		if err != nil {
			return err
		}
		if _, err := t.tx.Exec(t.ctx, `INSERT INTO capture_segments
			(business_line,end_user,business_day,batch_number,segment_number,payload,live)
			VALUES ($1,$2,$3,$4::text::numeric,$5,$6::jsonb,$7)`, t.bl, t.eu, date, number(seq), int64(i), encodePayload(data), segment.Text != ""); err != nil {
			return contextError(t.ctx, err)
		}
	}
	return nil
}

func (t *captureTx) Erase(date string, from, to domain.Ref) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.readOnly {
		t.erased = append(t.erased, erasedRange{date: date, from: from, to: to})
		for key, segments := range t.appended {
			if key.date == date {
				for i := range segments {
					if inRange(domain.Ref{Seq: key.number, Index: i}, from, to) {
						segments[i] = domain.CaptureSegment{}
					}
				}
			}
		}
		return nil
	}
	args := []any{t.bl, t.eu, date}
	query := `UPDATE capture_segments SET payload=NULL,live=false
		WHERE business_line=$1 AND end_user=$2 AND business_day=$3 AND payload IS NOT NULL AND ` + rangeSQL(&args, from, to, false)
	_, err := t.tx.Exec(t.ctx, query, args...)
	return contextError(t.ctx, err)
}
