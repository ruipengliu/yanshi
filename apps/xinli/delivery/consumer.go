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
			next := recovery.Progress{BusinessLine: line, Round: p.Round, Phase: recovery.Heads, Version: p.Version + 1}
			return c.Progress.Commit(ctx, p.Version, next, recovery.Batch{})
		}
		return err
	}
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
				if current.Phase != recovery.Follow || current.Round != p.Round {
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
					current = recovery.Progress{BusinessLine: line, Round: current.Round, Phase: recovery.Heads}
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
		result = errors.Join(result, err)
		terminal = terminal || err == nil && (e.Kind == "run_completed" || e.Kind == "run_failed")
	}
	if terminal && c.Terminal != nil {
		result = errors.Join(result, c.Terminal(ctx))
	}
	return result
}

// Start 不把处理/发送放在长轮询后；空闲分片不会拖住已接收任务。
// 错误只交给调用方的无正文计数回调，具体凭据与供应商错误不写日志。
func Start(ctx context.Context, lines []string, consumer *Consumer, worker *Worker, failed func()) func() {
	life, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	loop := func(fn func(context.Context) error, delay time.Duration) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt := 0
			for life.Err() == nil {
				pause := delay
				if err := fn(life); err != nil {
					attempt++
					pause = task.Backoff("delivery", attempt, time.Second, time.Minute)
					if failed != nil && life.Err() == nil {
						failed()
					}
				} else {
					attempt = 0
				}
				timer := time.NewTimer(pause)
				select {
				case <-life.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	for _, line := range lines {
		loop(func(ctx context.Context) error { return consumer.Poll(ctx, line) }, 10*time.Millisecond)
		loop(func(ctx context.Context) error { return consumer.Process(ctx, line) }, time.Second)
		// 稳定枚举每轮只取一页，持久任务重启后可再次发现；不依赖进程内定时回调去重。
		after := ""
		loop(func(ctx context.Context) error {
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

// B5 的全种类游标升级到 B6 选定集合时，显式重新对账，不复用旧过滤位置。
func filterChanged(err error) bool {
	var e *kernel.HTTPError
	return errors.As(err, &e) && e.StatusCode == 409 && e.Reason == "outbox_filter_changed"
}
