package sim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/service"
	"yanshi/internal/usage"
)

func (w *World) endUserWrite(write func() error) error {
	hook := w.endUserWriteHook
	if hook == nil {
		return write()
	}
	w.endUserWriteHook = nil
	if w.endUserWriteBefore {
		if err := hook(); err != nil {
			return err
		}
		return write()
	}
	if err := write(); err != nil {
		return err
	}
	return hook()
}

type barrierIndex struct {
	lifecycle.Index
	w *World
}

func (x barrierIndex) Put(ctx context.Context, s *lifecycle.Session) error {
	return x.w.endUserWrite(func() error { return x.Index.Put(ctx, s) })
}

type barrierDirectory struct {
	node.Directory
	w *World
}

func (d barrierDirectory) Register(ctx context.Context, info node.Info) (label string, gen uint64, err error) {
	err = d.w.endUserWrite(func() error {
		var e error
		label, gen, e = d.Directory.Register(ctx, info)
		return e
	})
	return
}

type barrierPush struct {
	notify.Registry
	w *World
}

func (r barrierPush) Register(ctx context.Context, d *notify.Device, at time.Time) error {
	return r.w.endUserWrite(func() error { return r.Registry.Register(ctx, d, at) })
}

type barrierMemory struct {
	memory.Store
	w *World
}

func (s barrierMemory) Put(ctx context.Context, m *memory.Memory, v *memory.Vector) error {
	return s.w.endUserWrite(func() error { return s.Store.Put(ctx, m, v) })
}

type barrierGrants struct {
	memory.Grants
	w *World
}

func (s barrierGrants) Create(ctx context.Context, g *memory.Grant) error {
	return s.w.endUserWrite(func() error { return s.Grants.Create(ctx, g) })
}

type barrierTiers struct {
	usage.TierStore
	w *World
}

func (s barrierTiers) Put(ctx context.Context, bl, eu, tier string, at time.Time) error {
	err := s.w.endUserWrite(func() error { return s.TierStore.Put(ctx, bl, eu, tier, at) })
	if err == nil && s.w.skipTierRecheck {
		panic(tierWriterCrash{})
	}
	return err
}

type tierWriterCrash struct{}

// crashTierWriter 在存储提交后直接中断调用栈，不执行 Service 的写后复查。
func (w *World) crashTierWriter(ctx context.Context, user string) (err error) {
	w.skipTierRecheck = true
	defer func() {
		w.skipTierRecheck = false
		if v := recover(); v != nil {
			if _, ok := v.(tierWriterCrash); !ok {
				panic(v)
			}
			if w.crashedTierUsers == nil {
				w.crashedTierUsers = map[string]bool{}
			}
			w.crashedTierUsers[user] = true
			w.Stats.TierWriterCrashes++
			err = nil
		}
	}()
	_, err = w.svc.PutTier(ctx, "bl", user, "pro")
	return fmt.Errorf("invariant: tier writer did not crash: %v", err)
}

func (w *World) deleteWithEndUserWrite(user string) error {
	ctx := context.Background()
	remove := func() error {
		_, err := w.svc.DeleteEndUser(ctx, "bl", user, w.hub, w.memories, w.usage, w.push)
		return err
	}
	actions := 7
	if w.faults {
		actions++
	}
	action := w.rng.IntN(actions)
	// 崩溃路径固定先注销再落盘，保证首次清理漏过这一行；其他路径覆盖三种写入顺序。
	if action == 7 {
		w.endUserWriteHook, w.endUserWriteBefore = remove, true
	} else {
		switch w.rng.IntN(3) {
		case 0:
			w.endUserWriteHook, w.endUserWriteBefore = remove, true
		case 1:
			w.endUserWriteHook, w.endUserWriteBefore = remove, false
		default:
			if err := remove(); err != nil {
				return err
			}
		}
	}
	defer func() { w.endUserWriteHook = nil }()
	w.nextInput++
	id := fmt.Sprintf("barrier-%d", w.nextInput)
	var err error
	var want error
	switch action {
	case 0:
		_, err = w.svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: user, Agent: "sim"})
		want = service.ErrNotFound
	case 1:
		w.meter.Speech(ctx, usage.Scope{BusinessLine: "bl", EndUser: user}, id, "sim@1", "sim/m", 1, 0, w.clock.Now())
		rows, e := w.usage.Report(ctx, usage.Query{BusinessLine: "bl", EndUser: user, To: w.clock.Now().Add(time.Hour), GroupBy: usage.ByEndUser})
		if e != nil || len(rows) != 0 {
			return fmt.Errorf("invariant: late usage was not anonymized: %v", e)
		}
	case 2, 3:
		_, err = w.hub.Connect(ctx, &v1.Hello{NodeId: id, BusinessLine: "bl", EndUser: user, Label: "late", ClientOnly: w.chance(0.5), PushPlatform: "sim", PushToken: "late-token"})
		want = node.ErrNotFound
	case 4:
		maxPerUser := w.memories.MaxPerUser
		w.memories.MaxPerUser = 1 << 20
		_, err = w.memories.Save(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: user, CallID: id, Category: "preference", Content: "偏好迟到写入 " + id})
		w.memories.MaxPerUser = maxPerUser
		want = memory.ErrNotFound
	case 5:
		from, to := "bl", "other"
		if w.chance(0.5) {
			from, to = to, from
		}
		_, err = w.memories.CreateGrant(ctx, user, from, to, []memory.Category{"preference"}, time.Time{})
		want = memory.ErrNotFound
	case 6:
		interleaved := w.endUserWriteHook != nil
		_, err = w.svc.PutTier(ctx, "bl", user, "pro")
		want = service.ErrNotFound
		if interleaved && errors.Is(err, want) {
			w.Stats.TiersWithdrawn++
		}
	case 7:
		if err := w.crashTierWriter(ctx, user); err != nil {
			return err
		}
		if tier, err := w.stores.Tiers.Get(ctx, "bl", user); err != nil || tier != "pro" {
			return fmt.Errorf("invariant: skipped recheck left no late tier: %q, %v", tier, err)
		}
	}
	if want != nil && !errors.Is(err, want) {
		return fmt.Errorf("invariant: write during account deletion returned %v, want %v", err, want)
	}
	if w.endUserWriteHook != nil {
		return errors.New("invariant: account deletion did not interleave with the write")
	}
	if action != 7 {
		w.Stats.EndUserBarrierWithdrawn++
	}
	return nil
}

