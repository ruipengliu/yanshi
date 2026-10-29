package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"persona/domain"
)

type rowKind uint8

const (
	singleton rowKind = iota
	sequence
	mapping
	tombstone
)

type category struct {
	field string
	table string
	key   string
	kind  rowKind
}

// SQL 中的表名和键列只来自静态描述；只有固定大小的编号与集合形状是单例。
var categories = []category{
	{"Meta", "book_meta", "", singleton},
	{"Changesets", "changesets", "changeset_key", sequence},
	{"Exchanges", "exchanges", "exchange_key", sequence},
	{"Commitments", "commitments", "commitment_key", sequence},
	{"Projects", "projects", "project_key", sequence},
	{"Days", "days", "day_key", sequence},
	{"Weeks", "weeks", "week_key", sequence},
	{"Merges", "merges", "merge_key", sequence},
	{"CaptureDays", "business_days", "business_day", mapping},
	{"Tombstones", "erasure_tombstones", "erasure_key", tombstone},
	{"DeletionTasks", "deletion_tasks", "task_key", sequence},
	{"WeekSessions", "week_sessions", "week_start", mapping},
	{"Submitted", "submitted", "session_id", mapping},
}

type bookRow struct {
	key            string
	ordinal        string
	payload        json.RawMessage
	previousKey    string
	replaces       bool
	status         string
	rebuildPending bool
}

type bookSnapshot map[string][]bookRow

type storedMeta struct {
	Counters  json.RawMessage
	NilFields []string
}

type storedValue struct {
	Value json.RawMessage
}

type storedTombstone struct {
	Date   string
	Source domain.Source
}

