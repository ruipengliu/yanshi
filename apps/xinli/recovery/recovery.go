// Package recovery 实现出站过期后的对账；选定事件与当前可访问状态收敛，不补发全部历史通知。
package recovery

import (
	"context"
	"errors"

	"xinli/account"
	"xinli/kernel"
)

var ErrConflict = errors.New("recovery: progress changed")

const (
	Heads    = "heads"
	Owners   = "owners"
	Sessions = "sessions"
	Events   = "events"
	Confirm  = "confirm"
	Catchup  = "catchup"
	Follow   = "follow"
)

type Position struct {
	Shard int
	H     string
	After string
	Done  bool
}

// Progress 只含位置和 ID；每个业务线/分片一份，CAS 拒绝崩溃前的迟到执行者。
// H 固定到本轮结束，After 是追赶位置，两者不能在枚举期间用新 head 覆盖。
type Progress struct {
	BusinessLine string
	Slots        []int
	Version      uint64
	// Revision 只由恢复步骤和重置推进，区分并发收页与另一位恢复执行者。
	Revision     uint64
	Round        uint64
	Phase        string
	Positions    []Position
	Shard        int
	OwnerAfter   string
	Owner        string
	SessionAfter string
	Session      string
	ReturnPhase  string
	ConfirmAfter string
}

type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type QuestionCall struct {
	RunID   string
	Started bool
}

// Checkpoint 只保留选择信封所需的元数据，不保存输入、输出或调用参数。
// Seq 仅在完整事件页提交时推进，收到稀疏出站信封不能推进它。
type Checkpoint struct {
	SessionID  string
	EndUser    string
	Seq        uint64
	Agent      Agent
	Questions  map[string]QuestionCall
	Background map[string]string
	SeenRound  uint64
	Gone       bool
}

type Batch struct {
	Checkpoint *Checkpoint
	Envelopes  []kernel.Envelope
	// Inaccessible 只能来自逐 ID 读取 404；不伪造 session_deleted 信封或 Event seq。
	Inaccessible string
}

// Consumer 是 B6 的事务接入点。Commit 必须原子校验 Version、去重接收记录、保存 Checkpoint 与 Progress。
// 消费者只能在提交后安排副作用，发送前另读当前可访问内容；404 要撤销已有发送。
// Known 返回 SeenRound != round 且未确认 Gone 的 Session，按 ID 升序，不能以未列出判删。
// 普通信封按 envelope_id 和 (session_id,seq) 去重；恢复等价物没有 envelope_id。
type Consumer interface {
	Load(context.Context, string) (Progress, error)
	Session(context.Context, string, string) (Checkpoint, error)
	Known(context.Context, string, uint64, string, int) ([]Checkpoint, error)
	Commit(context.Context, uint64, Progress, Batch) error
}

type Kernel interface {
	OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error)
	Outbox(context.Context, string, int, string, int) (kernel.OutboxPage, error)
	StableSessions(context.Context, string, string, string, int) ([]kernel.SessionInfo, error)
	SessionView(context.Context, string, string) (kernel.SessionView, error)
	Events(context.Context, string, string, uint64, int) (kernel.EventPage, error)
}

type Recoverer struct {
	Accounts account.Enumerator
	Kernel   Kernel
	Consumer Consumer
	PageSize int
}

