package app

import (
	"log/slog"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/decision"
	"yanshi/internal/delegation"
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
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

// Options 是装配核心对象图所需的、存储之外的全部输入：时钟与 ID、模型与各项策略。serve 从命令行参数解析出它们，
// 模拟测试、评测与 e2e 测试给出各自的替身。可失败的工作（打开存储、加载 MCP 服务器、解析参数）都在调用方完成后再
// 传入，因此 NewCore 不返回错误。
type Options struct {
	// 必填：Clock、IDs、Agents、Model、Blobs、NodeAuth。
	Clock clock.Clock
	// IDs 按用途返回 ID 生成器；prefix 是 "id"（Session、Run 与调用）、"mem"（Memory）或 "art"（工件），
	// 随机生成器忽略它，模拟测试用它得到可读的顺序 ID。
	IDs    func(prefix string) ids.Generator
	Agents *agentdef.Registry
	Model  model.Provider
	Blobs  artifact.BlobStore
	// NodeAuth 验证 Node 连接的身份；没有默认值，开发环境须显式给出 node.InsecureDevAuth{}。
	NodeAuth node.Authenticator

	// Local 是进程内 Capability；nil 时为 clock_now 与 show_card。Memory 的能力总是追加。
	Local []capability.Capability
	// SandboxTools 为 true 时登记沙箱工具；是否有控制器执行它们由调用方按进程角色决定（ControllerConfig）。
	SandboxTools bool
	MCP          capability.MCPSource
	// DefaultTimeout 用于未声明超时的设备能力；NodeCacheTTL > 0 时缓存每个 EndUser 的 Node 列表（capability.Catalog）。
	DefaultTimeout time.Duration
	NodeCacheTTL   time.Duration

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
	// HealthAllowed 报告业务线是否已取得健康信息的同意（ADR-0022）。
	HealthAllowed func(businessLine string) bool
	Embedder      memory.Embedder
	EmbedModel    string
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
	// Notifier 为 nil 表示没有配置推送通道。
	Notifier *notify.Notifier

	opts Options
}

// NewCore 装配核心对象图。
func NewCore(stores Stores, opts Options) *Core {
	if opts.IDs == nil {
		opts.IDs = func(string) ids.Generator { return ids.Random() }
	}
	c := &Core{Stores: stores, opts: opts}
	c.Store = &session.Store{Log: stores.Log, Waking: stores.Waking, IDs: opts.IDs("id"), Clock: opts.Clock,
		Snapshots: stores.Snapshots, Deletions: stores.Deletions}
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
		IDs: opts.IDs("mem"), HealthAllowed: opts.HealthAllowed, Embedder: opts.Embedder, EmbedModel: opts.EmbedModel}
	local := opts.Local
	if local == nil {
		local = []capability.Capability{capability.ClockNow(opts.Clock), card.New(c.Artifacts.Meta)}
	}
	var dir node.Directory = stores.Dir
	if opts.Injector != nil {
		dir = faults.Directory{Directory: dir, Injector: opts.Injector}
	}
	c.Catalog = &capability.Catalog{
		Local:          capability.NewRegistry(append(append([]capability.Capability{}, local...), memory.Capabilities(c.Memory)...)...),
		Nodes:          dir,
		MCP:            opts.MCP,
		Agents:         delegation.Directory{Registry: opts.Agents},
		DefaultTimeout: opts.DefaultTimeout, NodeCacheTTL: opts.NodeCacheTTL, Clock: opts.Clock,
	}
	if opts.SandboxTools {
		c.Catalog.Sandbox = &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}
	}
	// 委托（docs/design/m5-multi-agent.md）：Agent 是一种 Capability，派发目标是由调用派生的子 Session。
	c.Delegator = &delegation.Delegator{Logger: opts.Logger}
	c.Dispatch = &delegation.Router{Next: &sandbox.Router{Hub: c.Hub, Queue: stores.SandboxQueue}, Delegator: c.Delegator}
	moderator := opts.Moderator
	if opts.Injector != nil {
		moderator = faults.Moderator{Next: moderator, Injector: opts.Injector}
	}
	c.Service = &service.Service{Store: c.Store, Agents: opts.Agents, Nodes: c.Dispatch, Index: stores.Index,
		Deletions: stores.Deletions, Janitor: stores.JanitorQueue, Quotas: opts.Quotas, Moderator: moderator, Artifacts: c.Artifacts}
	c.Delegator.Service = c.Service
	return c
}

// WorkerConfig 返回第 id 个 Worker 的配置：pool 与 idle 是按 Worker 不同的两项（交互池与后台池各有一个空闲门控，
// ADR-0029），其余来自对象图。返回值可以在 runtime.New 之前修改。
func (c *Core) WorkerConfig(id string, pool workqueue.Pool, idle *workqueue.IdleGate) runtime.Config {
	cfg := runtime.Config{
		ID: id, Store: c.Store, Queue: c.Stores.Queue, Agents: c.opts.Agents, Model: c.opts.Model,
		Catalog: c.Catalog, Dispatch: c.Dispatch, Delegation: c.Delegator, InjectionCheck: c.opts.InjectionCheck,
		Artifacts: c.Artifacts, Memory: c.Memory, Live: c.opts.Live, LiveEndpoint: c.opts.LiveEndpoint, Logger: c.opts.Logger,
		LeaseTTL: c.opts.LeaseTTL, Heartbeat: c.opts.LeaseTTL / 3, Idle: idle, Pool: pool,
		Meter: c.opts.Meter, Quotas: c.opts.Quotas, Moderator: c.Service.Moderator,
		DrainTimeout: c.opts.DrainTimeout, Faults: c.opts.Injector,
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
		ID: id, Queue: c.Stores.JanitorQueue, Service: c.Service, Store: c.Store, Data: SessionData(c.Stores, workspaces, c.Artifacts),
		Workspaces: workspaces, Inbox: c.Stores.Inbox, Nodes: c.Stores.Dir, Ledger: c.Stores.Ledger,
		Queues: map[string]workqueue.Queue{"sessions": c.Stores.Queue, "sandboxes": c.Stores.SandboxQueue, "janitor": c.Stores.JanitorQueue},
		Index:  c.Stores.Index, Deletions: c.Stores.Deletions, Retention: retention, Clock: c.opts.Clock, Logger: c.opts.Logger,
		LeaseTTL: c.opts.LeaseTTL, Usage: c.Stores.Usage, UsageRetention: usageRetention, Presence: c.Stores.Presence,
	}
}