// 先转成无损 JSON 值再规范化键顺序，避免 JSONB 的对象重排造成假更新。
func canonicalJSON(data []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func marshalRow(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(data)
}

// 编码的是字段元组而非分隔符拼接；出现重复夹具实体时用同身份内的出现次序区分。
func entityKey(parts ...string) string {
	data, _ := json.Marshal(parts)
	return string(data)
}

func sessionScopes(parts bookParts) (map[string]string, error) {
	var days map[string]struct {
		Session  string
		Sessions []string
	}
	if err := json.Unmarshal(parts["CaptureDays"], &days); err != nil {
		return nil, err
	}
	scopes := map[string]string{}
	for date, day := range days {
		for _, session := range append(day.Sessions, day.Session) {
			if session != "" {
				scopes[session] = date
			}
		}
	}
	return scopes, nil
}

func scopeKey(session string, scopes map[string]string) string {
	if date := scopes[session]; date != "" {
		return entityKey("day", date)
	}
	return entityKey("session", session)
}

func rowIdentity(field string, raw json.RawMessage, scopes map[string]string) (string, error) {
	var item struct {
		ID, Session, Date string
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", err
	}
	switch field {
	case "Exchanges":
		return entityKey(scopeKey(item.Session, scopes), item.ID), nil
	case "Days":
		return entityKey(item.Date, scopeKey(item.Session, scopes)), nil
	case "Weeks":
		// Week 尚无周起点字段，领域的重交身份是 Session，不能由 FiledAt 猜周。
		return entityKey(item.Session), nil
	default:
		return entityKey(item.ID), nil
	}
}

func changesetSource(raw json.RawMessage, scopes map[string]string) (string, error) {
	var change domain.Changeset
	if err := json.Unmarshal(raw, &change); err != nil {
		return "", err
	}
	if change.Author == domain.AuthorDay {
		scope := scopeKey(change.Session, scopes)
		if change.Day != "" {
			scope = entityKey("day", change.Day)
		}
		return entityKey(change.Author, scope, change.Key), nil
	}
	return entityKey(change.Author, change.Session, change.Key), nil
}

func snapshotRows(book *domain.Book, before bookSnapshot) (bookSnapshot, error) {
	parts, err := splitBook(book)
	if err != nil {
		return nil, err
	}
	scopes, err := sessionScopes(parts)
	if err != nil {
		return nil, err
	}
	after := bookSnapshot{}
	meta := storedMeta{Counters: parts["Meta"]}
	for _, c := range categories {
		if c.kind != singleton && bytes.Equal(parts[c.field], []byte("null")) {
			meta.NilFields = append(meta.NilFields, c.field)
		}
		var rows []bookRow
		switch c.kind {
		case singleton:
			continue
		case sequence:
			var values []json.RawMessage
			if err := json.Unmarshal(parts[c.field], &values); err != nil {
				return nil, err
			}
			occurrences := map[string]int{}
			for _, value := range values {
				identity, err := rowIdentity(c.field, value, scopes)
				if err != nil {
					return nil, err
				}
				key := entityKey(identity, strconv.Itoa(occurrences[identity]))
				occurrences[identity]++
				payload, err := canonicalJSON(value)
				if err != nil {
					return nil, err
				}
				row := bookRow{key: key, payload: payload}
				if c.field == "DeletionTasks" {
					var task domain.DeletionTask
					if err := json.Unmarshal(value, &task); err != nil {
						return nil, err
					}
					row.status, row.rebuildPending = task.Status, task.RebuildPending
				}
				rows = append(rows, row)
			}
		case mapping:
			var values bookParts
			if err := json.Unmarshal(parts[c.field], &values); err != nil {
				return nil, err
			}
			for key, value := range values {
				payload, err := canonicalJSON(value)
				if c.field != "CaptureDays" {
					// JSONB 标量字符串与 U+0000 的转义封装不能混淆。
					payload, err = marshalRow(storedValue{Value: value})
				}
				if err != nil {
					return nil, err
				}
				rows = append(rows, bookRow{key: key, payload: payload})
			}
			slices.SortFunc(rows, func(a, b bookRow) int { return strings.Compare(a.key, b.key) })
		case tombstone:
			var days map[string][]domain.Source
			if err := json.Unmarshal(parts[c.field], &days); err != nil {
				return nil, err
			}
			var dates []string
			for date := range days {
				dates = append(dates, date)
			}
			slices.Sort(dates)
			for _, date := range dates {
				occurrences := map[string]int{}
				for _, source := range days[date] {
					payload, err := marshalRow(storedTombstone{Date: date, Source: source})
					if err != nil {
						return nil, err
					}
					identity := string(payload)
					key := entityKey(identity, strconv.Itoa(occurrences[identity]))
					occurrences[identity]++
					rows = append(rows, bookRow{key: key, payload: payload})
				}
			}
		}
		if err := matchRows(c, rows, before[c.field], scopes); err != nil {
			return nil, err
		}
		after[c.field] = rows
	}
	payload, err := marshalRow(meta)
	if err != nil {
		return nil, err
	}
	after["Meta"] = []bookRow{{payload: payload}}
	return after, nil
}

func matchRows(c category, rows, before []bookRow, scopes map[string]string) error {
	old := map[string]bookRow{}
	bySource := map[string][]bookRow{}
	for _, row := range before {
		old[row.key] = row
		if c.field == "Changesets" {
			key, err := changesetSource(row.payload, scopes)
			if err != nil {
				return err
			}
			bySource[key] = append(bySource[key], row)
		}
	}
	used := map[string]bool{}
	// 先保留相同 ID 的行，再匹配换了 ID 的来源取代，不能抢走后面的保留项。
	for i := range rows {
		if previous, ok := old[rows[i].key]; ok {
			rows[i].previousKey, rows[i].replaces, rows[i].ordinal = previous.key, true, previous.ordinal
			used[previous.key] = true
		}
	}
	if c.field == "Changesets" {
		for i := range rows {
			if rows[i].replaces {
				continue
			}
			key, err := changesetSource(rows[i].payload, scopes)
			if err != nil {
				return err
			}
			for _, previous := range bySource[key] {
				if !used[previous.key] {
					rows[i].previousKey, rows[i].replaces, rows[i].ordinal = previous.key, true, previous.ordinal
					used[previous.key] = true
					break
				}
			}
		}
	}
	if c.kind == sequence || c.kind == tombstone {
		return assignOrdinals(rows)
	}
	return nil
}

func parseOrdinal(text string) (*big.Rat, error) {
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("invalid persona ordinal")
	}
	return value, nil
}

func ordinalText(value *big.Rat) string {
	// 序位由整数和二等分得到，分母只有 2 的幂，能够无损写成有限十进制。
	text := value.FloatString(value.Denom().BitLen())
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}