func (w *World) checkEndUserDeleted(final bool) error {
	ctx := context.Background()
	for _, user := range w.deletedUsers {
		b, err := w.deletions.GetEndUser(ctx, "bl", user)
		if errors.Is(err, lifecycle.ErrNotFound) {
			// 模拟注销后换新 ID；原 ID 的屏障移除后不能留下崩溃写入。
			if tier, err := w.stores.Tiers.Get(ctx, "bl", user); err != nil || tier != "" {
				return errors.New("invariant: tier survived removal of account deletion barrier")
			}
			continue
		}
		if err != nil {
			return err
		}
		if final && b.CompletedAt.IsZero() {
			return errors.New("invariant: account deletion never completed")
		}
		if b.CompletedAt.IsZero() {
			continue
		}
		if !w.crashedTierUsers[user] {
			if tier, err := w.stores.Tiers.Get(ctx, "bl", user); err != nil || tier != "" {
				return errors.New("invariant: deleted account still has a tier")
			}
		}
		if ids, err := w.index.IDsOf(ctx, "bl", user); err != nil || len(ids) != 0 {
			return errors.New("invariant: deleted account still has indexed sessions")
		}
		if nodes, err := w.hub.Dir.List(ctx, node.Scope{BusinessLine: "bl", EndUser: user}); err != nil || len(nodes) != 0 {
			return errors.New("invariant: deleted account still has nodes")
		}
		if devices, err := w.push.List(ctx, "bl", user); err != nil || len(devices) != 0 {
			return errors.New("invariant: deleted account still has push devices")
		}
		if memories, err := w.memories.Store.List(ctx, user, []memory.Scope{{BusinessLine: "bl"}}, memory.Everything, 1); err != nil || len(memories) != 0 {
			return errors.New("invariant: deleted account still has memories")
		}
		if grants, err := w.memories.ListGrants(ctx, user, "bl"); err != nil || len(grants) != 0 {
			return errors.New("invariant: deleted account still has grants")
		}
	}
	return nil
}

func (w *World) fingerprintEndUsers(h io.Writer) {
	for _, user := range append(slices.Clone(w.users), w.deletedUsers...) {
		tier, err := w.stores.Tiers.Get(context.Background(), "bl", user)
		fmt.Fprintf(h, "tier:%s|%s|%v;", user, tier, err)
	}
	for _, user := range w.deletedUsers {
		b, err := w.deletions.GetEndUser(context.Background(), "bl", user)
		if err != nil {
			fmt.Fprint(h, "barrier missing;")
			continue
		}
		fmt.Fprintf(h, "%s|%s|%v|%d|%d|%d|%d;", b.BusinessLine, b.RequestID, b.Sealed, b.RequestedAt.UnixNano(), b.Revision, b.CompletedAt.UnixNano(), b.ExpiresAt.UnixNano())
		for _, sid := range slices.Sorted(slices.Values(b.Sessions)) {
			fmt.Fprint(h, sid, ";")
		}
	}
}

// expireEndUserBarriers 保证普通种子也走过宽限期，不依赖随机拨钟恰好超过一天。
func (w *World) expireEndUserBarriers() error {
	ctx := context.Background()
	until := w.clock.Now()
	for _, user := range w.deletedUsers {
		b, err := w.deletions.GetEndUser(ctx, "bl", user)
		if errors.Is(err, lifecycle.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if b.CompletedAt.IsZero() {
			return errors.New("invariant: account deletion never completed before grace expiry")
		}
		if b.ExpiresAt.After(until) {
			until = b.ExpiresAt
		}
	}
	w.clock.Advance(until.Sub(w.clock.Now()))
	// PruneEndUsers 每轮最多 100 条，逐批清理到所有已完成的注销都越过屏障。
	for round := 0; round <= len(w.deletedUsers)/100; round++ {
		if err := w.janitors[0].Sweep(ctx); err != nil {
			return err
		}
	}
	for _, user := range w.deletedUsers {
		if _, err := w.deletions.GetEndUser(ctx, "bl", user); !errors.Is(err, lifecycle.ErrNotFound) {
			return fmt.Errorf("invariant: expired account deletion barrier remains: %v", err)
		}
		if w.crashedTierUsers[user] {
			w.Stats.TierCrashRecoveries++
		}
	}
	return w.checkEndUserDeleted(true)
}
