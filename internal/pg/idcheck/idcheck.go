// Package idcheck 只读盘点历史快照中的 ID 命名空间，不修改、重命名或自动采信碰撞项。
package idcheck

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/shard"
)

// Finding 只含 ID、位置与固定原因，绝不输出消息、参数或二进制正文。
type Finding struct {
	Table  string `json:"table"`
	Field  string `json:"field"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type column struct {
	table    string
	name     string
	optional bool
	session  bool
}

// columns 显式列出内核持久 ID；真实 EndUser、业务线、UUID 控制水位不属于版本化 ID。
// 不按列名猜测全部 text 列，避免把个人内容带入报告。
var columns = []column{
	{"events", "session_id", false, true}, {"session_snapshots", "session_id", false, true},
	{"work_items", "session_id", false, true}, {"work_dues", "session_id", false, true},
	{"janitor_items", "session_id", false, true}, {"sessions", "session_id", false, true},
	{"presence_versions", "session_id", false, true}, {"sandbox_items", "session_id", false, true},
	{"sandbox_activity", "sandbox_id", false, true}, {"sandbox_ledger", "session_id", false, true},
	{"sandbox_ledger", "call_id", false, false}, {"artifacts", "id", false, false},
	{"artifacts", "session_id", false, true}, {"presence", "session_id", false, true},
	{"presence", "conn_id", false, false}, {"presence", "device_id", true, false},
	{"nodes", "node_id", false, false}, {"inbox_versions", "node_id", false, false},
	{"inbox", "node_id", false, false}, {"inbox", "call_id", false, false}, {"inbox", "session_id", false, true},
	{"session_tombstones", "session_id", false, true}, {"session_tombstones", "request_id", true, false},
	{"deletion_requests", "request_id", false, false}, {"end_user_tombstones", "request_id", false, false},
	{"memories", "id", false, false}, {"memories", "source_session", true, true},
	{"memories", "source_call", true, false}, {"memories", "superseded_by", true, false},
	{"memory_grants", "id", false, false}, {"memory_access", "memory_id", false, false},
	{"memory_access", "session_id", true, true}, {"memory_access", "run_id", true, false},
	{"usage_entries", "id", false, false}, {"usage_entries", "idempotency_key", false, false},
	{"push_devices", "device_id", false, false}, {"outbox", "envelope_id", false, false},
	{"outbox", "session_id", false, true}, {"outbox", "run_id", true, false},
	{"outbox", "call_id", true, false}, {"outbox", "deletion_request_id", true, false},
}

// ScanHistorical 的调用方须确认整个源库是历史范围；字符串无法证明对象的创建年代。
// 在停写快照上逐行输出，内存不随数据量增长；任何读取或解码错误都不能算预检通过。
func ScanHistorical(ctx context.Context, pool *pgxpool.Pool, emit func(Finding) error) (int, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	count := 0
	report := func(f Finding) error { count++; return emit(f) }
	for _, c := range columns {
		name := pgx.Identifier{c.name}.Sanitize()
		q := "SELECT " + name + " FROM " + pgx.Identifier{c.table}.Sanitize() + " WHERE " + name + " IS NOT NULL"
		if c.optional {
			q += " AND " + name + " <> ''"
		}
		if err := scanStrings(ctx, tx, q, func(id string) error { return inspect(c.table, c.name, id, c.session, report) }); err != nil {
			return count, err
		}
	}
	if err := scanStrings(ctx, tx, "SELECT unnest(session_ids) FROM end_user_tombstones", func(id string) error {
		return inspect("end_user_tombstones", "session_ids", id, true, report)
	}); err != nil {
		return count, err
	}
	payloads := []struct {
		table, key, field string
		newMessage        func() proto.Message
	}{
		{"events", "session_id", "data", func() proto.Message { return &v1.Event{} }},
		{"session_snapshots", "session_id", "data", func() proto.Message { return &v1.SessionSnapshot{} }},
		{"inbox", "call_id", "data", func() proto.Message { return &v1.Invoke{} }},
		{"sandbox_ledger", "call_id", "result", func() proto.Message { return &v1.InvokeResult{} }},
	}
	for _, p := range payloads {
		rows, err := tx.Query(ctx, "SELECT "+p.key+", "+p.field+" FROM "+p.table+" WHERE "+p.field+" IS NOT NULL")
		if err != nil {
			return count, err
		}
		for rows.Next() {
			var id string
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return count, err
			}
			msg := p.newMessage()
			if err := proto.Unmarshal(data, msg); err != nil {
				if err := report(Finding{Table: p.table, Field: p.field, ID: id, Reason: "invalid_protobuf"}); err != nil {
					rows.Close()
					return count, err
				}
				continue
			}
			if err := inspectMessage(p.table, p.field, id, msg.ProtoReflect(), report); err != nil {
				rows.Close()
				return count, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return count, err
		}
	}
	return count, tx.Commit(ctx)
}

func scanStrings(ctx context.Context, tx pgx.Tx, query string, visit func(string) error) error {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := visit(id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func inspect(table, field, id string, session bool, emit func(Finding) error) error {
	parsed := id
	for _, prefix := range []string{"artifact://", "sbx_", "@agent/"} {
		if strings.HasPrefix(parsed, prefix) {
			parsed = strings.TrimPrefix(parsed, prefix)
			if prefix != "artifact://" {
				session = true
			}
			break
		}
	}
	if shard.VersionedNamespace(parsed) {
		if err := emit(Finding{Table: table, Field: field, ID: id, Reason: "historical_namespace_collision"}); err != nil {
			return err
		}
	}
	var err error
	if session {
		_, _, err = shard.SessionSlotOf(parsed)
	} else {
		_, _, err = shard.SlotOf(parsed)
	}
	if err != nil {
		return emit(Finding{Table: table, Field: field, ID: id, Reason: "invalid_id"})
	}
	return nil
}

func inspectMessage(table, path, rowID string, msg protoreflect.Message, emit func(Finding) error) error {
	// 已退役或来自未知契约的字段也可能带历史 ID，不能把未检查的字节当成预检通过。
	if len(msg.GetUnknown()) != 0 {
		if err := emit(Finding{Table: table, Field: path, ID: rowID, Reason: "unparsed_protobuf_fields"}); err != nil {
			return err
		}
	}
	var result error
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		field := path + "." + name
		visit := func(v protoreflect.Value) error {
			if fd.Kind() == protoreflect.MessageKind {
				return inspectMessage(table, field, rowID, v.Message(), emit)
			}
			if fd.Kind() != protoreflect.StringKind {
				return nil
			}
			s := v.String()
			if s == "" {
				return nil
			}
			if (name == "uri" || name == "artifact_uri") && strings.HasPrefix(s, "artifact://") {
				return inspect(table, field, s, false, emit)
			}
			if name == "subject" && msg.Descriptor().FullName() == "yanshi.v1.DecisionMade" {
				kind := msg.Descriptor().Fields().ByName("kind")
				if msg.Get(kind).String() == "tool_result_injection" {
					return inspect(table, field, s, false, emit)
				}
			}
			if idField(name) {
				return inspect(table, field, s, name == "session_id", emit)
			}
			return nil
		}
		if fd.IsMap() {
			// decisions 的键是判断种类 / 对象 ID，值是判断结果；其余 map 不读取任意文本。
			if name == "decisions" {
				v.Map().Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
					kind, subject, ok := strings.Cut(k.String(), "/")
					if ok && kind == "tool_result_injection" {
						result = inspect(table, field+".key", subject, false, emit)
					}
					return result == nil
				})
			}
		} else if fd.IsList() {
			list := v.List()
			for i := 0; i < list.Len() && result == nil; i++ {
				result = visit(list.Get(i))
			}
		} else {
			result = visit(v)
		}
		return result == nil
	})
	return result
}

func idField(name string) bool {
	if name == "id" || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") {
		return true
	}
	switch name {
	case "from_call", "from_background", "delegated_run", "answered", "superseded_runs", "superseded_card_calls":
		return true
	}
	return false
}

// RequireClean 让命令在输出完整报告后非零退出，迁移脚本不能漏掉碰撞项。
func RequireClean(count int) error {
	if count != 0 {
		return fmt.Errorf("id preflight: %d finding(s), review the historical snapshot report", count)
	}
	return nil
}
