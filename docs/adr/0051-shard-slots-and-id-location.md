---
status: accepted
---

# 固定槽位、裸 ID 定位与停写迁移

> **修订（2026-11-18）：扩容默认整片搬迁。** 上线时把 1024 槽均分到 8 个分片、每片一个独立 PostgreSQL 实例；扩容是把整片搬到新主机（同步备库提升，沿用部署 §10 的切换步骤），槽位表与 `Identity()` 不变，只冻结搬迁中的那一片。按槽位搬行的迁槽工具推迟到三个触发条件之一出现时再做。见 §5.4。

> **修订（2026-11-15）：删除旧格式 ID 的兼容。** 系统上线前删除了无版本 ID 的解析、兼容清单与库 0 回退（`legacy_data_in_shard0`）、`derived` 定位类别和历史命名空间预检（`id-preflight`）。所有 ID 都必须带版本，无版本 ID 与损坏的 ID 一样报错；部署配置只含版本标签与槽位表。§5 已按此改写，"旧数据迁离库 0"的阶段缺口随之消失，[ADR-0057](0057-legacy-shard-migration.md) 不再需要。

[ADR-0028](0028-shard-keys.md) 确定 Session / EndUser 两类键。本决定固定持久 ID、槽位定位与停写约束；实现包含按键多库装配、有界遍历、部分可服务、逐片出站消费与分片身份。隔离同步主备演练、容量配方与实测结果见 [部署 §10](../design/deployment.md#10-p4-c6隔离同步主备切换与容量)，最终验收入口见 [一天验收](../design/day-acceptance.md#p4-关闭前的最终回归)。P4 的完整回归已于 2026-10-30 通过。

## 1. 固定槽位与部署配置

- 固定 **1024** 个槽位，编号 `0..1023`。键的原始 UTF-8 字节经 SHA-256，取前八字节按无符号大端整数解释，再 `% 1024`；不做 Unicode 归一化、大小写转换或业务线拼接。实现为 `internal/shard.Hash`。槽位数和算法属于持久格式，不能作为启动参数修改。
- 两类键共用同一槽位表，但键类别仍由存储契约区分；不同键哈希碰撞是正常的，同槽不意味着同一归属或授权。Session 的具体取槽规则见 §3，不能对带槽位的完整 ID 再调用 Hash。
- 槽位到分片是静态部署表。分片号是非负稳定编号，不是 DSN 数组下标；必须登记控制库 `0`。每个槽位恰好赋值一次，拒绝遗漏、重叠、越界与未登记的分片；没有配置的零值表不能用于定位。
- 不能用 `hash % 库数`：库数变化会重映射大量没有搬迁的键。同样不能使用 Go 进程随机种子的哈希。扩容迁移的是槽位，完成停写搬迁后才切换表，键到槽位不变。

`shard.ParseTable` 解析下列 JSON（区间首尾均包含），拒绝未知字段、非法值与尾随 JSON；`NewTable` 复制为不可变的表。纯定位配置不打开数据库；`app.ParseShardConfig` 在同一制品中补 `databases`（id、sql_dsn、direct_dsn），serve/migrate 校验每库物理身份、迁移与 Notifier，格式见 deployment.md。

```json
{
  "shards": [0, 1],
  "ranges": [
    {"first": 0, "last": 511, "shard": 0},
    {"first": 512, "last": 1023, "shard": 1}
  ]
}
```

单库配置为全部槽位到库 0。所有进程须使用同一份表；停写切换后不允许旧表写入者继续运行。`notify-shards` 仅控制通知频道，与数据库槽位表无关。

## 2. 键类别与对象归属

| 真实键 | 对象 / 表 |
|---|---|
| Session | Event、投影快照、工作项与远期工作项、Janitor 工作项、工件元数据、Presence 与版本、Session 删除记录、出站信封；Sandbox 队列、活跃时间与账本（由 `sbx_<Session ID>` 取出 Session） |
| EndUser（不含 BusinessLine） | Node、Inbox 与版本、Session 索引、Memory、Grant、Memory 访问记录、用量、档位、推送设备、EndUser 删除屏障 |
| 单元控制数据 | 不含 EndUser 的 `deletion_requests` 固定在库 0；其进度跨分片汇总 |
| 分片内控制数据 | `outbox_stream`、`outbox_checkpoints` 各库独立维护本分片的 epoch、水位与保留地板；`pg.Tables` 中的 Global 在此表示没有 Session / EndUser 键，不表示所有分片共享一个出站水位 |

同一 Session 的工件、调用、Presence 和沙箱账本继承 Session 槽位，不能拿各自随机 token 或 `call_id` 另选槽。调用的日志与沙箱账本在 Session 分片，设备 Inbox 中的投递副本随接收 Node 的 EndUser；两步可重复写入，不制造跨库事务。用量即使来自 Session 或 Call，也随 EndUser，且不把 Session ID 写入用量记录。

同一 EndUser 的跨业务线 Memory / Grant 在同一库。这在一个单元内成立；单元是一组共享 EndUser 身份与 Memory 的业务线，默认所有业务线在同一个单元（[部署设计](../design/deployment.md) §1，2026-11-14 补充）。业务线仍是隔离与授权条件，但不是内核哈希键；Persona 的 `(BusinessLine, EndUser)` 键属于业务侧，与本 ADR 不混用。

## 3. 新 ID 格式与 Session 索引

槽位用固定三位小写 base32 整数编码，字母表为 `0123456789abcdefghijklmnopqrstuv`，高位补零，不是字节流的 base32。合法范围 `000..0vv`；`100` 表示 1024，拒绝。`v1` 是格式版本。

| 对象 | 格式 | 槽位来源 |
|---|---|---|
| Session | `ses_v1_<sss>_<uuu>_<token>` | `sss = Hash("ses_" + token)`；`uuu = Hash(EndUser)` |
| 工件、平台生成的调用等 Session 对象 | `<prefix>_v1_<sss>_<token>` | 读所属 Session 的 `sss`，不哈希对象 token |
| Memory、Grant、用量等 EndUser 对象 | `<prefix>_v1_<uuu>_<token>` | `Hash(EndUser)`，不从来源 Session / call_id 取槽 |

对象 prefix 只含小写英文字母，`ses` 专用于两槽格式；现有前缀 `art`、`mem`、`grant` 保留，用量可用 `use`。token 非空，仅含 `[A-Za-z0-9_-]`；生产随机 token 保留原来完整的 128 位（32 个小写十六进制字符），确定性生成器保留 `prefix-N`。生产随机 Session 长 47、工件长 43。外部派生值若不符合 token 字符集，须先用已有或明确的新确定性摘要方案编码，不能截断外部 ID。token 的唯一性 / 幂等范围仍由调用方保证，槽位不能替代命名空间；Memory 派生须包含真实 EndUser、业务线与调用的完整身份，覆盖不同 EndUser 同槽且 call_id 相同的情况。

`ids.Generator` 仍是 `func() string`：`Random`、`Sequential` 不变；`Session(endUser)`、`ForSession(prefix, sessionID)`、`ForEndUser(prefix, endUser)`，每次只消耗一个原始 ID。`SessionID(token, endUser)`、`WithSession(prefix, token, sessionID)`、`WithEndUser(prefix, token, endUser)` 支持幂等派生，不消费生成器。错误不重试、不额外抽随机数。

`SlotOf` 返回 `(slot, error)`；`SessionSlotOf` 与 `EndUserSlotOfSession` 分别取得 Session 数据和索引的槽位。Session 专用解析拒绝其他对象的格式。没有版本标记、未知版本、非规范槽位、缺段、非法 token 一律报错（2026-11-15 起无版本 ID 不再按旧 ID 处理）。解析只用于定位，不是身份验证器。

**选择在 Session ID 中同时携带 EndUser 槽位。** `sessions` 表仍 ByEndUser；`Get / Touch / SetNextDue / MarkClosed / Delete` 只有 Session ID 时用 `uuu` 定位，日志、队列、快照等用 `sss`。代价是 Session ID 多四个字符、归属 EndUser 不能被任意修改；收益是不增加需要一致性维护与删除登记的辅助索引，也不增加每次访问的定位查询。

槽位只能定位到库，不能还原真实 EndUser。索引接口仍须在调用方已知时传入 EndUser；只有裸 ID 的路径先在唯一目标库取真实索引行，再以真实 EndUser 做等值访问与归属检查，不能把 `uuu` 当真实键或权限。新建 Session 时须校验 `uuu == Hash(真实 EndUser)`；委托派生必须拿到相同的真实 EndUser，子 Session 自身仍独立选槽，不继承父 Session 数据槽。

## 4. 外部 ID 与授权

以下入口必须补真实键，不能用生成器或对外部 ID 的格式猜测代替：

| 入口 | 必须携带 / 恢复的真实键 |
|---|---|
| Node 注册归属检查、Directory.Get / SetOffline、心跳与断开 | 经认证的 EndUser；Node ID 在 EndUser 内唯一，注册仍核对完整 Scope |
| Inbox.Put / Remove / Pending / Wait 及版本查询，网关投递与 Worker 撤回 | EndUser + Node ID；结果仍以所属 Inbox 中的 Session / Run / call_id 回写 |
| `call_id`；Sandbox Ledger.Get / Put、重复调用结果 | 所属 Session；不能按 call_id 哈希或相信其外形（模型给的 ID 由 Worker 改写为平台 ID，设备侧仍只把它当去重键） |
| Presence.Touch / 连接退出 | 连接的已认证订阅 Session 集合，逐 Session 续期或删除；不能按 Connection ID 选一个库 |
| EndUser 的用量、档位、Node、Grant、删除屏障、推送登记 | 原始 EndUser；不拼 BusinessLine，也不解析 EndUser ID 的字符串外形 |
| 内部 `CreateRequest.SessionID`（委托派生） | 真实 EndUser 和父 Session / call_id；校验带版本的格式与 `uuu == Hash(EndUser)` |
| 客户端 input_id、card_id、动作 ID、外部 Schedule ID | 已授权 Session 内的去重或定位，不能作为独立分片键 |

在线裸工件、Memory、Grant、用量 ID 只能选一个明确目标；禁止逐库试探。读出后仍核对元数据的 Session、EndUser、BusinessLine 或 Grant 条件。伪造、篡改、碰撞的槽位最多改变查询地点，不能改变授权；不存在与不属于请求方走既有拒绝路径。工件输入仍由 `service.normalizeMedia` 比较真实 `Meta.SessionID`，HTTP 下载仍按真实 Session 鉴权。

## 5. 定位器、部署配置与停写迁移

**旧格式 ID 的兼容已删除（2026-11-15）。** 系统上线前没有需要保留的旧数据，删除无版本 ID 的解析、兼容清单与库 0 回退后，"旧数据迁离库 0"不再是缺口（原为阶段缺口，见 [ADR 评审](../review/2026-11-14-adr-review.md) §2.7）。任何单元都可以把槽位分到多个库；迁移只剩 §5.4 的停写迁槽。

### 5.1 唯一目标

`shard.Locator.Locate(Target{Kind, Key})` 是纯函数，返回唯一稳定分片号或错误；零值不可用，不持有连接，不探测数据库。`Kind` 必须明确：

| Kind | Key | 定位 |
|---|---|---|
| `session_data` | Session ID | `sss` 查槽位表 |
| `session_index` | Session ID | `uuu` 查槽位表 |
| `end_user` | 原始真实 EndUser | 以 Hash 查槽位表 |
| `object` | 工件、调用、Memory、Grant、用量 ID | 内嵌所属键槽查槽位表 |

无版本、损坏、未知版本的 ID 报错，不回退任何库。`ids` 保留生成与解析 API，解析实现统一在 `shard`，避免定位器与生成器互相依赖或维护两套格式。在线选择器必须用 Locator。定位只决定去哪查；同槽不证明 Session、EndUser、BusinessLine、Grant 归属。

### 5.2 EndUser

EndUser 由业务线指定，字符串没有版本，一律按 Hash 定位，包括看似带版本的 EndUser。`DefaultLocator()` 为全部槽到库 0，供单库默认装配。库 0 同时是控制库：不含 EndUser 的单元控制数据固定在库 0（§2），所以槽位表必须登记库 0。

### 5.3 部署配置

部署配置是只读的 JSON 制品，由版本标签与槽位表组成；`ParseDeployment` 载入，`app.ParseShardConfig` 另加各库的 DSN：

```json
{
  "version": "cell-2026-11",
  "table": {
    "shards": [0, 1],
    "ranges": [{"first": 0, "last": 511, "shard": 0}, {"first": 512, "last": 1023, "shard": 1}]
  }
}
```

解析拒绝未知字段（包括已删除的 `compatibility`）、重复 JSON 字段、缺失版本、无效槽位表与尾随 JSON。`Locator.Identity()` 返回版本与规范化 SHA-256 摘要，摘要覆盖版本、槽位表与登记分片；各进程须用它核对所有进程的一致性，不能只比较版本标签。

### 5.4 扩容路线、停写与幂等

**默认路线：预分片、整片搬迁（2026-11-18 定稿）。**

- **上线即分 8 片。** 生产单元在上线时把 1024 槽均分到 8 个分片（每片 128 槽），每片是一个独立的 PostgreSQL 实例（主 + 同步备），初期可以多片共用一台主机。开发、模拟与测试仍默认单库，另有两库测试与 8 片扇出测试（`TestFanoutEightShardsSpansTwoWaves`）。8 是"不需要迁槽工具时一个单元的最大扩展倍数"；代价随片数线性增长：每进程每片一条 LISTEN 与一个 SQL 池、xinli 每业务线每片一条长轮询、离线扇出在并发上限 4 下分两波、就绪探针逐片探测。
- **扩容 = 整片搬迁。** 在新主机上为该片建物理流复制备库，切到同步后按[部署 §10](../design/deployment.md#10-p4-c6隔离同步主备切换与容量) 的步骤关闭该片准入、提升、隔离旧主、改指端点。物理复制保留 `shard_identity`、出站 epoch、游标、地板与序列（出站游标基于 `xid8`，逻辑复制或导出导入会换掉事务号空间、丢掉序列，所以不用），不旋转 epoch，不需要新代码。槽位表与 `Identity()` 不变，只换该片的 DSN。
- **只冻结搬迁中的那一片。** 其余分片照常读写；依赖该片的请求按 §7 有界返回 503。下文"停入口及全部写入者"只适用于改变槽位表的迁槽。
- **停写预算。** 一次停写（从关闭准入到恢复准入；整片搬迁时是该片的不可用时间）预算 15 分钟，在目标环境用未来 12 个月预期的最大数据量演练实测，记入[上线检查清单](../launch-checklist.md)。超出预算即触发重审条件，考虑在线追平或更细的预分片。（2026-11-21：整片搬迁的步骤、回滚与报告见[部署 §10.5](../design/deployment.md#105-整片搬迁扩容)；`make shard-move` 在 Mac mini 上 1GB 与 10GB 的停写窗口为 0.4～0.6 s，不随数据量增长，目标环境仍待实测。）
- **迁槽工具推迟。** 按槽位搬行（逐表按 `sss` / `uuu` / EndUser 哈希选行、复制、校验）在以下任一情况出现时才做：8 片都已独占实例仍不够；出现需要拆分的热点槽位；按业务线拆分单元。
- **迁移制品。** 迁移或搬迁报告只含各表各片（或各槽）的行数与内容摘要、源与目标分片号、前后的 `Identity()` 摘要，以及失败对象的 ID（Session、工件、Memory 等）；不含原始 EndUser（业务线给的 EndUser 是个人数据，EndUser 键的行用对象 ID 指代）。备份是个人数据，回滚窗口（7 天）结束即删，不作恢复数据的来源；在回滚窗口内若用备份回滚，须重放窗口期间的删除记录（ADR-0015 的"先写后查"）。

**迁槽（改变槽位表）的停写步骤。** 仅支持新建单元或停写迁移：盘点并备份 → 停入口及全部写入者（Worker、控制器、Janitor）→ 等出站流消费方追平 → 迁移并校验数量、归属、幂等身份、删除屏障及新写入路由，删除源分片上被迁槽位的出站行 → 部署同一映射与格式版本 → 受影响分片的出站流换代 → 验证后恢复。出站游标不映射、不搬行，见 [ADR-0046](0046-business-line-outbox.md)“迁槽与游标”（2026-11-22）。只停止 HTTP 不算停写；回滚同样停写并恢复数据与映射的一致版本。派生的 Memory 和委托 Session 的重试仍须命中原对象，不能只替换字符串创建第二份。

用量匿名化为 `~` 后留原分片，不按 `~` 重哈希。`usage_entries.idempotency_key` 的完整原键与匿名行共同保留原位置；同一原键仍是全局去重身份，不放宽成每用户一笔费用。新写入跟随真实键目标，不独立选库。

### 5.5 历史命名空间预检（已删除）

原先的 `id-preflight` 命令扫描历史快照中与新格式碰撞的旧 ID，用于登记 `historical: true` 的兼容条目。兼容清单删除后它没有用途，于 2026-11-15 一并删除。

不做在线迁槽、自动再平衡、跨地域双活或网关通知归属。扩容的默认做法是 §5.4 的整片搬迁。新格式上线后降级到旧定位代码不是安全的滚动回退方案。

## 6. `pg.Tables.Offline` 的逐项安排

所有 fan-out 有并发上限、每库超时、总预算和续扫位置；分片失败不能阻止健康分片做独立清理，但依赖全部结果的操作必须显式失败，不把部分结果报成完整结果。扫描先取得键，随后每条事务只处理一个真实键。下表对应 `pg.Tables` 的 Offline 路径。`app.fanout` 统一限制为默认并发 4、每片 1 秒、整轮 4 秒，分片号按配置顺序合并，轮次起点公平轮转。`ShardOptions.Fanout` 可覆盖这些值；模拟的 `DeterministicQueues` 使用原顺序、不启动 goroutine、不以墙钟截止。

只有连接故障、分片不可用和截止时限耗尽归为 `shard_unavailable`。存储约束、事务归属与清理回调的业务错误保留错误链，调用方仍可用 `errors.Is/As` 判断；独立清理即使取得健康片结果也必须返回这些业务错误，不能把它们当成可忽略的断片。调用方主动取消保留取消错误。

| 表与 Offline 路径 | 类别 | 分片后的执行 |
|---|---|---|
| outbox.List / Read / Head | List 完整；Read/Head 单片 | 按显式分片读各自信封与安全起点，游标绑定分片 / epoch；List 的完整分片枚举有界汇总，失败不隐藏分片 |
| outbox.Maintain | 独立清理 | 每库扫描候选、逐 Session 分批清理；删除信封按该库地板处理 |
| Queue.Depths | 需要全部结果 | 各库队列深度按工作类别汇总，缺片不报告完整积压 |
| outbox.Stats | 需要全部结果 | 各库统计后汇总；缺一库不报告完整空间用量 |
| work_items.Claim | 独立清理 | 公平轮转逐分片认领，租约与后续操作回原分片；坏库超时后让其他库继续 |
| work_dues.Promote | 独立清理 | 逐库读到期候选，逐 Session 事务移回本库队列；共享总预算并公平推进 |
| janitor_items.Claim | 独立清理 | 与工作队列一样逐分片认领、续租、完成 |
| sandbox_items.Claim | 独立清理 | 与工作队列一样逐分片认领；由沙箱 Node ID 还原 Session |
| sandbox_activity.ClaimIdle | 独立清理 | 每库按时间认领空闲候选，在所属 Session 的分片完成回收 |
| sandbox_ledger.Prune | 独立清理 | 逐库有界扫描更新时间，按 Session 分批清理 |
| presence.Prune | 独立清理 | 逐库扫描过期项，逐 Session 清理并更新本库版本 |
| session_tombstones.Pending | 独立清理 | 各库独立列出未完成删除并入本库 Janitor 队列，可重复执行 |
| session_tombstones.Request | 需要全部结果 | 先在库 0 读取并授权删除请求，再到每库按 request_id 统计，累计 Sessions / Completed；去掉与 deletion_requests 的跨库 JOIN，缺库时报不可用，不报告完成 |
| inbox.RemoveSession | 需要全部结果 | 已知 EndUser 时只清该用户；归属索引已删时离线扫描恢复 EndUser、逐键事务删除，全部成功才完成清理。逐库扫描有界 |
| sessions.List（未指定 EndUser） | 需要全部结果 | 低频业务线服务端查询，各库取有界分页，以稳定排序键合并；分页位置保留各库进度，缺库不返回完整列表 |
| sessions.IdleBefore / ClosedBefore / DueBefore / ScheduledAfter | 独立清理 | 每库扫描保留策略或绝对期限候选；按真实 EndUser 更新索引，按 Session 槽执行关闭 / 删除 / 定时，多步可重复 |
| memories.DeleteSession | 需要全部结果 | 逐 EndUser 分片查来源 Session，再按真实 EndUser 删除；所有分片完成后才确认该清理步骤 |
| usage_entries.Report | 需要全部结果 | 各库按业务线 / 时间返回部分统计或分页，完整合并后返回；缺库不补零 |
| usage_entries.Prune | 独立清理 | 逐库按时间扫描、按真实键有界清理；匿名行留原分片 |
| usage_entries 匿名化后的 `~` | 独立清理 | 不搬库；按原 EndUser 在原库匿名化，随后按 ID 定位或逐库离线清理；不能再用匿名值求槽 |
| usage_hourly.Prune | 独立清理 | 各库有界清理历史聚合行 |
| usage_hourly 的 `end_user = ''` 合计 / Spent | 需要全部结果 | 每库保留业务线部分和，汇总完整部分和后缓存供在线配额判断；不把全部业务线写入一个热点库 |
| end_user_tombstones.PendingEndUsers | 独立清理 | 各 EndUser 分片认领注销确认工作；其关联 Session 删除状态按 Session 分片检查，缺库就保持未完成 |
| end_user_tombstones.PruneEndUsers | 独立清理 | 各库扫描宽限期候选，在 EndUser 所属库事务清除迟到数据并删屏障；需要的跨库 Session 清理未确认时不删屏障 |

业务线配额是软限制（ADR-0019）：缓存键包含业务线、用户类别、整点统计窗口与 `Identity()` 的版本和摘要；项保存完整分片集合、扫描开始时间及到期时间。默认有效期 15 秒，从扫描开始计时，最多 1024 项；映射切换或重装 router 后失效。有效期内直接复用完整和，不随新计量延长有效期；这增加了至多一个有效期内新增费用的软上限超出窗口。只发布所有相关分片成功的部分和。分片失联时可用仍在有效期内的完整缓存；没有有效完整缓存则有界失败，不能用缺失用量当零放行。EndUser 配额在其所属库读取，不为查询本用户用量扫描其他库。

`pg.WithTx` 依 pool 身份复用事务，不是跨库事务管理器。Log / Queue / Waking 必须选同一 Session 的同一库；EndUser 屏障与该键的迟到数据清理仍须同库。跨键与跨库的多步操作保留幂等和恢复进度。

## 7. 部分可服务

SQL 池先构造、再有界探测，`-migrate=false` 允许断片时冷启动；可达库的身份或迁移配置错误仍拒绝启动，暂时不可达的库在恢复时先校验身份与迁移再开放访问。`-migrate=true` 与独立 migrate 任务要求全部迁移成功。

按 Q118，健康分片的读取、写入、认领和出站消费继续。仅依赖失联分片的请求有界失败；汇总不得把部分成功当作完整成功。就绪检查报告每个分片的 SQL、迁移与 LISTEN 状态，结合进程角色和可服务分片决定是否接受流量，不能简单对所有分片取 AND；请求处理仍按其实际依赖拒绝不可用目标。控制库 0 失联可阻塞删除请求查询等依赖它的操作，但不能因此关闭不依赖它的健康 Session。gateway 要求至少一片 SQL、身份、迁移和 LISTEN 均正常；worker、sandbox、janitor 要求至少一片 SQL、身份和迁移正常，通知断开时由轮询兜底。组合角色采用其中最严格的 gateway 规则。探针与短操作各有预算；目标不可用返回 503 和 Retry-After。

索引保留策略、定时索引与待删除候选使用每路径/每片的循环游标，扫描到底回到开头；失败片保留原位置。每库返回的整页全部交给调用者，不在合并后截掉已推进游标的候选。游标只缓存位置，重启从头重扫；完成清理、提升以及出站地板保存在原库。队列认领每次只取一片，持有来源租约；ClaimIdle 已取走的各片候选全部交回。完整性查询与独立工作分开：后者可使用健康片候选，多片 Janitor 在阶段或单项失败后仍处理其他工作，最后返回累计错误。Inbox/Memory 的跨库清理只有全部成功才确认该删除步骤。

## 8. 当前状态与迁移约束

> 本 ADR 的内容分三层（2026-11-14 补充）：§1–§4 与 §9 是**持久格式与保证**（固定 ID 格式与槽位算法、真实归属校验、唯一定位与事务保证），推翻代价最高；§5 是**定位与对未来迁移的约束**（"旧数据迁离库 0"的阶段缺口已随旧格式 ID 兼容的删除消失，2026-11-15）；本节列出**已经实现与已有测试入口的能力**，它们以完整 check / sim 与部署验收为证据，不能由接口存在推定通过。

Session、工件、Memory、Grant、用量等原本全局唯一的 ID 保持原契约，不能放宽为 `(EndUser, ID)` 可重复存两行；真实键用于定位和等值访问，不改变幂等身份。

- 所有顶层 Session 只经 service.Create，HTTP/Connection 不开放指定 ID；内部重试核对业务线、真实 EndUser、父调用，委托由父 Session/call_id 摘要派生，子 Session 独立选槽。
- Worker 改写模型 call_id 为 ForSession；Run、Schedule、Event 在已知 Session 下定位，今后若独立寻址也须带 Session 槽。Connection/进程 ID 不作为持久分片键。
- Presence 只续期已认证的 Session，连接退出逐 Session Remove；Sandbox Ledger 以 Session/call_id 定位，核对 sbx_ 与 Invoke 归属，started 先落盘，Prune 先扫描真实 Session 再逐键复验删除，Forget 及模拟删除检查保持。投影消失不计沙箱费用，陈旧队列释放后交 Janitor。
- Node/Inbox 按真实 EndUser+Node ID 定位，同用户 Node 不可改业务线，跨用户同名 Node 的代数独立；以认证 Conn.EndUser 而非 Hello 自报值授权。通知键用 `len(EndUser):EndUserNodeID:`（UTF-8 字节数及尾冒号），不将通知频道数当数据库槽位数。
- @user 不经目录/Inbox，@agent/ 直接委托，sbx_ 不登记目录但经 Inbox；设备 forget 跳过三种伪 Node，HostApp 切账号负责隔离和清理本地数据。
- Session 索引、工件、Memory/Grant、用量等在线访问均携带真实键；裸查只在唯一定位目标中恢复真实归属再鉴权，不逐库试探。工件在读内容前核对 Meta.SessionID；Janitor 先删 Blob 再删元数据，两遍清扫和写后复查保持。Memory 幂等命中（调用派生或相似度）均核对完整归属，仅 ErrNotFound 继续；派生摘要包含 EndUser、业务线、来源 Session 和完整 CallID 的逐字段十进制字节长度前缀。Memory 保留 Put 覆盖、相似度、历史失效、审批与召回规则；RecordAccess 同时核对 EndUser/Owner，保留单次召回批写。Grant 撤销核对 From/To，创建核对双方删除屏障。
- 计量对完整原幂等键作 SHA-256 后 WithEndUser(use)，不截断含斜杠的键、不继承 Session 槽、不额外消耗 ID；ID 或原键冲突不再次累计，新行才更新小时聚合，匿名化及写后复查保持。
- `pg.Tables.Gaps` 为空，裸定位只登记 artifacts.Lookup、sessions.LookupSession。所有 PG 存储经 PoolSelector；普通单库默认 Locator，多库由 app 包装，每库 PostgresShard 的 Select 只断言本分片；直接构造的 PG 存储和内存同样遵守默认定位。核心对象图只由 app.Stores/app.NewCore 装配，不各自布线。
- 1024 槽格式、两真实 database 的存储一致性与差分、逐库删除不变量、部分可服务、独立出站消费已有测试入口；完整 check/sim 和部署 §10 的同步提升、容量、最终一天与 eval 报告是验收证据，不能由接口存在推定通过。

必要迁移注意事项：

- **0032–0033（账本）**：并发唯一索引后切换 `(session_id, call_id)` 主键并令 Session 非空。历史缺失 Session 时失败并保留原记录，不猜归属、不删 started；旧控制器的 ON CONFLICT(call_id) 不兼容，须停写同步切换。
- **0034–0038（Node / Inbox）**：从目录与 Session 索引回填真实 EndUser；冲突、多归属、无法恢复的在途调用失败回滚，不清空调用。仅没有投递且归属已删的版本缓存可在停写时删除重建。三张表并发建索引后切换复合主键，不重写 ID、代数、调用与顺序；回填留足窗口、核对数量，禁止新旧 ON CONFLICT 混跑。
- **0039–0040（用量）**：原 ID 回填非空 idempotency_key，再独立单语句、可重复并发唯一索引；不改历史 ID、金额、聚合或匿名归属。索引完成后才能恢复新写入者，旧写入者缺非空列不得混跑；旧 / 新 / 匿名重复顺序均须保持去重。
- **删除恢复**：Janitor 先用投影 EndUser，投影已删则 LookupSession；清日志与快照后第二遍清扫，再删索引。两者皆无时 Inbox 离线恢复真实 EndUser、逐用户事务，全部成功才完成；Session tombstone 不新增 EndUser。Memory 保留既有离线 DeleteSession，已知归属优先 DeleteSessionForEndUser。不能通过改变删除顺序来规避定位。
- **协议与长度**：proto 均为 string；随机 Session 47，sbx_ 后 51，@agent/ 后 54，Docker 名称 62 / 61；工件 43、调用 44，满足 ask_user 64 与 fsblob / FileLedger 128。S3 key 不拼 Session；Persona Session 为 bytea、xinli 为 text / JSONB，无需定长迁移；网页展示截尾不影响完整 ID。修改协议运行 gen；修改移动契约运行 mobile；格式生成或模型可见行为变化按项目规则评测。


## 9. 同步提升与身份连续性

每个分片的 `public.shard_identity` 保存随机 instance_id 和稳定 shard_id。只由迁移在空表初始化，serve 只读核对；SQL 与直连入口须同身份、同业务 schema，分片间 instance_id 不得重复。固定 public 防止多个 schema 假扮独立 database。恢复目标再次核对迁移、身份；主备物理复制保留该行。

已确认写入的无损切换要求同步备库持久化提交 WAL。唯一应用代理先关闭准入及存量连接，提升同步备库、隔离旧主并核实，最后固定端点改指新主；不允许靠旧主恢复可达而自动回选。失败保持入口关闭，生产 HA 控制器必须有真实 fencing。同步提升保留出站 epoch/key/水位；备份恢复或连续性不可证明时，在开放入口前显式旋转 epoch，不能借此放宽已确认写入不丢的要求。切换后的单主降级必须明确，在恢复同步冗余前不再承诺容忍下一次主库失效。

## 重审条件

（2026-11-14 补充）与 [ADR-0028](0028-shard-keys.md) 的重审条件相同：出现需要原子地跨两个 EndUser 的产品需求；停写迁移槽位或整片搬迁所需的时间超出停机预算（15 分钟，§5.4）；8 个预分片都已独占实例仍不够；出现热点槽位；按业务线拆分单元，使需要共享 Memory / Grant 的业务线分属不同单元。（"上线前评估是否删除旧格式 ID 兼容"一条已于 2026-11-15 完成：已删除。）