// 删除不压缩序位；新投影插进两个旧实体之间时取中点，避免改写后面的全部行。
// 领域把一个已有实体移到末尾时，只为这个移动的实体分配新序位。
func assignOrdinals(rows []bookRow) error {
	anchors := map[int]*big.Rat{}
	var last *big.Rat
	for i := range rows {
		if rows[i].ordinal == "" {
			continue
		}
		value, err := parseOrdinal(rows[i].ordinal)
		if err != nil {
			return err
		}
		if last == nil || value.Cmp(last) > 0 {
			anchors[i], last = value, value
		}
	}
	last = nil
	for i := 0; i < len(rows); {
		if value, ok := anchors[i]; ok {
			rows[i].ordinal, last = ordinalText(value), value
			i++
			continue
		}
		next := i + 1
		for next < len(rows) {
			if _, ok := anchors[next]; ok {
				break
			}
			next++
		}
		upper := anchors[next]
		for ; i < next; i++ {
			value := new(big.Rat)
			switch {
			case last == nil && upper != nil:
				value.Sub(upper, new(big.Rat).SetInt64(int64(next-i)))
			case upper == nil:
				if last != nil {
					value.Set(last)
				}
				value.Add(value, big.NewRat(1, 1))
			default:
				value.Add(last, upper).Quo(value, big.NewRat(2, 1))
			}
			rows[i].ordinal, last = ordinalText(value), value
		}
	}
	return nil
}

func bookFromRows(snapshot bookSnapshot) (*domain.Book, error) {
	if len(snapshot["Meta"]) != 1 {
		return nil, fmt.Errorf("persona owner is missing book metadata")
	}
	var meta storedMeta
	if err := json.Unmarshal(snapshot["Meta"][0].payload, &meta); err != nil {
		return nil, err
	}
	parts := bookParts{"Meta": meta.Counters}
	for _, c := range categories {
		rows := snapshot[c.field]
		switch c.kind {
		case sequence:
			var values []json.RawMessage
			if !slices.Contains(meta.NilFields, c.field) {
				values = []json.RawMessage{}
			}
			for _, row := range rows {
				values = append(values, row.payload)
			}
			data, err := json.Marshal(values)
			if err != nil {
				return nil, err
			}
			parts[c.field] = data
		case mapping:
			var values bookParts
			if !slices.Contains(meta.NilFields, c.field) || len(rows) > 0 {
				values = bookParts{}
			}
			for _, row := range rows {
				value := row.payload
				if c.field != "CaptureDays" {
					var stored storedValue
					if err := json.Unmarshal(value, &stored); err != nil {
						return nil, err
					}
					value = stored.Value
				}
				values[row.key] = value
			}
			data, err := json.Marshal(values)
			if err != nil {
				return nil, err
			}
			parts[c.field] = data
		}
	}
	var days map[string]bookParts
	if err := json.Unmarshal(parts["CaptureDays"], &days); err != nil {
		return nil, err
	}
	tombstones := map[string][]domain.Source{}
	for date, day := range days {
		// 空墓碑数组与 nil 的形状留在业务日这一行，不需要空的墓碑实体。
		if !bytes.Equal(day["Tombstones"], []byte("null")) {
			tombstones[date] = []domain.Source{}
		} else {
			tombstones[date] = nil
		}
	}
	for _, row := range snapshot["Tombstones"] {
		var value storedTombstone
		if err := json.Unmarshal(row.payload, &value); err != nil {
			return nil, err
		}
		if _, ok := days[value.Date]; !ok {
			return nil, fmt.Errorf("persona tombstone has no business day")
		}
		tombstones[value.Date] = append(tombstones[value.Date], value.Source)
	}
	data, err := json.Marshal(tombstones)
	if err != nil {
		return nil, err
	}
	parts["Tombstones"] = data
	return joinBook(parts)
}

