package orchestrator

import (
	"context"
	"errors"
	"time"

	"persona/domain"
)

// StartExpirations 与删除待办分开启动；生产装配由 A11 接入。
// 注入的 tick 让测试可确定地推进扫描，默认周期与删除恢复一致。
func (o *Orchestrator) StartExpirations(tick <-chan time.Time) {
	o.mu.Lock()
	if o.closed || o.startedExpirations {
		o.mu.Unlock()
		return
	}
	o.startedExpirations = true
	o.pending.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.pending.Done()
		if tick == nil {
			ticker := time.NewTicker(o.retryInterval)
			defer ticker.Stop()
			tick = ticker.C
		}
		for {
			if err := o.ScanExpirations(o.ctx); err != nil {
				o.failed("expiration_scan", "", "", "", err)
			}
			select {
			case <-o.stopRetries:
				return
			case <-o.ctx.Done():
				return
			case _, ok := <-tick:
				if !ok {
					return
				}
			}
		}
	}()
}

// ScanExpirations 只发现 owner，逐键事务重查业务日，不与内核发生网络调用。
func (o *Orchestrator) ScanExpirations(ctx context.Context) error {
	if !o.begin() {
		return errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	owners, err := o.store.ExpiringCaptures(ctx, o.now())
	if err != nil {
		return err
	}
	var result error
	for _, owner := range owners {
		err := o.store.Update(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
			now := o.now()
			for _, date := range b.ExpiringCaptureDays(now) {
				if err := b.ExpireCaptures(date, now); err != nil {
					return err
				}
			}
			return nil
		})
		result = errors.Join(result, err)
	}
	return result
}
