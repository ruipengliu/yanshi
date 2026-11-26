package app

import (
	"context"
	"log/slog"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/consent"
	"yanshi/internal/decision"
	"yanshi/internal/delegation"
	"yanshi/internal/extloop"
	"yanshi/internal/faults"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/oneshot"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

// DefinitionAccess 提供各入口所需的定义访问检查。
type DefinitionAccess interface {
	service.AgentAccess
	oneshot.Access
}

// Options 是装配核心对象图所需的、存储之外的全部输入：时钟与 ID、模型与各项策略。serve 从命令行参数解析出它们，
// 模拟测试、评测与 e2e 测试给出各自的替身。可失败的工作（打开存储、加载 MCP 服务器、解析参数）都在调用方完成后再
// 传入，因此 NewCore 不返回错误。
type Options struct {
	// 必填：Clock、IDs、Agents、Model、Blobs、NodeAuth。
	Clock clock.Clock
	// IDs 按用途返回 ID 生成器；prefix 是 "id"（Session、Run 与调用）、"mem"（Memory）、"art"（工件）或 "use"（单次调用计量），
	// 随机生成器忽略它，模拟测试用它得到可读的顺序 ID。
	IDs    func(prefix string) ids.Generator
	Agents *agentdef.Registry
	// DefinitionAccess 只用于新建 Session、委托与单次调用；nil 时开放全部定义。
	DefinitionAccess DefinitionAccess
	// SessionKeySecret 须在同一单元内的网关间一致；空值禁用带键的创建。
	SessionKeySecret []byte
	// Oneshots 非 nil 时提供单次调用；并发与总时限零值取默认。
	// 同时省略并发与等待时，等待取默认；显式并发配零等待表示只尝试立即获取。
	Oneshots               *oneshot.Registry
	OneshotMax             int
	OneshotWait            time.Duration
	OneshotDeadline        time.Duration
	OneshotBusinessLineMax map[string]int
	Model                  model.Provider
	Blobs                  artifact.BlobStore
	// NodeAuth 验证 Node 连接的身份；没有默认值，开发环境须显式给出 node.InsecureDevAuth{}。
	NodeAuth node.Authenticator

	// Local 是进程内 Capability；nil 时为 clock_now 与 show_card。Memory 与 schedule 的能力总是追加。
	Local []capability.Capability
	// SandboxTools 为 true 时登记沙箱工具；是否有控制器执行它们由调用方按进程角色决定（ControllerConfig）。
	SandboxTools bool
	MCP          capability.MCPSource
	// LoopModelMax 是外部循环经内核模型端点调用的合计并发上限，0 取 extloop.DefaultEndpointMax（ADR-0063）。
	LoopModelMax int
	// Loops 解析 AgentDef 指向的外部循环登记（ADR-0062，extloop.Registry）；nil 时这样的 Run 以配置错误失败。
	Loops runtime.Loops
	// DefaultTimeout 用于未声明超时的设备能力；NodeCacheTTL > 0 时缓存每个 EndUser 的 Node 列表（capability.Catalog）。
	DefaultTimeout time.Duration
	NodeCacheTTL   time.Duration
	// BackgroundGrace 与 BackgroundTimeout 是后台调用的宽限期与后台时限（ADR-0035）：调用离线设备时，设备离线超过宽限期
	// 就转入后台，Run 不再等它；后台调用至多等到后台时限。零值取默认（20 秒、24 小时）；BackgroundGrace < 0 时不转后台，
	// 调用离线设备的 Run 挂起到调用的截止时间。
	BackgroundGrace   time.Duration
	BackgroundTimeout time.Duration
	// EndUserGrace 是注销全部清除完成后的屏障宽限期；零值取 24 小时。
	EndUserGrace time.Duration

	// 以下为 nil 或零值时对应功能关闭，与各包自身的约定一致。
	Moderator moderation.Moderator
	// Injector 非 nil 时，在设备目录与内容安全的 seam 上套故障注入包装（ADR-0033）；模型的包装由调用方在注册
	// 提供商时套，因为它按提供商而非按图。
	Injector       *faults.Injector
	InjectionCheck decision.Decider
	Meter          *usage.Meter
	Quotas         *usage.Quotas
	// Pusher 非 nil 时装配提醒（notify.Notifier）。
	Pusher notify.Pusher
	// HealthAllowed 报告业务线是否开启了健康信息（配置 memory.health，表示业务线有取得单独同意的流程，ADR-0022）。
	// 开启的业务线中，还须业务线为 EndUser 登记同意（Stores.Consents）才可用；nil 表示一律未开启。
	HealthAllowed func(businessLine string) bool
	// HealthEveryone 视开启健康信息的业务线中所有 EndUser 都已同意：-auth none 的开发模式与评测没有业务线服务端来登记
	// （ADR-0022 修订第 4 条）。
	HealthEveryone bool
	Embedder       memory.Embedder
	EmbedModel     string
	// Live 是本进程的增量总线，LiveEndpoint 是供其他进程拉取增量的内部地址（ADR-0013）。
	Live         live.Bus
	LiveEndpoint string
	// LeaseTTL 是 Worker 与控制器的租约时长，续约间隔取其 1/3。
	LeaseTTL     time.Duration
	DrainTimeout time.Duration
	Logger       *slog.Logger
}

// Core 是核心对象图：Event 日志之上、传输层之下的全部平台对象。四个装配点（serve、模拟测试、评测、e2e 测试）
// 都由 NewCore 构造它，跨切面的策略（故障注入、判断、提醒、计量）只在这里接入。
//
// 字段导出是为了让测试替身在构造之后替换：模拟测试把 Service.Nodes 换成会出错的派发、给 Delegator 挂钩子。
// 因此 NewCore 不按值捕获任何其他对象的字段，对象之间一律持指针、在调用时解出；事后改字段是安全的。
type Core struct {
	Stores    Stores
	Store     *session.Store
	Artifacts *artifact.Service
	Hub       *node.Hub
	Memory    *memory.Service
	Catalog   *capability.Catalog
	Delegator *delegation.Delegator
	// Dispatch 是 Worker 的派发链：委托 → 沙箱 → 设备（node.Hub）。
	Dispatch *delegation.Router
	Service  *service.Service
	Oneshots *oneshot.Service
	// LoopModels 是外部循环调用模型的端点（ADR-0063）；Loops 是 *extloop.Registry 时装配，否则为 nil。
	LoopModels *extloop.Endpoint
	// EndUserData 与首次注销共用清理入口，宽限期结束前由 Janitor 再清一遍。
	EndUserData []lifecycle.EndUserHolder
	// Notifier 为 nil 表示没有配置推送通道。
	Notifier *notify.Notifier

	opts Options
}

// NewCore 装配核心对象图。
func NewCore(stores Stores, opts Options) *Core {
	if opts.EndUserGrace <= 0 {
		opts.EndUserGrace = lifecycle.DefaultEndUserGrace
	}
	if opts.Meter != nil {
		opts.Meter.Deletions = stores.Deletions
	}
	if opts.IDs == nil {
		opts.IDs = func(string) ids.Generator { return ids.Random() }
	}
	// 配额、管理接口与注销必须共用缓存，才能使本进程写后立即生效。
	var tiers usage.TierStore
	if stores.Tiers != nil {
		tiers = usage.NewTierCache(stores.Tiers, opts.Clock)
	}
	if opts.Quotas != nil {
		opts.Quotas.Tiers = tiers
	}
	// 健康信息的同意（ADR-0022 修订）：管理接口、Memory 闸门与注销共用缓存，撤回在本进程立即生效。
	var consents consent.Store
	if stores.Consents != nil {
		consents = consent.NewCache(stores.Consents, opts.Clock)
	}
	c := &Core{Stores: stores, opts: opts}
	c.Store = &session.Store{Log: stores.Log, Waking: stores.Waking, IDs: opts.IDs("id"), Clock: opts.Clock,
		Snapshots: stores.Snapshots, Deletions: stores.Deletions, Outbox: stores.Outbox}
	c.Artifacts = &artifact.Service{Meta: stores.ArtifactMeta, Blobs: opts.Blobs, IDs: opts.IDs("art"), Clock: opts.Clock,
		Deletions: stores.Deletions}
	c.Hub = &node.Hub{Dir: stores.Dir, Inbox: stores.Inbox, Store: c.Store, Auth: opts.NodeAuth, Push: stores.Push,
		Deletions: stores.Deletions, Clock: opts.Clock, Logger: opts.Logger}
	if opts.Logger != nil {
		c.Hub.Waker = node.LogWaker{Logger: opts.Logger}
	}
	if opts.Pusher != nil {
		// 提醒（docs/design/m3-duplex-channel.md §9）
		c.Notifier = &notify.Notifier{Registry: stores.Push, Presence: stores.Presence, Pusher: opts.Pusher, Clock: opts.Clock,
			Logger: opts.Logger}
	}
	// Memory（docs/design/m4-memory-grant.md）：Agent 通过能力读写，Run 开始时召回。
	c.Memory = &memory.Service{Store: stores.Memories, Grants: stores.Grants, Deletions: stores.Deletions, Clock: opts.Clock,
		IDs: opts.IDs("mem"), Embedder: opts.Embedder, EmbedModel: opts.EmbedModel}
	if opts.HealthAllowed != nil {
		c.Memory.Health = c.healthAllowed
	}
	local := opts.Local
	if local == nil {
		local = []capability.Capability{capability.ClockNow(opts.Clock), card.New(c.Artifacts.Meta)}
	}
	var dir node.Directory = stores.Dir
	if opts.Injector != nil {
		dir = faults.Directory{Directory: dir, Injector: opts.Injector}
	}
	c.Catalog = &capability.Catalog{
		Local:          capability.NewRegistry(),
		Nodes:          dir,
		MCP:            opts.MCP,
		Agents:         delegation.Directory{Registry: opts.Agents, Access: opts.DefinitionAccess},
		DefaultTimeout: opts.DefaultTimeout, NodeCacheTTL: opts.NodeCacheTTL, Clock: opts.Clock,
	}
	if opts.SandboxTools {
		c.Catalog.Sandbox = &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}
	}
	// 委托（docs/design/m5-multi-agent.md）：Agent 是一种 Capability，派发目标是由调用派生的子 Session。
	c.Delegator = &delegation.Delegator{Artifacts: c.Artifacts, Logger: opts.Logger}
	c.Dispatch = &delegation.Router{Next: &sandbox.Router{Hub: c.Hub, Queue: stores.SandboxQueue}, Delegator: c.Delegator}
	moderator := opts.Moderator
	if opts.Injector != nil {
		moderator = faults.Moderator{Next: moderator, Injector: opts.Injector}
	}
	c.Service = &service.Service{Store: c.Store, Agents: opts.Agents, AgentAccess: opts.DefinitionAccess, Nodes: c.Dispatch, Index: stores.Index,
		Deletions: stores.Deletions, Janitor: stores.JanitorQueue, Quotas: opts.Quotas, Tiers: tiers, Moderator: moderator, Artifacts: c.Artifacts,
		Consents: consents, HealthEnabled: opts.HealthAllowed, SessionKeySecret: opts.SessionKeySecret}
	if tiers != nil {
		c.EndUserData = append(c.EndUserData, lifecycle.EndUserHolder{Name: "tiers", Delete: func(ctx context.Context, bl, eu string) error {
			if c.Service.Tiers == nil {
				return nil
			}
			return c.Service.Tiers.Delete(ctx, bl, eu)
		}, Invalidate: func(bl, eu string) {
			if cache, ok := c.Service.Tiers.(interface{ Invalidate(string, string) }); ok {
				cache.Invalidate(bl, eu)
			}
		}})
	}
	if consents != nil {
		c.EndUserData = append(c.EndUserData, lifecycle.EndUserHolder{Name: "consents", Delete: func(ctx context.Context, bl, eu string) error {
			if c.Service.Consents == nil {
				return nil
			}
			return c.Service.Consents.DeleteEndUser(ctx, bl, eu)
		}, Invalidate: func(bl, eu string) {
			if cache, ok := c.Service.Consents.(interface{ Invalidate(string, string) }); ok {
				cache.Invalidate(bl, eu)
			}
		}})
	}
	if opts.Oneshots != nil {
		maxCalls, wait, deadline := opts.OneshotMax, opts.OneshotWait, opts.OneshotDeadline
		if maxCalls == 0 {
			maxCalls = oneshot.DefaultMax
		}
		if opts.OneshotMax == 0 && wait == 0 {
			wait = oneshot.DefaultWait
		}
		if deadline == 0 {
			deadline = oneshot.DefaultDeadline
		}
		c.Oneshots = &oneshot.Service{Registry: opts.Oneshots, Access: opts.DefinitionAccess, Model: opts.Model, Moderator: moderator,
			Quotas: opts.Quotas, Meter: opts.Meter, Deletions: stores.Deletions, Clock: opts.Clock, IDs: opts.IDs("use"),
			Max: maxCalls, BusinessLineMax: opts.OneshotBusinessLineMax, Wait: wait, Deadline: deadline}
	}
	if reg, ok := opts.Loops.(*extloop.Registry); ok && reg != nil {
		c.LoopModels = &extloop.Endpoint{Keys: reg.Keys, Agents: opts.Agents, Model: opts.Model, Quotas: opts.Quotas, Meter: opts.Meter, Max: opts.LoopModelMax,
			Deletions: stores.Deletions, Clock: opts.Clock, IDs: opts.IDs("use"), Logger: opts.Logger}
		if opts.DefinitionAccess != nil {
			c.LoopModels.Access = opts.DefinitionAccess
		}
	}
	local = append(append([]capability.Capability{}, local...), memory.Capabilities(c.Memory)...)
	c.Catalog.Local = capability.NewRegistry(append(local, c.Service.ScheduleCapability())...)
	c.Delegator.Service = c.Service
	// 设备结果经 Service 写入：调用已转入后台时，结果是一条输入（ADR-0035）。
	c.Hub.Results = c.Service
	c.Hub.Queue = stores.Queue
	return c
}

