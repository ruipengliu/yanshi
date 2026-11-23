package delivery

import (
	"context"
	"errors"
	"sync"
	"time"

	"xinli/account"
	"xinli/kernel"
	"xinli/recovery"
	"xinli/task"
)

// Source 让首次 H、恢复追赶与普通消费绑定同一过滤集合。
type Source struct{ *kernel.Client }

var kinds = []string{"approval_requested", "question", "run_completed", "run_failed", "session_deleted"}

func (s Source) OutboxHeads(ctx context.Context, line string) ([]kernel.OutboxShard, error) {
	return s.OutboxHeadsKinds(ctx, line, kinds)
}
func (s Source) Outbox(ctx context.Context, line string, shard int, after string, limit int) (kernel.OutboxPage, error) {
	return s.OutboxKinds(ctx, line, shard, after, limit, kinds)
}

type Consumer struct {
	Progress recovery.Consumer
	Store    Store
	Kernel   recovery.Kernel
	Accounts account.Enumerator
	Now      func() time.Time
	// Terminal 加速 A8 的既有安全对账；丢失本地唤醒仍有持久 retry_at 与周期扫描。
	Terminal func(context.Context) error
}

func (c *Consumer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Consumer) Poll(ctx context.Context, line string) error {
	p, err := c.Progress.Load(ctx, line)
	if err != nil {
		return err
	}
	if p.Phase != recovery.Follow {
		r := recovery.Recoverer{Accounts: c.Accounts, Kernel: c.Kernel, Consumer: c.Progress}
		_, err := r.Step(ctx, line)
		if filterChanged(err) {
			next := recovery.Progress{BusinessLine: line, Round: p.Round, Phase: recovery.Heads, Version: p.Version + 1, Revision: p.Revision}
			if recovery.Shard(ctx) >= 0 {
				next.Revision++
			}
			return c.Progress.Commit(ctx, p.Version, next, recovery.Batch{})
		}
		if err != nil {
			return retryDetail{cause: err, phase: p.Phase, session: p.Session}
		}
		return err
	}
	return c.pollPages(ctx, line, p)
}

func (c *Consumer) pollPages(ctx context.Context, line string, p recovery.Progress) error {
	// 每个分片独立请求、独立提交；坏分片不能使健康分片已收到的页等待整轮成功。
	var wg sync.WaitGroup
	errs := make(chan error, len(p.Positions))
	for _, pos := range p.Positions {
		wg.Add(1)
		go func(pos recovery.Position) {
			defer wg.Done()
			call, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			page, err := c.Kernel.Outbox(call, line, pos.Shard, pos.After, 100)
			reset := kernel.OutboxResetRequired(err) || filterChanged(err)
			if err != nil && !reset {
				errs <- err
				return
			}
			if !reset && page.Next == "" {
				errs <- errors.New("delivery: missing cursor")
				return
			}
			for {
				current, err := c.Progress.Load(call, line)
				if err != nil {
					errs <- err
					return
				}
				if current.Phase != p.Phase || current.Round != p.Round {
					return
				}
				index := -1
				for i, v := range current.Positions {
					if v.Shard == pos.Shard {
						index = i
					}
				}
				if index < 0 || current.Positions[index].After != pos.After {
					return
				}
				version := current.Version
				batch := recovery.Batch{Envelopes: page.Events}
				if reset {
					revision := current.Revision
					if recovery.Shard(call) >= 0 {
						revision++
					}
					current = recovery.Progress{BusinessLine: line, Round: current.Round, Phase: recovery.Heads, Revision: revision}
					batch = recovery.Batch{}
				} else {
					current.Positions[index].After = page.Next
				}
				current.Version = version + 1
				err = c.Progress.Commit(call, version, current, batch)
				if errors.Is(err, recovery.ErrConflict) {
					continue
				}
				if err != nil {
					errs <- err
				}
				return
			}
		}(pos)
	}
	wg.Wait()
	close(errs)
	var result error
	for err := range errs {
		result = errors.Join(result, err)
	}
	return result
}

