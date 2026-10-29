package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"yanshi/internal/pg"
	"yanshi/internal/shard"
)

// FanoutOptions 限制一次离线轮次；续扫只在成功后推进，失败不会跳过工作。
type FanoutOptions struct {
	Concurrency  int
	ShardTimeout time.Duration
	Budget       time.Duration
}

type scanPosition struct {
	Next    int
	Cursors map[int]string
}

type shardResult[T any] struct {
	ID    int
	Value T
	Err   error
}

// fanout 保留配置顺序合并结果。模拟不启动 goroutine，也不使用墙钟取消确定性步骤。
// cursor 只属于本路径、本分片；调用方只能在已消费候选后交回下一位置。
func fanout[T any](ctx context.Context, r *storeRouter, path string, fn func(context.Context, Stores, string) (T, string, error)) []shardResult[T] {
	r.scanMu.Lock()
	position := r.positions[path]
	if position == nil {
		position = &scanPosition{Cursors: map[int]string{}}
		r.positions[path] = position
	}
	start := position.Next
	if !r.deterministic {
		position.Next = (start + 1) % len(r.ids)
	}
	cursors := make(map[int]string, len(position.Cursors))
	for id, cursor := range position.Cursors {
		cursors[id] = cursor
	}
	r.scanMu.Unlock()
	if !r.deterministic {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.fanout.Budget)
		defer cancel()
	}
	results := make([]shardResult[T], len(r.ids))
	run := func(i int) {
		id := r.ids[(start+i)%len(r.ids)]
		results[i].ID = id
		attempt := ctx
		if !r.deterministic {
			var cancel context.CancelFunc
			attempt, cancel = context.WithTimeout(ctx, r.fanout.ShardTimeout)
			defer cancel()
		}
		if err := attempt.Err(); err != nil {
			results[i].Err = shardFailure(id, err)
			return
		}
		s, err := r.at(attempt, id)
		if err != nil {
			results[i].Err = shardFailure(id, err)
			return
		}
		value, cursor, err := fn(attempt, s, cursors[id])
		results[i].Value = value
		if err != nil {
			results[i].Err = shardFailure(id, err)
			return
		}
		r.scanMu.Lock()
		position.Cursors[id] = cursor
		r.scanMu.Unlock()
	}
	if r.deterministic {
		for i := range results {
			run(i)
		}
		return results
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(r.fanout.Concurrency, len(results)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				run(i)
			}
		}()
	}
	for i := range results {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func collect[T any](ctx context.Context, r *storeRouter, path string, complete bool, fn func(context.Context, Stores) (T, error)) ([]T, error) {
	parts := fanout(ctx, r, path, func(ctx context.Context, s Stores, cursor string) (T, string, error) {
		v, err := fn(ctx, s)
		return v, cursor, err
	})
	var values []T
	var errs []error
	for _, part := range parts {
		if part.Err != nil {
			errs = append(errs, part.Err)
		} else {
			values = append(values, part.Value)
		}
	}
	if len(errs) > 0 && (complete || r.deterministic || len(values) == 0) {
		return nil, errors.Join(errs...)
	}
	return values, independentError(errs)
}

// 连接故障不泄漏 DSN；业务错误必须保留错误链，清理回调依赖它决定是否重试。
func shardFailure(id int, err error) error {
	if onlyUnavailable(err) {
		return fmt.Errorf("shard %d: %w", id, unavailableFailure{err})
	}
	return fmt.Errorf("shard %d: %w", id, reportedFailure{cause: err, report: shardFailureText(err)})
}

type reportedFailure struct{ cause, report error }

func (e reportedFailure) Error() string { return e.report.Error() }
func (e reportedFailure) Unwrap() error { return e.cause }

// 混合错误的连接分支也要脱敏；原树保存在 Unwrap 中，不影响业务错误的识别。
func shardFailureText(err error) error {
	if onlyUnavailable(err) {
		return shard.ErrUnavailable
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return err
			}
			var branches []error
			for _, child := range children {
				branches = append(branches, shardFailureText(child))
			}
			return errors.Join(branches...)
		}
	}
	return err
}

// 对外错误保持固定文本，内部仍可识别连接与时限错误的原始原因。
type unavailableFailure struct{ cause error }

func (e unavailableFailure) Error() string   { return shard.ErrUnavailable.Error() }
func (e unavailableFailure) Unwrap() []error { return []error{shard.ErrUnavailable, e.cause} }

// PruneEndUsers 会合并不同候选与取消错误；其中任一业务错误都不能被超时掩盖。
func onlyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !onlyUnavailable(child) {
					return false
				}
			}
			return true
		}
	}
	return errors.Is(err, shard.ErrUnavailable) || pg.IsUnavailable(err)
}

// 健康片的结果可以继续处理，但部分成功不能掩盖程序、授权或清理回调错误。
func independentError(errs []error) error {
	var remaining []error
	for _, err := range errs {
		if !onlyUnavailable(err) {
			remaining = append(remaining, err)
		}
	}
	return errors.Join(remaining...)
}

func (r *storeRouter) each(ctx context.Context, path string, fn func(context.Context, Stores) error) error {
	_, err := collect(ctx, r, path, true, func(ctx context.Context, s Stores) (struct{}, error) {
		return struct{}{}, fn(ctx, s)
	})
	return err
}

func (o FanoutOptions) defaults() FanoutOptions {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.ShardTimeout <= 0 {
		o.ShardTimeout = time.Second
	}
	if o.Budget <= 0 {
		o.Budget = 4 * time.Second
	}
	return o
}

func (r *storeRouter) attempt(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.deterministic {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.fanout.ShardTimeout)
}