// healthAllowed 是 Memory 的健康信息闸门（ADR-0022 修订）：业务线开启，且该 EndUser 已同意（或开发模式与评测视为都已同意）。
// 同意记录在调用时经 Service.Consents（共享缓存）读取，测试替换字段后同样生效。
func (c *Core) healthAllowed(ctx context.Context, businessLine, endUser string) (bool, error) {
	if c.opts.HealthAllowed == nil || !c.opts.HealthAllowed(businessLine) {
		return false, nil
	}
	if c.opts.HealthEveryone {
		return true, nil
	}
	if c.Service.Consents == nil || endUser == "" {
		return false, nil
	}
	at, err := c.Service.Consents.Get(ctx, businessLine, endUser, consent.Health)
	return !at.IsZero(), err
}

// WorkerConfig 返回第 id 个 Worker 的配置：pool 与 idle 是按 Worker 不同的两项（交互池与后台池各有一个空闲门控，
// ADR-0029），其余来自对象图。返回值可以在 runtime.New 之前修改。
func (c *Core) WorkerConfig(id string, pool workqueue.Pool, idle *workqueue.IdleGate) runtime.Config {
	cfg := runtime.Config{
		ID: id, Store: c.Store, Queue: c.Stores.Queue, Agents: c.opts.Agents, Model: c.opts.Model, Loops: c.opts.Loops,
		Catalog: c.Catalog, Dispatch: c.Dispatch, Delegation: c.Delegator, InjectionCheck: c.opts.InjectionCheck,
		Artifacts: c.Artifacts, Memory: c.Memory, Live: c.opts.Live, LiveEndpoint: c.opts.LiveEndpoint, Logger: c.opts.Logger,
		LeaseTTL: c.opts.LeaseTTL, Heartbeat: c.opts.LeaseTTL / 3, Idle: idle, Pool: pool,
		Meter: c.opts.Meter, Quotas: c.opts.Quotas, Moderator: c.Service.Moderator,
		DrainTimeout: c.opts.DrainTimeout, Faults: c.opts.Injector,
		Schedules: c.Service, Background: c.Service, BackgroundGrace: c.opts.BackgroundGrace, BackgroundTimeout: c.opts.BackgroundTimeout,
	}
	if c.opts.BackgroundGrace >= 0 {
		cfg.Devices = c.Stores.Dir
	}
	if c.Notifier != nil {
		cfg.Notify = c.Notifier
	}
	return cfg
}