// Step 做一次有界读取并提交进度。任意网络失败保留旧位置；进程重建只需复用同一 Consumer。
// 首次启动也先对账；B6 可以用同一 Commit 接入已有出站位置，410 自动进入 Heads。
func (r *Recoverer) Step(ctx context.Context, line string) (Progress, error) {
	if line == "" || r.Accounts == nil || r.Kernel == nil || r.Consumer == nil {
		return Progress{}, errors.New("recovery: missing configuration")
	}
	p, err := r.Consumer.Load(ctx, line)
	if err != nil {
		return p, err
	}
	version := p.Version
	observed := p
	batch := Batch{}
	limit := r.PageSize
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	switch p.Phase {
	case "", Heads:
		heads, err := r.Kernel.OutboxHeads(ctx, line)
		if err != nil {
			return p, err
		}
		if len(heads) == 0 {
			return p, errors.New("recovery: missing heads")
		}
		next := Progress{BusinessLine: line, Round: p.Round + 1, Phase: Owners}
		seen := map[int]bool{}
		for _, h := range heads {
			if id := Shard(ctx); id >= 0 && h.Shard != id {
				continue
			}
			next.Slots = append(next.Slots, h.Slots...)
			if h.Status != "ready" || h.Next == "" || h.Shard < 0 || seen[h.Shard] {
				return p, errors.New("recovery: unavailable shard")
			}
			seen[h.Shard] = true
			next.Positions = append(next.Positions, Position{Shard: h.Shard, H: h.Next, After: h.Next})
		}
		if len(next.Positions) == 0 {
			return p, errors.New("recovery: missing shard")
		}
		p = next
	case Owners:
		owners, err := r.Accounts.AccountOwners(ctx, line, p.OwnerAfter, 1)
		if err != nil {
			return p, err
		}
		if len(owners) == 0 {
			p.Phase, p.Owner = Confirm, ""
		} else {
			if owners[0].BusinessLine != line || owners[0].EndUser <= p.OwnerAfter {
				return p, errors.New("recovery: invalid account page")
			}
			p.Owner, p.SessionAfter, p.Phase = owners[0].EndUser, "", Sessions
		}
	case Sessions:
		page, err := r.Kernel.StableSessions(ctx, line, p.Owner, p.SessionAfter, 1)
		if err != nil {
			return p, err
		}
		if len(page) == 0 {
			p.OwnerAfter, p.Owner, p.Phase = p.Owner, "", Owners
		} else {
			if page[0].ID <= p.SessionAfter || page[0].EndUser != p.Owner {
				return p, errors.New("recovery: invalid session page")
			}
			p.Session, p.ReturnPhase, p.Phase = page[0].ID, Sessions, Events
		}
	case Confirm:
		known, err := r.Consumer.Known(ctx, line, p.Round, p.ConfirmAfter, 1)
		if err != nil {
			return p, err
		}
		if len(known) == 0 {
			p.Phase, p.Shard = Catchup, 0
		} else {
			p.Session, p.Owner, p.ReturnPhase, p.Phase = known[0].SessionID, known[0].EndUser, Confirm, Events
		}
	case Events:
		if !ownsSession(p.Session, Shard(ctx), p.Slots) {
			finishSession(&p)
			break
		}
		cp, err := r.Consumer.Session(ctx, line, p.Session)
		if err != nil {
			return p, err
		}
		if cp.Gone {
			finishSession(&p)
			break
		}
		// 服务令牌只有业务线约束，逐 ID 读取仍核验账号归属。
		view, err := r.Kernel.SessionView(ctx, line, p.Session)
		if errors.Is(err, kernel.ErrNotFound) {
			batch.Inaccessible = p.Session
			finishSession(&p)
			break
		}
		if err != nil {
			return p, err
		}
		if view.ID != p.Session || view.BusinessLine != line || view.EndUser != p.Owner {
			return p, errors.New("recovery: session owner mismatch")
		}
		if cp.EndUser != "" && cp.EndUser != p.Owner {
			return p, errors.New("recovery: checkpoint owner mismatch")
		}
		cp.SessionID, cp.EndUser = p.Session, p.Owner
		page, err := r.Kernel.Events(ctx, line, p.Session, cp.Seq, limit)
		if errors.Is(err, kernel.ErrNotFound) {
			batch.Inaccessible = p.Session
			finishSession(&p)
			break
		}
		if err != nil {
			return p, err
		}
		batch.Envelopes, err = Replay(line, &cp, page.Events)
		if err != nil {
			return p, err
		}
		cp.SeenRound = p.Round
		batch.Checkpoint = &cp
		if len(page.Events) < limit {
			finishSession(&p)
		}
	case Catchup, Follow:
		if len(p.Positions) == 0 || p.Shard < 0 || p.Shard >= len(p.Positions) {
			return p, errors.New("recovery: invalid shard progress")
		}
		pos := &p.Positions[p.Shard]
		page, err := r.Kernel.Outbox(ctx, line, pos.Shard, pos.After, limit)
		if kernel.OutboxResetRequired(err) {
			// 新 H 必须先于下一轮枚举取得；不能用空 after 悄悄跳过过期部分。
			p = Progress{BusinessLine: line, Round: p.Round, Phase: Heads}
			break
		}
		if err != nil {
			return p, err
		}
		if page.Next == "" {
			return p, errors.New("recovery: missing next cursor")
		}
		batch.Envelopes, pos.After = page.Events, page.Next
		if p.Phase == Catchup && len(page.Events) == 0 {
			pos.Done = true
		}
		// 每步轮换分片，某个持续繁忙的分片不能饿死其他分片。
		p.Shard = (p.Shard + 1) % len(p.Positions)
		if p.Phase == Catchup {
			all := true
			for _, position := range p.Positions {
				all = all && position.Done
			}
			if all {
				p.Phase = Follow
			}
		}
	default:
		return p, errors.New("recovery: unknown phase")
	}
	p.BusinessLine, p.Version = line, version+1
	if Shard(ctx) >= 0 {
		p.Revision = observed.Revision + 1
	}
	if err := r.commit(ctx, observed, &p, batch); err != nil {
		return Progress{}, err
	}
	return p, nil
}

// 网络读取之后收页可以先提交；只合并游标写入，不覆盖其他恢复步骤或新世代的重置。
func (r *Recoverer) commit(ctx context.Context, observed Progress, next *Progress, batch Batch) error {
	expected := observed.Version
	for attempt := 0; attempt < 8; attempt++ {
		err := r.Consumer.Commit(ctx, expected, *next, batch)
		if !errors.Is(err, ErrConflict) || Shard(ctx) < 0 {
			return err
		}
		current, err := r.Consumer.Load(ctx, observed.BusinessLine)
		if err != nil {
			return err
		}
		if current.Revision != observed.Revision || current.Phase != observed.Phase || current.Round != observed.Round {
			return ErrConflict
		}
		// 收页只会改 After 与 Version；它接收的信封已经和当前位置原子落盘。
		for i := range next.Positions {
			for _, pos := range current.Positions {
				if next.Positions[i].Shard == pos.Shard && next.Positions[i].H == pos.H {
					next.Positions[i].After = pos.After
				}
			}
		}
		expected, next.Version = current.Version, current.Version+1
	}
	return ErrConflict
}

func finishSession(p *Progress) {
	if p.ReturnPhase == Sessions {
		p.SessionAfter = p.Session
	} else {
		p.ConfirmAfter = p.Session
	}
	p.Session, p.Phase = "", p.ReturnPhase
	p.ReturnPhase = ""
}
