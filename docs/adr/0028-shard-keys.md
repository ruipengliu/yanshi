---
status: accepted
---

# 数据按 Session 或 EndUser 分片

单个 PostgreSQL 主库是写入上限（ADR-0007）。容量估算见[延展性评审](../review/2026-10-11-scalability-review.md)：亿级月活时峰值约每秒 3 千个 Run、4.5 万条 Event，超出单库能力。扩容的办法是把数据按键分到多个库。分片层可以以后再做，但从现在起，表结构与访问方式就得守住键的边界；否则下一张新表就可能把两类键混在一起，到分片时拆不开。

## 决定

**两类键，外加不分片的全局表。** 每张表登记在 `pg.Tables`（`internal/pg/shards.go`）中，`TestEveryTableHasShardKey` 检查迁移中的每张表都已登记，且键所在的列存在。

| 键 | 表 |
|---|---|
| Session（`session_id`） | Event 日志、投影快照、工作队列与 Janitor 队列、工件元数据、Presence、删除记录；沙箱的队列、账本与活跃时间（键由沙箱 Node ID `sbx_<Session ID>` 推导） |
| EndUser（`end_user`） | Node 目录与 Inbox（键是 Node 的所属 EndUser）、Session 索引、Memory、Grant 与访问记录、用量、推送设备 |
| 全局 | 删除请求（不含 EndUser，ADR-0015） |

- EndUser 键不含业务线。Grant 让一条业务线读取同一 EndUser 在另一条业务线的 Memory（ADR-0003）；按 `(业务线, EndUser)` 分片，召回与注销账号就要跨分片。
- 键经哈希映射到固定数量的槽位，槽位再映射到分片。扩容迁移的是槽位，不改变任何键的哈希。

**访问规则：**

1. 在线路径按键访问表：查询条件中包含键的等值条件。
2. 不按键的访问（按其他列查询、按时间扫描、队列认领）分片后要遍历所有分片，只允许出现在三类路径中：Janitor 的删除与保留策略、按时间的清理、低频的业务线服务端查询。每张表的这类路径登记在 `Offline` 中。
3. 一条 SQL、一个事务只涉及一个键。跨键的效果靠可重复的多步写入完成，例如先写日志再写 Inbox（ADR-0004）；目前已经如此。
4. 新的存储接口在参数中带上键。目前在线路径中只凭对象自身 ID 访问的地方登记在 `Gaps` 中，分片层投入使用之前须补上键参数：工件、Memory、Grant 按 ID 读取；Inbox、Node 按 Node ID 读取；Session 索引按 Session ID 更新；Presence 按连接续期。
5. 通知频道按分片命名（`pg.Notifier.Channel`）：频道名由主题与键的哈希分片决定，进程只 LISTEN 有订阅者的频道，每条通知只送到关心它的进程。分片数（`serve -notify-shards`）为 1 时频道名就是主题名。

## Considered Options

- **全部按 Session 分片**：负载最均匀，但列出用户的 Session、召回 Memory、注销账号都要遍历所有分片。
- **全部按 EndUser 分片**：同一用户的数据都在一起，派发调用甚至可以与写日志在同一个事务里完成。代价有两处：只带 Session ID 的请求（服务令牌、Janitor、进程间拉取增量）要先查 Session 属于谁，或者让 Session ID 编码槽位，改变 ID 格式；Event 日志是写入量最大的表，按用户聚集后，重度用户所在的分片更热。
- **分布式数据库（PG 协议兼容）**：可以替代自建分片层，但私有化部署（ADR-0001）的运维门槛高。本 ADR 的规则同样适用：按键访问是这类数据库高效的前提。

## Consequences

- 现在不分片，这里只约束表结构与访问方式。实现分片层时，`eventlog.Log`、`workqueue.Queue` 等接口不变，按键路由到各分片，并通过一致性套件与差分模拟测试。
- 队列认领在每个分片内进行：分片后每个分片有自己的队列，认领方轮流认领。
- 业务线级的汇总变成各分片的部分和：用量的业务线合计行由每个分片各自累计，业务线配额检查汇总各分片，在线路径上须缓存（配额是软限制，ADR-0019）。
- `node_id` 目前由主键保证全局唯一；分片后改为在 EndUser 内唯一，Inbox 与路由按 `(EndUser, Node ID)` 寻址。
- 新增表时，先在 `pg.Tables` 登记键与离线路径；需要跨键查询时说明属于哪一类离线路径。
