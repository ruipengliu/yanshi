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
}

// Tables 是全部表的分片键。
var Tables = map[string]TableKey{
	"events":            {Class: BySession, Column: "session_id"},
	"session_snapshots": {Class: BySession, Column: "session_id"},
	"work_items": {Class: BySession, Column: "session_id",
		Offline: []string{"Claim 在队列中选取任意可认领的项：分片后每个分片一个队列，认领方轮流认领"}},
	"janitor_items": {Class: BySession, Column: "session_id",
		Offline: []string{"Claim（同 work_items）"}},
	"sandbox_items": {Class: BySession, Column: "session_id", Derived: "沙箱 Node ID sbx_<Session ID>",
		Offline: []string{"Claim（同 work_items）"}},
	"sandbox_activity": {Class: BySession, Column: "sandbox_id", Derived: "沙箱 Node ID sbx_<Session ID>",
		Offline: []string{"ClaimIdle：沙箱控制器按最近活跃时间回收空闲沙箱"}},
	"sandbox_ledger": {Class: BySession, Column: "session_id",
		Offline: []string{"Prune：按更新时间清理"},
		Gaps:    []string{"Get / Put 只凭 call_id（nodesdk.Ledger）"}},
	"artifacts": {Class: BySession, Column: "session_id",
		Gaps: []string{"Get / Delete 只凭工件 ID（artifact://<id>）"}},
	"presence": {Class: BySession, Column: "session_id",
		Offline: []string{"Prune：按有效期清理"},
		Gaps:    []string{"Touch 按连接续期，跨该连接订阅的所有 Session"}},
	"presence_versions": {Class: BySession, Column: "session_id"},
	"session_tombstones": {Class: BySession, Column: "session_id",
		Offline: []string{"Pending：Janitor 列出未完成的删除", "Request：按 request_id 汇总删除请求的进度（业务线服务端查询）"}},

	"nodes": {Class: ByEndUser, Column: "end_user",
		Gaps: []string{"Get / SetOffline 与注册时的归属检查只凭 node_id；node_id 现由主键保证全局唯一，分片后改为在 EndUser 内唯一"}},
	"inbox": {Class: ByEndUser, Column: "node_id", Derived: "Node 所属的 EndUser",
		Offline: []string{"RemoveSession：Janitor 按 Session 清除"},
		Gaps:    []string{"Put / Remove / Pending / Wait 只凭 node_id（网关与 Worker 都知道所属 EndUser）"}},
	"inbox_versions": {Class: ByEndUser, Column: "node_id", Derived: "Node 所属的 EndUser",
		Gaps: []string{"同 inbox"}},
	"sessions": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"List 不指定 EndUser：业务线服务端列出 Session", "IdleBefore / ClosedBefore：Janitor 执行保留策略"},
		Gaps:    []string{"Get / Touch / MarkClosed / Delete 只凭 Session ID"}},
	"memories": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"DeleteSession：Janitor 按来源 Session 删除"},
		Gaps:    []string{"Get / Delete 只凭 Memory ID"}},
	"memory_grants": {Class: ByEndUser, Column: "end_user",
		Gaps: []string{"Get / Revoke 只凭 Grant ID"}},
	"memory_access": {Class: ByEndUser, Column: "end_user"},
	"usage_entries": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"Report：业务线服务端的用量报表", "Prune：按时间清理", "注销 EndUser 后键改为匿名值 '~'，行留在原分片"},
		Gaps:    []string{"AnonymizeEntry 只凭用量记录 ID"}},
	"usage_hourly": {Class: ByEndUser, Column: "end_user",
		Offline: []string{"Prune：按时间清理",
			"end_user = '' 的业务线合计行由每个分片各自累计；业务线配额检查（Spent）汇总各分片，在线路径上须缓存"}},
	"push_devices": {Class: ByEndUser, Column: "end_user"},

	// 删除请求不含 EndUser（ADR-0015）；进度由 session_tombstones 汇总。
	"deletion_requests": {Class: Global},
}