func loadBook(ctx context.Context, tx pgx.Tx, bl, eu []byte) (*domain.Book, bookSnapshot, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM owners WHERE business_line=$1 AND end_user=$2)`, bl, eu).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if !exists {
		return domain.NewBook(), nil, nil
	}
	snapshot := bookSnapshot{}
	for _, c := range categories {
		rows, err := loadRows(ctx, tx, bl, eu, c)
		if err != nil {
			return nil, nil, fmt.Errorf("load persona %s: %w", c.table, err)
		}
		snapshot[c.field] = rows
	}
	book, err := bookFromRows(snapshot)
	return book, snapshot, err
}

func loadRows(ctx context.Context, tx pgx.Tx, bl, eu []byte, c category) ([]bookRow, error) {
	key, ordinal := `''::bytea`, `''::text`
	if c.kind != singleton {
		key = c.key
	}
	if c.kind == sequence || c.kind == tombstone {
		ordinal = "ordinal::text"
	}
	status, rebuild := `''::text`, "false"
	if c.field == "DeletionTasks" {
		status, rebuild = "status", "rebuild_pending"
	}
	query := "SELECT " + key + "," + ordinal + ",payload," + status + "," + rebuild + " FROM " + c.table + " WHERE business_line=$1 AND end_user=$2"
	if c.kind == sequence || c.kind == tombstone {
		// 限定底层列，避免 SELECT ordinal::text 的输出名让 ORDER BY 按文字排序。
		query += " ORDER BY " + c.table + ".ordinal," + c.table + "." + c.key
	} else if c.kind == mapping {
		query += " ORDER BY " + c.key
	}
	rows, err := tx.Query(ctx, query, bl, eu)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []bookRow
	for rows.Next() {
		var key, payload []byte
		var row bookRow
		if err := rows.Scan(&key, &row.ordinal, &payload, &row.status, &row.rebuildPending); err != nil {
			return nil, err
		}
		decoded, err := decodePayload(payload)
		if err != nil {
			return nil, err
		}
		row.payload, err = canonicalJSON(decoded)
		if err != nil {
			return nil, err
		}
		row.key = string(key)
		result = append(result, row)
	}
	return result, rows.Err()
}

func equalRow(a, b bookRow) bool {
	return a.key == b.key && a.ordinal == b.ordinal && bytes.Equal(a.payload, b.payload) && a.status == b.status && a.rebuildPending == b.rebuildPending
}

func saveBook(ctx context.Context, tx pgx.Tx, bl, eu []byte, before, after bookSnapshot) error {
	for _, c := range categories {
		old := map[string]bookRow{}
		for _, row := range before[c.field] {
			old[row.key] = row
		}
		retained := map[string]bool{}
		for _, row := range after[c.field] {
			if row.replaces {
				retained[row.previousKey] = true
			} else if _, ok := old[row.key]; ok {
				retained[row.key] = true
			}
		}
		for _, row := range before[c.field] {
			if retained[row.key] {
				continue
			}
			if _, err := tx.Exec(ctx, "DELETE FROM "+c.table+" WHERE business_line=$1 AND end_user=$2 AND "+c.key+"=$3", bl, eu, []byte(row.key)); err != nil {
				return fmt.Errorf("delete persona %s: %w", c.table, err)
			}
		}
		for _, row := range after[c.field] {
			previousKey := row.key
			if row.replaces {
				previousKey = row.previousKey
			}
			if previous, ok := old[previousKey]; ok && equalRow(previous, row) {
				continue
			}
			if err := writeRow(ctx, tx, bl, eu, c, row); err != nil {
				return fmt.Errorf("save persona %s: %w", c.table, err)
			}
		}
	}
	return nil
}

func writeRow(ctx context.Context, tx pgx.Tx, bl, eu []byte, c category, row bookRow) error {
	if c.kind == singleton {
		_, err := tx.Exec(ctx, `INSERT INTO book_meta(business_line,end_user,payload) VALUES ($1,$2,$3::jsonb)
			ON CONFLICT (business_line,end_user) DO UPDATE SET payload=EXCLUDED.payload`, bl, eu, encodePayload(row.payload))
		return err
	}
	columns := "business_line,end_user," + c.key + ",payload"
	values := "$1,$2,$3,$4::jsonb"
	updates := c.key + "=EXCLUDED." + c.key + ",payload=EXCLUDED.payload"
	args := []any{bl, eu, []byte(row.key), encodePayload(row.payload)}
	if c.kind == sequence || c.kind == tombstone {
		columns += ",ordinal"
		values += ",$5::text::numeric"
		updates += ",ordinal=EXCLUDED.ordinal"
		args = append(args, row.ordinal)
	}
	if c.field == "DeletionTasks" {
		columns += ",status,rebuild_pending"
		values += ",$6,$7"
		updates += ",status=EXCLUDED.status,rebuild_pending=EXCLUDED.rebuild_pending"
		args = append(args, row.status, row.rebuildPending)
	}
	if row.replaces && row.previousKey != row.key {
		// 来源取代会换变更集 ID，只更新原实体行，序位沿用装载时的值。
		query := "UPDATE " + c.table + " SET " + c.key + "=$3,payload=$4::jsonb,ordinal=$5::text::numeric WHERE business_line=$1 AND end_user=$2 AND " + c.key + "=$6"
		_, err := tx.Exec(ctx, query, bl, eu, []byte(row.key), encodePayload(row.payload), row.ordinal, []byte(row.previousKey))
		return err
	}
	query := "INSERT INTO " + c.table + " (" + columns + ") VALUES (" + values + ") ON CONFLICT (business_line,end_user," + c.key + ") DO UPDATE SET " + updates
	_, err := tx.Exec(ctx, query, args...)
	return err
}
