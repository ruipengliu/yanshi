package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/pg"
	"yanshi/internal/shard"
)

type shardConnection struct {
	id           int
	pool, direct *pgxpool.Pool
	notifier     *pg.Notifier
	mu           sync.Mutex
	checked      time.Time
	status       shardStatus
	verify       func(pg.DatabaseID) bool
}

func openShards(ctx context.Context, path string, notifyShards int, migrate bool, clk clock.Clock, logger *slog.Logger) (*storage, func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	config, locator, err := app.ParseShardConfig(data)
	if err != nil {
		return nil, nil, err
	}
	nctx, cancel := context.WithCancel(ctx)
	st := &storage{}
	var stopped []<-chan struct{}
	var unregisterPool func()
	closeAll := func() {
		cancel()
		if unregisterPool != nil {
			unregisterPool()
		}
		for _, done := range stopped {
			<-done
		}
		for _, part := range st.shardConnections {
			if part.direct != nil {
				part.direct.Close()
			}
			if part.pool != nil {
				part.pool.Close()
			}
		}
	}
	fail := func(err error) (*storage, func(), error) { closeAll(); return nil, nil, err }
	parts := map[int]app.Stores{}
	identities := map[string]bool{}
	var identityMu sync.Mutex
	owners := map[string]int{}
	expected := map[int]string{}
	byID := map[int]app.ShardDatabase{}
	for _, db := range config.Databases {
		byID[db.ID] = db
	}
	for _, id := range locator.Shards() {
		db := byID[id]
		part := &shardConnection{id: id}
		part.verify = func(identity pg.DatabaseID) bool {
			identityMu.Lock()
			defer identityMu.Unlock()
			if owner, ok := owners[identity.Instance]; ok && owner != id {
				return false
			}
			if old, ok := expected[id]; ok && old != identity.Instance {
				return false
			}
			owners[identity.Instance], expected[id] = id, identity.Instance
			return true
		}
		st.shardConnections = append(st.shardConnections, part)
		if part.pool, err = openShardPool(ctx, db.SQLDSN, false); err != nil {
			return fail(fmt.Errorf("shard %d SQL configuration invalid", id))
		}
		if part.direct, err = openShardPool(ctx, db.DirectDSN, true); err != nil {
			return fail(fmt.Errorf("shard %d direct configuration invalid", id))
		}
		probe, stop := context.WithTimeout(ctx, 2*time.Second)
		reachable := part.direct.Ping(probe) == nil && part.pool.Ping(probe) == nil
		stop()
		if reachable || migrate {
			if err = migrateOrCheck(ctx, part.direct, migrate); err != nil {
				return fail(fmt.Errorf("shard %d migrations unavailable", id))
			}
			if migrate {
				if err = pg.InitShardIdentity(ctx, part.direct, id); err != nil {
					return fail(fmt.Errorf("shard %d identity mismatch", id))
				}
			}
			sqlID, sqlSchema, identityErr := pg.DatabaseIdentity(ctx, part.pool)
			directID, directSchema, directErr := pg.DatabaseIdentity(ctx, part.direct)
			if identityErr != nil || directErr != nil || sqlID != directID || sqlSchema != directSchema || sqlID.Shard != id {
				return fail(fmt.Errorf("shard %d database identity mismatch", id))
			}
			if identities[sqlID.Instance] {
				return fail(fmt.Errorf("shard %d duplicates a database", id))
			}
			identities[sqlID.Instance] = true
			if !part.verify(sqlID) {
				return fail(fmt.Errorf("shard %d identity mismatch", id))
			}
			if err = migrateOrCheck(ctx, part.pool, false); err != nil {
				return fail(fmt.Errorf("shard %d SQL migrations unavailable", id))
			}
		}
		part.notifier = pg.NewNotifier(part.direct, logger).WithShards(notifyShards)
		done := make(chan struct{})
		stopped = append(stopped, done)
		go func() { defer close(done); part.notifier.Run(nctx) }()
		if reachable {
			ready, stop := context.WithTimeout(ctx, time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			for !part.notifier.Ready() && ready.Err() == nil {
				select {
				case <-ready.Done():
				case <-ticker.C:
				}
			}
			ticker.Stop()
			stop()
		}
		parts[id] = app.PostgresShard(part.pool, part.notifier, clk, locator, id)
	}
	st.Stores, err = app.Sharded(nctx, locator, parts, app.ShardOptions{Available: func(ctx context.Context, id int) error {
		for _, part := range st.shardConnections {
			if part.id == id {
				status := part.probe(ctx)
				if status.SQL && status.Migrations && status.Identity {
					return nil
				}
			}
		}
		return shard.ErrUnavailable
	}})
	if err != nil {
		return fail(err)
	}
	unregisterPool = metrics.RegisterPool(func() (int32, int32, int32, int64, int64, time.Duration) {
		var acquired, total, capacity int32
		var acquires, empty int64
		var duration time.Duration
		for _, part := range st.shardConnections {
			stat := part.pool.Stat()
			acquired += stat.AcquiredConns()
			total += stat.TotalConns()
			capacity += stat.MaxConns()
			acquires += stat.AcquireCount()
			empty += stat.EmptyAcquireCount()
			duration += stat.AcquireDuration()
		}
		return acquired, total, capacity, acquires, empty, duration
	})
	_, st.routingDigest = locator.Identity()
	return st, closeAll, nil
}

func migrateShards(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	config, locator, err := app.ParseShardConfig(data)
	if err != nil {
		return err
	}
	byID := map[int]app.ShardDatabase{}
	identities := map[string]bool{}
	for _, db := range config.Databases {
		byID[db.ID] = db
	}
	for _, id := range locator.Shards() {
		pool, err := pg.OpenDirect(ctx, byID[id].DirectDSN)
		if err != nil {
			return fmt.Errorf("shard %d direct connection failed", id)
		}
		err = pg.Migrate(ctx, pool)
		if err == nil {
			err = pg.InitShardIdentity(ctx, pool, id)
		}
		identity, _, identityErr := pg.DatabaseIdentity(ctx, pool)
		pool.Close()
		if err != nil || identityErr != nil {
			return fmt.Errorf("shard %d migration or identity failed", id)
		}
		if identities[identity.Instance] {
			return fmt.Errorf("shard %d duplicates a database", id)
		}
		identities[identity.Instance] = true
		fmt.Printf("shard %d migrated\n", id)
	}
	return nil
}

// 先构造可重连的池，断片不妨碍健康片冷启动；每条 SQL 与建连都有上界。
func openShardPool(ctx context.Context, dsn string, direct bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.ConnectTimeout = time.Second
	if !direct {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "2000"
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "1000"
	}
	if direct {
		cfg.MaxConns = 4
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

type shardStatus struct {
	Shard      int  `json:"shard"`
	SQL        bool `json:"sql"`
	Migrations bool `json:"migrations"`
	Version    int  `json:"migration_version"`
	Listen     bool `json:"listen"`
	Identity   bool `json:"identity"`
}

func (p *shardConnection) probe(ctx context.Context) shardStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.checked) < time.Second {
		status := p.status
		status.Listen = p.notifier != nil && p.notifier.Ready()
		return status
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	status := shardStatus{Shard: p.id, Listen: p.notifier != nil && p.notifier.Ready()}
	status.SQL = p.pool.Ping(ctx) == nil
	if status.SQL {
		pending, err := pg.Pending(ctx, p.pool)
		status.Migrations = err == nil && len(pending) == 0
		_ = p.pool.QueryRow(ctx, `SELECT coalesce(max(version),0) FROM schema_migrations`).Scan(&status.Version)
		sqlID, sqlSchema, err := pg.DatabaseIdentity(ctx, p.pool)
		directID, directSchema, directErr := pg.DatabaseIdentity(ctx, p.direct)
		status.Identity = err == nil && directErr == nil && sqlID.Shard == p.id && sqlID == directID && sqlSchema == directSchema
		if status.Identity && p.verify != nil {
			status.Identity = p.verify(sqlID)
		}
	}
	p.checked, p.status = time.Now(), status
	return status
}
