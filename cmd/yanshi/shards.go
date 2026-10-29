package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/pg"
)

type shardConnection struct {
	id           int
	pool, direct *pgxpool.Pool
	notifier     *pg.Notifier
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
	closeAll := func() {
		cancel()
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
	identities := map[pg.DatabaseID]bool{}
	byID := map[int]app.ShardDatabase{}
	for _, db := range config.Databases {
		byID[db.ID] = db
	}
	for _, id := range locator.Shards() {
		db := byID[id]
		part := &shardConnection{id: id}
		st.shardConnections = append(st.shardConnections, part)
		if part.pool, err = pg.Open(ctx, db.SQLDSN, ""); err != nil {
			return fail(fmt.Errorf("shard %d SQL connection failed", id))
		}
		if part.direct, err = pg.OpenDirect(ctx, db.DirectDSN); err != nil {
			return fail(fmt.Errorf("shard %d direct connection failed", id))
		}
		sqlID, sqlSchema, err := pg.DatabaseIdentity(ctx, part.pool)
		if err != nil {
			return fail(fmt.Errorf("shard %d SQL identity: %w", id, err))
		}
		directID, directSchema, err := pg.DatabaseIdentity(ctx, part.direct)
		if err != nil {
			return fail(fmt.Errorf("shard %d direct identity: %w", id, err))
		}
		if sqlID != directID || sqlSchema != directSchema {
			return fail(fmt.Errorf("shard %d SQL and direct database identity differ", id))
		}
		if identities[sqlID] {
			return fail(fmt.Errorf("shard %d duplicates a physical database", id))
		}
		identities[sqlID] = true
		if err = migrateOrCheck(ctx, part.direct, migrate); err != nil {
			return fail(fmt.Errorf("shard %d: %w", id, err))
		}
		// SQL 入口也须看见同一 schema，不能只验证直连地址上的迁移。
		if err = migrateOrCheck(ctx, part.pool, false); err != nil {
			return fail(fmt.Errorf("shard %d SQL: %w", id, err))
		}
		part.notifier = pg.NewNotifier(part.direct, logger).WithShards(notifyShards)
		done := make(chan struct{})
		stopped = append(stopped, done)
		go func() { defer close(done); part.notifier.Run(nctx) }()
		ready, stop := context.WithTimeout(ctx, 10*time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
		for !part.notifier.Ready() && ready.Err() == nil {
			select {
			case <-ready.Done():
			case <-ticker.C:
			}
		}
		ticker.Stop()
		connected := part.notifier.Ready()
		stop()
		if !connected {
			return fail(fmt.Errorf("shard %d notifier not ready", id))
		}
		parts[id] = app.PostgresShard(part.pool, part.notifier, clk, locator, id)
	}
	st.Stores, err = app.Sharded(nctx, locator, parts, app.ShardOptions{})
	if err != nil {
		return fail(err)
	}
	metrics.RegisterPool(func() (int32, int32, int32, int64, int64, time.Duration) {
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
	identities := map[pg.DatabaseID]bool{}
	for _, db := range config.Databases {
		byID[db.ID] = db
	}
	for _, id := range locator.Shards() {
		pool, err := pg.OpenDirect(ctx, byID[id].DirectDSN)
		if err != nil {
			return fmt.Errorf("shard %d direct connection failed", id)
		}
		identity, _, identityErr := pg.DatabaseIdentity(ctx, pool)
		if identityErr != nil {
			pool.Close()
			return identityErr
		}
		if identities[identity] {
			pool.Close()
			return fmt.Errorf("shard %d duplicates a physical database", id)
		}
		identities[identity] = true
		err = pg.Migrate(ctx, pool)
		pool.Close()
		if err != nil {
			return fmt.Errorf("shard %d migration: %w", id, err)
		}
		fmt.Printf("shard %d migrated\n", id)
	}
	return nil
}
