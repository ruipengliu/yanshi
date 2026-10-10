package sim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/usage"
)

// 配额与计量的模拟（docs/design/m4-quota-usage.md §6）：配额设得很紧，使 Run 在故障下频繁因配额挂起、
// 新 Run 被拒绝；随机注销账号，与在途计量竞争。收敛阶段撤销配额（相当于调高配额），
// 挂起的 Run 须经定期复查恢复并结束。

// simPrices：模拟模型每 token 1 微元；沙箱每秒执行 1000 微元（模拟中每次执行占 1 秒）。
var simPrices = &usage.PriceList{Models: map[string]usage.Price{"sim/m": {Input: 1, Output: 1}}}

func init() { simPrices.Sandbox.PerHour = 3.6 }

func simLimits(long bool) map[string]usage.Limits {
	l := usage.Limits{Monthly: 0.015, EndUserDaily: 0.006}
	if long {
		l = usage.Limits{Monthly: 1, EndUserDaily: 0.4}
	}
	if err := l.Prepare(); err != nil {
		panic(err)
	}
	return map[string]usage.Limits{"bl": l}
}

// checkedUsage 在每次记录模型用量时检查：该调用的业务线与 EndUser 在调用时都未超额。
// 模拟中检查、调用与记录在同一个 Step 内完成，其间没有其他记录，因此记录时的已用即检查时的已用。
type checkedUsage struct {
	usage.Store
	w *World
}

func (s *checkedUsage) Record(ctx context.Context, e *usage.Entry) error {
	if e.Kind == usage.Model {
		if p, err := s.w.quotas.Check(ctx, e.BusinessLine, e.EndUser, e.At); err == nil && p != nil {
			s.w.violations = append(s.w.violations, fmt.Sprintf("model call for %s/%s recorded while the %s quota was exceeded (%d ≥ %d)",
				e.BusinessLine, e.EndUser, p.Scope, p.Spent, p.Limit))
		}
	}
	return s.Store.Record(ctx, e)
}

// deleteAccount 注销一个 EndUser：删除其全部 Session，匿名化用量；该位置换成新的 EndUser。
func (w *World) deleteAccount() error {
	ctx := context.Background()
	i := w.rng.IntN(len(w.sessions))
	user := w.users[i]
	ids, err := w.index.IDsOf(ctx, "bl", user)
	if err != nil {
		return err
	}
	if _, err := w.svc.DeleteEndUser(ctx, "bl", user, w.memories, w.usage, w.push); err != nil {
		return fmt.Errorf("delete account %s: %w", user, err)
	}
	if devices, err := w.push.List(ctx, "bl", user); err != nil || len(devices) > 0 {
		return fmt.Errorf("invariant: deleted account %s still has %d push devices (err %v)", user, len(devices), err)
	}
	w.Stats.AccountDeletions++
	w.deletedUsers = append(w.deletedUsers, user)
	for _, id := range ids {
		w.closed = slices.DeleteFunc(w.closed, func(s string) bool { return s == id })
		if !slices.Contains(w.deleted, id) {
			w.deleted = append(w.deleted, id)
		}
	}
	w.tracef("delete account %s (%d sessions)", user, len(ids))
	w.generation++
	w.users[i] = fmt.Sprintf("u%dg%d", i, w.generation)
	return w.replace(i)
}

// checkUsage 检查计量的不变量：
//   - 每个 EndUser 已写入日志的模型 token 不超过已记录的 token（计量先于写日志，不会少计）；
//   - 已注销的 EndUser 不再出现在用量中。
func (w *World) checkUsage() error {
	ctx := context.Background()
	rows, err := w.usage.Report(ctx, usage.Query{BusinessLine: "bl", To: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), GroupBy: usage.ByEndUser})
	if err != nil {
		return err
	}
	recorded := map[string]uint64{}
	for _, r := range rows {
		recorded[r.Key] = r.InputTokens + r.OutputTokens
	}
	for _, u := range w.deletedUsers {
		if _, ok := recorded[u]; ok {
			return fmt.Errorf("invariant: usage of deleted account %s was not anonymized", u)
		}
	}
	committed := map[string]uint64{}
	for _, sid := range append(slices.Clone(w.sessions), w.closed...) {
		events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
		if err != nil {
			return err
		}
		var user string
		for _, e := range events {
			var u *v1.Usage
			switch p := e.GetPayload().(type) {
			case *v1.Event_SessionCreated:
				user = p.SessionCreated.GetEndUser()
			case *v1.Event_AssistantMessage:
				u = p.AssistantMessage.GetUsage()
			case *v1.Event_ContextCompacted:
				u = p.ContextCompacted.GetUsage()
			}
			committed[user] += u.GetInputTokens() + u.GetOutputTokens()
		}
	}
	for user, n := range committed {
		if n > recorded[user] {
			return fmt.Errorf("invariant: %s has %d model tokens in logs but only %d recorded", user, n, recorded[user])
		}
	}
	return nil
}

// submitOverQuota 报告提交是否因配额被拒绝（这是预期结果，不是错误）。
func (w *World) submitOverQuota(err error) bool {
	if !errors.Is(err, usage.ErrExceeded) {
		return false
	}
	w.Stats.QuotaRejections++
	return true
}