func (c *Consumer) Process(ctx context.Context, line string) error {
	pending, err := c.Store.Pending(ctx, line, 100)
	if err != nil {
		return err
	}
	var result error
	terminal := false
	for _, e := range pending {
		err := c.Store.Process(ctx, e, c.now())
		if err != nil {
			err = retryDetail{cause: err, session: e.SessionID, run: e.RunID, call: e.CallID, envelope: e.EnvelopeID}
		}
		result = errors.Join(result, err)
		terminal = terminal || err == nil && (e.Kind == "run_completed" || e.Kind == "run_failed")
	}
	if terminal && c.Terminal != nil {
		result = errors.Join(result, c.Terminal(ctx))
	}
	return result
}

// Start 不把处理/发送放在长轮询后；空闲分片不会拖住已接收任务。
// 错误只交给调用方的固定类别与 ID 回调，具体凭据与供应商错误不写日志。
func Start(ctx context.Context, lines []string, consumer *Consumer, worker *Worker, failed func(Failure)) func() {
	life, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// parent 是循环的寿命：分片循环在该片被移出槽位表时单独停止，其余随 Start 的返回函数一起停止。
	loop := func(parent context.Context, stage, line string, shard int, fn func(context.Context) error, delay time.Duration) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt := 0
			for parent.Err() == nil {
				pause := delay
				if err := fn(parent); err != nil {
					if errors.Is(err, recovery.ErrConflict) {
						attempt = 0
					} else {
						attempt++
						pause = task.Backoff("delivery", attempt, time.Second, time.Minute)
					}
					if failed != nil && parent.Err() == nil {
						for _, info := range failureInfos(stage, line, shard, err) {
							failed(info)
						}
					}
				} else {
					attempt = 0
				}
				timer := time.NewTimer(pause)
				select {
				case <-parent.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	for _, line := range lines {
		// 发现循环登记与注销分片；每个分片有自己的长轮询与退避，不等待整轮。键 -1 是单库的进度。
		started := map[int]context.CancelFunc{}
		// retiring 是已停止、进度尚未重置成功的分片；冲突或存储错误时下一轮发现再试。
		retiring := map[int]bool{}
		loop(life, "discovery", line, -1, func(ctx context.Context) error {
			heads, err := consumer.Kernel.OutboxHeads(ctx, line)
			if err != nil {
				return err
			}
			want := map[int]string{}
			if len(heads) == 1 && heads[0].Shard == 0 {
				want[-1] = heads[0].Identity
			} else {
				for _, head := range heads {
					want[head.Shard] = head.Identity
				}
			}
			// 某片核对失败不妨碍登记其余分片，错误汇总后交给退避。
			var result error
			// 失联的分片仍会列出，未列出说明它已移出槽位表（合并或下线）：停掉它的循环，否则对 404 无限重试；
			// 进度重置回 Heads，同一编号以后再加入时从新的 H 对账，不沿用旧游标。迁出的 Session 由接收槽位的
			// 分片覆盖：它换代或槽位表摘要变化后重新对账（ADR-0046“迁槽与游标”）。
			for id, stop := range started {
				if _, ok := want[id]; ok {
					continue
				}
				stop()
				delete(started, id)
				retiring[id] = true
			}
			for id := range retiring {
				if _, ok := want[id]; ok {
					delete(retiring, id)
					continue
				}
				err := consumer.Retire(scope(ctx, id), line)
				if err == nil {
					delete(retiring, id)
				}
				result = errors.Join(result, err)
			}
			for id, identity := range want {
				result = errors.Join(result, consumer.ReconcileIdentity(scope(ctx, id), line, identity))
				if started[id] != nil {
					continue
				}
				shard, stop := context.WithCancel(life)
				started[id] = stop
				if id < 0 {
					loop(shard, "consume", line, 0, func(ctx context.Context) error { return consumer.Poll(ctx, line) }, 10*time.Millisecond)
					continue
				}
				// 恢复枚举可能等待另一个 EndUser 分片；已取得 H 的本片仍独立接收出站。
				loop(shard, "recovery", line, id, func(ctx context.Context) error {
					ctx = recovery.WithShard(ctx, id)
					p, err := consumer.Progress.Load(ctx, line)
					if err != nil || p.Phase == recovery.Follow {
						return err
					}
					return consumer.Poll(ctx, line)
				}, 100*time.Millisecond)
				loop(shard, "outbox", line, id, func(ctx context.Context) error {
					ctx = recovery.WithShard(ctx, id)
					p, err := consumer.Progress.Load(ctx, line)
					if err != nil || len(p.Positions) == 0 {
						return err
					}
					return consumer.pollPages(ctx, line, p)
				}, 10*time.Millisecond)
			}
			return result
		}, time.Second)
		loop(life, "process", line, -1, func(ctx context.Context) error { return consumer.Process(ctx, line) }, time.Second)
		// 稳定枚举每轮只取一页，持久任务重启后可再次发现；不依赖进程内定时回调去重。
		after := ""
		loop(life, "owner_scan", line, -1, func(ctx context.Context) error {
			owners, err := consumer.Accounts.AccountOwners(ctx, line, after, 20)
			if err != nil {
				return err
			}
			var result error
			for _, owner := range owners {
				call, cancel := context.WithTimeout(ctx, 30*time.Second)
				result = errors.Join(result, worker.ScanOwner(call, owner))
				cancel()
				after = owner.EndUser
			}
			if len(owners) < 20 {
				after = ""
			}
			return result
		}, time.Second)
	}
	return func() { cancel(); wg.Wait() }
}

// ReconcileIdentity 在槽位表摘要与取 H 时不同时把进度重置回 Heads，开始新一轮对账；还在 Heads 或尚无进度时不动。
// 滚动部署期间新旧进程交替回答，可能连续重置几次，部署完成后收敛；重置只重做对账，不丢已收的信封。
// 冲突说明进度刚被别的步骤推进，留给下一轮发现再查。
func (c *Consumer) ReconcileIdentity(ctx context.Context, line, identity string) error {
	p, err := c.Progress.Load(ctx, line)
	if err != nil {
		return err
	}
	if p.Identity == identity {
		return nil
	}
	if err := c.restart(ctx, line, p); err != nil && !errors.Is(err, recovery.ErrConflict) {
		return err
	}
	return nil
}

// Retire 把已移出槽位表的分片的进度重置回 Heads，丢掉旧游标与本轮位置；Round 保留，Checkpoint 按 Session 保存、不受影响。
// 冲突原样返回：调用方须在下一轮重试，不能把未重置的记录当作已退役。
func (c *Consumer) Retire(ctx context.Context, line string) error {
	p, err := c.Progress.Load(ctx, line)
	if err != nil {
		return err
	}
	return c.restart(ctx, line, p)
}

func (c *Consumer) restart(ctx context.Context, line string, p recovery.Progress) error {
	if p.Phase == "" || p.Phase == recovery.Heads {
		return nil
	}
	next := recovery.Progress{BusinessLine: line, Round: p.Round, Phase: recovery.Heads, Version: p.Version + 1, Revision: p.Revision}
	if recovery.Shard(ctx) >= 0 {
		next.Revision++
	}
	return c.Progress.Commit(ctx, p.Version, next, recovery.Batch{})
}

// scope 选择发现循环中键 id 的进度：-1 是单库，其余是分片编号。
func scope(ctx context.Context, id int) context.Context {
	if id < 0 {
		return ctx
	}
	return recovery.WithShard(ctx, id)
}

// B5 的全种类游标升级到 B6 选定集合时，显式重新对账，不复用旧过滤位置。
func filterChanged(err error) bool {
	var e *kernel.HTTPError
	return errors.As(err, &e) && e.StatusCode == 409 && e.Reason == "outbox_filter_changed"
}
