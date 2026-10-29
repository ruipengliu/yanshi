package sim

import (
	"context"
	"fmt"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/outbox"
)

// consumer 的游标和接收表模拟一次业务库事务，effects 模拟按 envelope_id 去重的处理。
// 故障节拍独立于主随机数，增加消费检查不改变 Worker 的故障轨迹。
type outboxConsumer struct {
	cursor      string
	received    map[string]outbox.Envelope
	effects     map[string]int
	publishedAt map[string]time.Time
	steps       int
}

func (w *World) consumeOutbox(ctx context.Context, faults bool) (int, error) {
	c := &w.outboxConsumer
	if c.received == nil {
		c.received = map[string]outbox.Envelope{}
		c.effects = map[string]int{}
		c.publishedAt = map[string]time.Time{}
	}
	c.steps++
	req := outbox.Request{BusinessLine: "bl", After: c.cursor, Limit: 3}
	page, err := w.stores.Outbox.Read(ctx, req)
	if err != nil {
		return 0, err
	}
	w.Stats.OutboxReads++
	if faults && c.steps%5 == 0 {
		// 事务提交前崩溃，整页和候选游标一同丢弃。
		w.Stats.OutboxConsumerCrashes++
		return len(page.Events), nil
	}
	if err := w.receiveOutbox(page); err != nil {
		return 0, err
	}
	if faults && c.steps%5 == 1 && len(page.Events) > 0 {
		// 提交成功但响应丢失：重启后重读旧游标，接收与处理都按稳定 ID 去重。
		w.Stats.OutboxConsumerCrashes++
		replay, err := w.stores.Outbox.Read(ctx, req)
		if err != nil {
			return 0, err
		}
		w.Stats.OutboxReads++
		if err := w.receiveOutbox(replay); err != nil {
			return 0, err
		}
	}
	return len(page.Events), nil
}

func (w *World) receiveOutbox(page outbox.Page) error {
	c := &w.outboxConsumer
	for _, e := range page.Events {
		if e.BusinessLine != "bl" {
			return fmt.Errorf("invariant: foreign envelope %s", e.EnvelopeID)
		}
		if previous, exists := c.received[e.EnvelopeID]; exists {
			if previous != e {
				return fmt.Errorf("invariant: replay changed envelope %s", e.EnvelopeID)
			}
			w.Stats.OutboxRedeliveries++
			continue
		}
		c.received[e.EnvelopeID] = e
		c.publishedAt[e.EnvelopeID] = w.clock.Now()
		c.effects[e.EnvelopeID]++
		w.Stats.OutboxConsumed++
	}
	c.cursor = page.Next
	return nil
}

func (w *World) checkOutboxConsumer(rows []outbox.Envelope) error {
	for id, count := range w.outboxConsumer.effects {
		if count != 1 {
			return fmt.Errorf("invariant: envelope %s produced %d effects", id, count)
		}
	}
	for _, e := range rows {
		if w.outboxConsumer.received[e.EnvelopeID] != e || w.outboxConsumer.effects[e.EnvelopeID] != 1 {
			return fmt.Errorf("invariant: retained envelope %s not consumed exactly once", e.EnvelopeID)
		}
	}
	return nil
}

// drainOutbox 在静止写入的收敛点核对全部保留行。PG 的 xmin 可能短暂受其他测试 schema 阻塞，
// 等待使用真实时间且不拨动模拟时钟，避免跨数据库调度噪声改变定时与保留语义。
func (w *World) drainOutbox(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	rows, err := w.stores.Outbox.List(ctx, "bl")
	if err != nil {
		return err
	}
	for {
		n, err := w.consumeOutbox(ctx, false)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := w.checkOutboxConsumer(rows); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("outbox consumer did not converge: %w", w.checkOutboxConsumer(rows))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// simOutbox 在 Stores seam 记录合法淘汰，旧的日志对应不变量不能把满 72h 的清理当成漏写。
// 清理前排空消费，B4 只验证活跃消费者；落后至过期后的应用对账属于 B5。
type simOutbox struct {
	outbox.Store
	w *World
}

func (s simOutbox) Maintain(ctx context.Context, clk clock.Clock, batch int) error {
	if err := s.w.drainOutbox(ctx); err != nil {
		return err
	}
	before, err := s.Store.List(ctx, "bl")
	if err != nil {
		return err
	}
	if err := s.Store.Maintain(ctx, clk, batch); err != nil {
		return err
	}
	after, err := s.Store.List(ctx, "bl")
	if err != nil {
		return err
	}
	remaining := map[string]bool{}
	for _, e := range after {
		remaining[e.EnvelopeID] = true
	}
	if s.w.prunedEnvelopes == nil {
		s.w.prunedEnvelopes = map[string]bool{}
	}
	for _, e := range before {
		if remaining[e.EnvelopeID] {
			continue
		}
		if clk.Now().Sub(s.w.outboxConsumer.publishedAt[e.EnvelopeID]) < outbox.Retention {
			return fmt.Errorf("invariant: envelope %s pruned before publication retention", e.EnvelopeID)
		}
		s.w.prunedEnvelopes[e.EnvelopeID] = true
		s.w.Stats.OutboxPruned++
	}
	return nil
}
