// Package pgnode 是 node.Directory 与 node.Inbox 的 PostgreSQL 实现。
package pgnode

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/node"
	"yanshi/internal/pg"
)

type Directory struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewDirectory(pool *pgxpool.Pool, c clock.Clock) *Directory {
	return &Directory{pool: pool, clock: c}
}

var _ node.Directory = (*Directory)(nil)

func (d *Directory) Register(ctx context.Context, info node.Info) (string, uint64, error) {
	caps, err := proto.Marshal(&v1.CapabilitySet{Capabilities: info.Capabilities})
	if err != nil {
		return "", 0, err
	}
	base := node.SanitizeLabel(info.Label)
	// 并发注册可能在同一标签上撞唯一约束，重试即可。
	for range 8 {
		var label string
		var gen int64
		err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
			var bl, user string
			err := tx.QueryRow(ctx, `SELECT business_line, end_user FROM nodes WHERE node_id = $1 FOR UPDATE`, info.NodeID).Scan(&bl, &user)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			case bl != info.Scope.BusinessLine || user != info.Scope.EndUser:
				return fmt.Errorf("node %s belongs to another end user", info.NodeID)
			}
			rows, err := tx.Query(ctx, `SELECT label FROM nodes WHERE business_line = $1 AND end_user = $2 AND node_id <> $3`,
				info.Scope.BusinessLine, info.Scope.EndUser, info.NodeID)
			if err != nil {
				return err
			}
			taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			used := map[string]bool{}
			for _, l := range taken {
				used[l] = true
			}
			label = base
			for n := 2; used[label]; n++ {
				label = fmt.Sprintf("%s-%d", base, n)
			}
			// 归属在 UPSERT 中再判一次：上面的 FOR UPDATE 锁不住尚不存在的行，两个 EndUser 并发首次注册同一 ID 时
			// 都能通过前置检查，后到的一方在冲突后走 DO UPDATE；条件不满足时不更新、不返回行，注册失败。
			err = tx.QueryRow(ctx, `
				INSERT INTO nodes (node_id, business_line, end_user, label, kind, host_app, capabilities, online, last_seen, gen)
				VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8, 1)
				ON CONFLICT (node_id) DO UPDATE SET label = $4, kind = $5, host_app = $6, capabilities = $7,
					online = true, last_seen = $8, gen = nodes.gen + 1
				WHERE nodes.business_line = EXCLUDED.business_line AND nodes.end_user = EXCLUDED.end_user
				RETURNING gen`,
				info.NodeID, info.Scope.BusinessLine, info.Scope.EndUser, label, info.Kind, info.HostApp, caps, d.clock.Now(),
			).Scan(&gen)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("node %s belongs to another end user", info.NodeID)
			}
			return err
		})
		if pg.IsUniqueViolation(err) {
			continue
		}
		if err != nil {
			return "", 0, err
		}
		return label, uint64(gen), nil
	}
	return "", 0, fmt.Errorf("register node %s: label contention", info.NodeID)
}

func (d *Directory) SetOffline(ctx context.Context, nodeID string, gen uint64) error {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		WITH upd AS (UPDATE nodes SET online = false, last_seen = $3 WHERE node_id = $1 AND gen = $2 RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM nodes WHERE node_id = $1)`, nodeID, int64(gen), d.clock.Now()).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return node.ErrNotFound
	}
	return nil
}

const nodeCols = `node_id, business_line, end_user, label, kind, host_app, capabilities, online, last_seen`

func scanNode(r pgx.CollectableRow) (*node.Info, error) {
	n := &node.Info{}
	var caps []byte
	if err := r.Scan(&n.NodeID, &n.Scope.BusinessLine, &n.Scope.EndUser, &n.Label, &n.Kind, &n.HostApp, &caps, &n.Online, &n.LastSeen); err != nil {
		return nil, err
	}
	set := &v1.CapabilitySet{}
	if err := proto.Unmarshal(caps, set); err != nil {
		return nil, err
	}
	n.Capabilities = set.GetCapabilities()
	return n, nil
}

func (d *Directory) Get(ctx context.Context, nodeID string) (*node.Info, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE node_id = $1`, nodeID)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, scanNode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, node.ErrNotFound
	}
	return n, err
}

