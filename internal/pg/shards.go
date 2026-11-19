package pg

// 分片键（ADR-0028）。每张表登记它的数据按哪一类键分片：分片之后，同一个键的数据在同一个分片中，
// 一条 SQL、一个事务只涉及一个键。不按键的访问要遍历所有分片，只允许出现在登记的离线路径中。
// 新增表时必须在这里登记（TestEveryTableHasShardKey 检查）。

// KeyClass 是分片键的类别。
type KeyClass string

const (
	// BySession 的键是 Session ID：Session 的日志，以及只属于一个 Session 的数据。
	BySession KeyClass = "session"
	// ByEndUser 的键是 EndUser，不含业务线：Grant 让一条业务线读取同一 EndUser 在另一条业务线的 Memory，
	// 召回与注销账号都应留在一个分片内。
	ByEndUser KeyClass = "end_user"
	// Global 是数据量与写入量都很小的表，不分片。
	Global KeyClass = "global"
)

// TableKey 是一张表的分片键。
type TableKey struct {
	Class KeyClass
	// Column 是键所在的列；Derived 非空时，键由这一列推导。
	Column  string
	Derived string
	// Offline 是不按键访问这张表的路径：Janitor 的删除与保留策略、按时间的清理、低频的业务线服务端查询。
	// 分片后它们遍历所有分片。
	Offline []string
	// Gaps 是在线路径中只凭对象自身 ID 访问、接口尚未带上键的地方。分片层投入使用之前须补上键参数。
	Gaps []string
	// Locate 是接口已明确保留的单库定位路径；C4 接定位适配，C5 接库，不逐库试探。
	Locate []string
}

// Tables 是全部表的分片键。
var Tables = map[string]TableKey{
	// 每个数据库独立一行，不是集中在库 0 的单元控制数据。
	"shard_identity":     {Class: Global},
	"outbox_stream":      {Class: Global},
	"outbox_checkpoints": {Class: Global},
	"outbox": {Class: BySession, Column: "session_id",
		Offline: []string{"List / Read / Head：业务线服务端按分片读取信封与安全起点", "Maintain：扫描候选后逐 Session 分批清理", "Stats：Janitor 统计保留空间"}},
	"end_user_tiers":    {Class: ByEndUser, Column: "end_user"},
	"end_user_consents": {Class: ByEndUser, Column: "end_user"},
	"events":            {Class: BySession, Column: "session_id"},
	"session_snapshots": {Class: BySession, Column: "session_id"},
	"work_items": {Class: BySession, Column: "session_id",
		Offline: []string{"Claim 在队列中选取任意可认领的项：分片后每个分片一个队列，认领方轮流认领"}},
	"work_dues": {Class: BySession, Column: "session_id",
		Offline: []string{"Promote：只读到期候选，再逐 Session 事务移回工作队列"}},
	"janitor_items": {Class: BySession, Column: "session_id",
		Offline: []string{"Claim（同 work_items）"}},
	"sandbox_items": {Class: BySession, Column: "session_id", Derived: "沙箱 Node ID sbx_<Session ID>",
		Offline: []string{"Claim（同 work_items）"}},
	"sandbox_activity": {Class: BySession, Column: "sandbox_id", Derived: "沙箱 Node ID sbx_<Session ID>",
		Offline: []string{"ClaimIdle：沙箱控制器按最近活跃时间回收空闲沙箱"}},
	"sandbox_ledger": {Class: BySession, Column: "session_id",
		Offline: []string{"Prune：按更新时间清理"}},
	"artifacts": {Class: BySession, Column: "session_id",
		Locate: []string{"Lookup：HTTP 鉴权前按 Object 定位唯一库，再按真实 Meta.SessionID 鉴权；Get / Delete 带 Session"}},
	"presence": {Class: BySession, Column: "session_id",
		Offline: []string{"Prune：按有效期清理"}},
	"presence_versions": {Class: BySession, Column: "session_id"},
	"session_tombstones": {Class: BySession, Column: "session_id",
		Offline: []string{"Pending：Janitor 列出未完成的删除", "Request：按 request_id 汇总删除请求的进度（业务线服务端查询）"}},

	// Node ID 只在 EndUser 内唯一；业务线属于 Scope 授权条件，不参与键。
	"nodes": {Class: ByEndUser, Column: "end_user"},
	"inbox": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"RemoveSession：已知 EndUser 时逐键撤回；索引删除后扫描恢复 EndUser，再逐键事务清除"}},
	"inbox_versions": {Class: ByEndUser, Column: "end_user"},
	"sessions": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"List 不指定 EndUser：业务线服务端列出 Session", "IdleBefore / ClosedBefore / DueBefore / ScheduledAfter：Janitor 执行保留策略与绝对期限"},
		Locate:  []string{"LookupSession：用 shard.SessionIndex 定位唯一库并取真实索引行，归属仍由 EndUser / BusinessLine 校验"}},
	"memories": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"DeleteSession：扫描来源 Session 的真实 EndUser，再逐键删除；Janitor 已知归属时用 DeleteSessionForEndUser"}},
	"memory_grants": {Class: ByEndUser, Column: "end_user"},
	"memory_access": {Class: ByEndUser, Column: "end_user"},
	"usage_entries": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"Report：业务线服务端的用量报表", "Prune：按时间清理", "注销 EndUser 后键改为匿名值 '~'，行和幂等键留在原分片，AnonymizeEntry 用调用方原 EndUser 定位"}},
	"usage_hourly": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"Prune：按时间清理",
			"end_user = '' 的业务线合计行由每个分片各自累计；业务线配额检查（Spent）汇总各分片，在线路径上须缓存"}},
	"push_devices": {Class: ByEndUser, Column: "end_user"},

	"end_user_tombstones": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"PendingEndUsers：Janitor 确认注销的 Session 清理进度", "PruneEndUsers：按宽限期扫描，逐 EndUser 事务清理迟到数据后删除到期屏障"}},

	// 删除请求不含 EndUser（ADR-0015）；进度由 session_tombstones 汇总。
	"deletion_requests": {Class: Global},
}