// ControllerConfig 返回第 id 个沙箱控制器；provider 随进程角色而变（没有沙箱角色的进程不启动控制器），
// 不属于对象图。返回值可以在 Run 之前修改。
func (c *Core) ControllerConfig(id string, provider sandbox.Provider, idle *workqueue.IdleGate) *sandbox.Controller {
	return &sandbox.Controller{
		ID: id, Queue: c.Stores.SandboxQueue, Hub: c.Hub, Provider: provider,
		Activity: c.Stores.Activity, Ledger: c.Stores.Ledger, Artifacts: c.Artifacts, Clock: c.opts.Clock, Logger: c.opts.Logger,
		Lifecycle: &lifecycle.Guard{Index: c.Stores.Index, Deletions: c.Stores.Deletions}, Idle: idle, Meter: c.opts.Meter,
		LeaseTTL: c.opts.LeaseTTL, Heartbeat: c.opts.LeaseTTL / 3,
	}
}

// Janitor 返回 Janitor：回收已关闭与已删除 Session 的资源，执行各业务线的保留策略（docs/design/m2-session-lifecycle.md）。
// provider 为 nil 表示本进程没有沙箱，工作区无需销毁。
func (c *Core) Janitor(id string, provider sandbox.Provider, retention map[string]lifecycle.Retention, usageRetention time.Duration) *janitor.Janitor {
	workspaces := sandbox.Workspaces{Queue: c.Stores.SandboxQueue, Provider: provider, Activity: c.Stores.Activity}
	return &janitor.Janitor{
		ContinueIndependent: c.Stores.shards != nil && !c.Stores.shards.deterministic,
		ID:                  id, Queue: c.Stores.JanitorQueue, Outbox: c.Stores.Outbox, Service: c.Service, Store: c.Store, Data: SessionData(c.Stores, workspaces, c.Artifacts),
		Workspaces: workspaces, Inbox: c.Stores.Inbox, Nodes: c.Stores.Dir, Ledger: c.Stores.Ledger,
		Queues: map[string]workqueue.Queue{"sessions": c.Stores.Queue, "sandboxes": c.Stores.SandboxQueue, "janitor": c.Stores.JanitorQueue},
		Index:  c.Stores.Index, Deletions: c.Stores.Deletions, Retention: retention, Clock: c.opts.Clock, Logger: c.opts.Logger,
		LeaseTTL: c.opts.LeaseTTL, Usage: c.Stores.Usage, UsageRetention: usageRetention, Presence: c.Stores.Presence,
		EndUserGrace: c.opts.EndUserGrace, EndUserData: c.EndUserData,
	}
}