func (d *Directory) List(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE business_line = $1 AND end_user = $2 ORDER BY label`,
		scope.BusinessLine, scope.EndUser)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanNode)
}

type Inbox struct {
	pool     *pgxpool.Pool
	notifier *pg.Notifier
}

func NewInbox(pool *pgxpool.Pool, n *pg.Notifier) *Inbox { return &Inbox{pool: pool, notifier: n} }

var _ node.Inbox = (*Inbox)(nil)

// bump 在同一事务内递增版本并发出通知。
func (in *Inbox) bump(ctx context.Context, tx pgx.Tx, nodeID string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO inbox_versions (node_id, version) VALUES ($1, 1)
		ON CONFLICT (node_id) DO UPDATE SET version = inbox_versions.version + 1`, nodeID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, in.notifier.Channel(pg.TopicInbox, nodeID), nodeID)
	return err
}

func (in *Inbox) Put(ctx context.Context, nodeID string, inv *v1.Invoke) error {
	b, err := proto.Marshal(inv)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, in.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO inbox (node_id, call_id, data, ord, session_id) VALUES ($1, $2, $3, nextval('inbox_seq'), $4)
			ON CONFLICT DO NOTHING`, nodeID, inv.GetCallId(), b, inv.GetSessionId())
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return in.bump(ctx, tx, nodeID)
	})
}

func (in *Inbox) Remove(ctx context.Context, nodeID, callID string) error {
	return pgx.BeginFunc(ctx, in.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM inbox WHERE node_id = $1 AND call_id = $2`, nodeID, callID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return in.bump(ctx, tx, nodeID)
	})
}

func (in *Inbox) version(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, nodeID string) (uint64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT version FROM inbox_versions WHERE node_id = $1), 0)`, nodeID).Scan(&v)
	return uint64(v), err
}

func (in *Inbox) Pending(ctx context.Context, nodeID string) ([]*v1.Invoke, uint64, error) {
	var items []*v1.Invoke
	var version uint64
	// 可重复读事务保证条目与版本号来自同一快照。
	err := pgx.BeginTxFunc(ctx, in.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		v, err := in.version(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		version = v
		rows, err := tx.Query(ctx, `SELECT data FROM inbox WHERE node_id = $1 ORDER BY ord`, nodeID)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Invoke, error) {
			var b []byte
			if err := r.Scan(&b); err != nil {
				return nil, err
			}
			inv := &v1.Invoke{}
			return inv, proto.Unmarshal(b, inv)
		})
		return err
	})
	return items, version, err
}

func (in *Inbox) Wait(ctx context.Context, nodeID string, version uint64) error {
	return in.notifier.WaitFor(ctx, pg.TopicInbox, nodeID, func(ctx context.Context) (bool, error) {
		v, err := in.version(ctx, in.pool, nodeID)
		return v != version, err
	})
}

func (in *Inbox) RemoveSession(ctx context.Context, sessionID string) error {
	return pgx.BeginFunc(ctx, in.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM inbox WHERE session_id = $1 RETURNING node_id`, sessionID)
		if err != nil {
			return err
		}
		nodes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		slices.Sort(nodes)
		for _, n := range slices.Compact(nodes) {
			if err := in.bump(ctx, tx, n); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Directory) DeleteScope(ctx context.Context, scope node.Scope) ([]string, error) {
	rows, err := d.pool.Query(ctx, `DELETE FROM nodes WHERE business_line = $1 AND end_user = $2 RETURNING node_id`,
		scope.BusinessLine, scope.EndUser)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	slices.Sort(ids)
	return ids, err
}
