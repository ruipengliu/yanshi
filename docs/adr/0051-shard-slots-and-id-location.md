---
status: accepted
---

# 固定槽位、裸 ID 定位与停写迁移

[ADR-0028](0028-shard-keys.md) 已确定 Session / EndUser 两类键。[Persona 架构](../design/persona-architecture.md) §13 Q109、Q110、Q118 与 §11 Q124 的 C1 要求真实多库之前先固定裸 ID 定位与兼容规则。本 ADR 确定格式、定位与迁移规则。C1–C4 已具备带真实键的接口、定位器、只读兼容映射与历史预检；装配层现已提供每库连接/通知/迁移、按键路由、有界 Offline fan-out、完整用量缓存、部分就绪与多分片出站消费；验收仍以远端 `make check`、`make sim` 为准。

## 1. 固定槽位与部署配置

- 固定 **1024** 个槽位，编号 `0..1023`。键的原始 UTF-8 字节经 SHA-256，取前八字节按无符号大端整数解释，再 `% 1024`；不做 Unicode 归一化、大小写转换或业务线拼接。实现为 `internal/shard.Hash`。槽位数和算法属于持久格式，不能作为启动参数修改。
- 两类键共用同一槽位表，但键类别仍由存储契约区分；不同键哈希碰撞是正常的，同槽不意味着同一归属或授权。Session 的具体取槽规则见 §3，不能对带槽位的完整 ID 再调用 Hash。
- 槽位到分片是静态部署表。分片号是非负稳定编号，不是 DSN 数组下标；必须登记兼容库 `0`。每个槽位恰好赋值一次，拒绝遗漏、重叠、越界与未登记的分片；没有配置的零值表不能用于定位。
- 不能用 `hash % 库数`：库数变化会重映射大量没有搬迁的键。同样不能使用 Go 进程随机种子的哈希。扩容迁移的是槽位，完成停写搬迁后才切换表，键到槽位不变。

`shard.ParseTable` 解析下列 JSON（区间首尾均包含），拒绝未知字段、非法值与尾随 JSON；`NewTable` 复制为不可变的表。纯定位配置不打开数据库；C5a 由 `app.ParseShardConfig` 在同一制品中补 `databases`（id、sql_dsn、direct_dsn），serve/migrate 校验每库物理身份、迁移与 Notifier，格式见 deployment.md。

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

同一 EndUser 的跨业务线 Memory / Grant 在同一库。业务线仍是隔离与授权条件，但不是内核哈希键；Persona 的 `(BusinessLine, EndUser)` 键属于业务侧，与本 ADR 不混用。

## 3. 新 ID 格式与 Session 索引

槽位用固定三位小写 base32 整数编码，字母表为 `0123456789abcdefghijklmnopqrstuv`，高位补零，不是字节流的 base32。合法范围 `000..0vv`；`100` 表示 1024，拒绝。`v1` 是格式版本。

| 对象 | 格式 | 槽位来源 |
|---|---|---|
| Session | `ses_v1_<sss>_<uuu>_<token>` | `sss = Hash("ses_" + token)`；`uuu = Hash(EndUser)` |
| 工件、平台生成的调用等 Session 对象 | `<prefix>_v1_<sss>_<token>` | 读所属 Session 的 `sss`，不哈希对象 token |
| Memory、Grant、用量等 EndUser 对象 | `<prefix>_v1_<uuu>_<token>` | `Hash(EndUser)`，不从来源 Session / call_id 取槽 |

对象 prefix 只含小写英文字母，`ses` 专用于两槽格式；现有前缀 `art`、`mem`、`grant` 保留，用量可用 `use`。token 非空，仅含 `[A-Za-z0-9_-]`；生产随机 token 保留原来完整的 128 位（32 个小写十六进制字符），确定性生成器保留 `prefix-N`。生产随机 Session 长 47、工件长 43。外部派生值若不符合 token 字符集，须先用已有或明确的新确定性摘要方案编码，不能截断外部 ID。token 的唯一性 / 幂等范围仍由调用方保证，槽位不能替代命名空间；C3 的 Memory 派生须包含真实 EndUser、业务线与调用的完整身份，覆盖不同 EndUser 同槽且 call_id 相同的情况。

`ids.Generator` 仍是 `func() string`：`Random`、`Sequential` 不变；新增 `Session(endUser)`、`ForSession(prefix, sessionID)`、`ForEndUser(prefix, endUser)`，每次只消耗一个原始 ID。`SessionID(token, endUser)`、`WithSession(prefix, token, sessionID)`、`WithEndUser(prefix, token, endUser)` 支持幂等派生，不消费生成器。错误不重试、不额外抽随机数。

`SlotOf` 返回 `(slot, legacy, error)`；`SessionSlotOf` 与 `EndUserSlotOfSession` 分别取得 Session 数据和索引的槽位。Session 专用解析拒绝其他对象的新格式。第二段为 `v` 加十进制数字时进入版本化命名空间：未知版本、非规范槽位、缺段、非法 token 均报错，不按旧 ID 处理。没有版本标记才是 legacy；这种宽松解析保留已有测试与内部指定的旧 ID，并不是身份验证器。

**选择在 Session ID 中同时携带 EndUser 槽位。** `sessions` 表仍 ByEndUser；`Get / Touch / SetNextDue / MarkClosed / Delete` 只有 Session ID 时用 `uuu` 定位，日志、队列、快照等用 `sss`。旧 Session 的这两种访问分别查兼容映射；未覆盖时仅在明确允许的部署中去兼容库 0。代价是 Session ID 多四个字符、归属 EndUser 不能被任意修改；收益是不增加需要一致性维护与删除登记的辅助索引，也不增加每次访问的定位查询。

槽位只能定位到库，不能还原真实 EndUser。索引接口仍须在调用方已知时传入 EndUser；只有裸 ID 的路径先在唯一目标库取真实索引行，再以真实 EndUser 做等值访问与归属检查，不能把 `uuu` 当真实键或权限。新建 Session 时须校验 `uuu == Hash(真实 EndUser)`；C2 的委托派生必须拿到相同的真实 EndUser，子 Session 自身仍独立选槽，不继承父 Session 数据槽。

## 4. 外部 ID 与授权

以下入口必须补真实键，不能用生成器或对外部 ID 的格式猜测代替：

| 入口 | 必须携带 / 恢复的真实键 |
|---|---|
| Node 注册归属检查、Directory.Get / SetOffline、心跳与断开 | C3a 已补经认证的 EndUser；Node ID 在 EndUser 内唯一，注册仍核对完整 Scope |
| Inbox.Put / Remove / Pending / Wait 及版本查询，网关投递与 Worker 撤回 | C3a 已补 EndUser + Node ID；结果仍以所属 Inbox 中的 Session / Run / call_id 回写 |
| `call_id`；Sandbox Ledger.Get / Put、重复调用结果 | 所属 Session；不能按 call_id 哈希或相信其外形（模型给的 ID 由 Worker 改写为平台 ID，设备侧仍只把它当去重键） |
| Presence.Touch / 连接退出 | 连接的已认证订阅 Session 集合，逐 Session 续期或删除；不能按 Connection ID 选一个库 |
| EndUser 的用量、档位、Node、Grant、删除屏障、推送登记 | 原始 EndUser；不拼 BusinessLine，也不解析 EndUser ID 的字符串外形 |
| 内部 `CreateRequest.SessionID`（委托派生） | 真实 EndUser 和父 Session / call_id；C2 校验新格式并保留旧派生的幂等身份 |
| 客户端 input_id、card_id、动作 ID、外部 Schedule ID | 已授权 Session 内的去重或定位，不能作为独立分片键 |

在线裸工件、Memory、Grant、用量 ID 只能选一个明确目标；禁止逐库试探。读出后仍核对元数据的 Session、EndUser、BusinessLine 或 Grant 条件。伪造、篡改、碰撞的槽位最多改变查询地点，不能改变授权；不存在与不属于请求方走既有拒绝路径。工件输入仍由 `service.normalizeMedia` 比较真实 `Meta.SessionID`，HTTP 下载仍按真实 Session 鉴权。

## 5. 定位器、兼容映射与停写迁移

**P4 交付限制：只支持新建单元，或全部槽位留在库 0 的既有单元。把旧数据迁离库 0 不在 P4 支持范围。** 静态映射无法覆盖旧 Session 之后生成的旧式 ID，恢复这类 Session 的写入需要在线目录；本 ADR 下文的迁移清单是未来迁移的约束，不是 P4 已支持迁出的声明。部署配置拒绝迁出的兼容条目；槽位分到其他库的新单元必须关闭 legacy 回退、使用空兼容清单。


### 5.1 唯一目标

`shard.Locator.Locate(Target{Kind, Key})` 是纯函数，返回唯一稳定分片号或错误；零值不可用，不持有连接，不探测数据库。`Kind` 必须明确：

| Kind | Key | 新格式的定位 |
|---|---|---|
| `session_data` | Session ID | `sss` 查槽位表 |
| `session_index` | Session ID | `uuu` 查槽位表 |
| `end_user` | 原始真实 EndUser | 先查 EndUser 映射，再以 Hash 查槽位表 |
| `object` | 工件、Memory、Grant、用量 ID | 内嵌所属键槽查槽位表 |
| `derived` | 迁移清单独立登记的旧派生调用 / 原幂等键 | 有版本时取内嵌槽；没有版本时查该类别清单 |

无版本标记的 legacy 先查相同 Kind / Key 的权威兼容条目；未覆盖时，只有 `legacy_data_in_shard0: true` 才允许返回库 0。该声明意味着**全部未覆盖历史目标确实仍在库 0**，不是“查不到就试试 0”。库 0 与槽位 0 无关。新格式查槽位表；损坏、未知版本报错，不回退。唯一例外是 §5.5 核实后显式声明的历史命名空间碰撞。

`ids` 保留生成与解析 API，解析实现统一在 `shard`，避免定位器与生成器互相依赖或维护两套格式。底层 `Table.Shard(slot, true)` 仅保留旧 API 的兼容含义；在线选择器必须用 Locator，不能绕过兼容清单与回退开关。定位只决定去哪查；同槽或映射命中都不证明 Session、EndUser、BusinessLine、Grant 归属。

### 5.2 EndUser 与启用前提

EndUser 由业务线指定，字符串没有可判定新旧的版本。其兼容条目优先于 Hash，包括看似版本化 ID 的 EndUser。`ValidateActivation(table, compatibility)`（`NewLocator` 同样调用）拒绝无效表、未登记目标分片；若声明存在未覆盖旧数据且映射为空，要求所有 1024 槽都在库 0，否则拒绝。

非空映射不是完整性证明。迁移必须核实所有历史真实键、裸对象、索引与幂等键的目标一致；尤其恢复写入的 EndUser，其映射位置必须与新生成对象 / Session 索引携带的 Hash 槽位置一致。不能借添加一条无关映射绕过历史用户盘点。新建单元可以显式关闭旧数据回退。`DefaultLocator()` 为全部槽到 0、空映射、`legacy_data_in_shard0: true`，供单库默认装配。

### 5.3 权威、只读的部署清单

`CompatibilityConfig` 的 `version` 必填；`entries` 是 Kind / Key / Shard 条目数组，旧 Session 数据位置与索引位置可以分别登记；`derived` 保存需要独立保留的旧调用或用量原幂等键。`NewCompatibility` 复制条目，不暴露在线增删；不新增在线定位表。`ParseCompatibility` 可单独载入，`ParseDeployment` 将槽位表与清单作为同一 JSON 部署制品载入：

```json
{
  "table": {
    "shards": [0, 1],
    "ranges": [{"first": 0, "last": 1023, "shard": 0}]
  },
  "compatibility": {
    "version": "migration-2026-01",
    "legacy_data_in_shard0": true,
    "entries": [
      {"kind": "end_user", "key": "old-user", "shard": 0},
      {"kind": "session_data", "key": "ses_old", "shard": 1},
      {"kind": "session_index", "key": "ses_old", "shard": 0},
      {"kind": "object", "key": "art_old", "shard": 1},
      {"kind": "derived", "key": "old-meter-key", "shard": 0},
      {"kind": "object", "key": "mem_v9_historical", "shard": 0, "historical": true}
    ]
  }
}
```

示例中迁出的 `ses_old` 只表示历史对象位置，不授权恢复其写入（见下段）。解析拒绝未知字段、重复 JSON 字段、重复 Kind / Key、空键、未知类别、缺失目标分片、负数或未登记分片、缺失版本、尾随 JSON。普通兼容条目只接受无版本 ID；版本化历史碰撞须显式 `historical: true`。`Locator.Identity()` 返回版本与规范化 SHA-256 摘要，摘要覆盖槽位表、登记分片、排序后的全部条目和回退声明，条目顺序不影响摘要。C5 须用它核对所有进程的一致性，不能只比较版本标签。

**保留的写入限制：** 旧 Session 下新工件仍产生旧式随机 ID，旧父的委托仍保持旧派生身份；静态旧 ID 清单不能定位这些未来对象。保持现有生成语义、又不增加在线目录时，不能把仍可产生这类对象的 legacy Session 搬离库 0 后恢复写入，也不能关闭其未来旧 ID 所需的回退。迁出的历史 Session 可由映射读取、清理，但恢复写入前必须另行决定可恢复的 ID 方案并验证；不能靠逐库搜索、静默新建第二份对象或假装静态清单已覆盖未来 ID 补漏。C5 必须把这一项作为启用阻断条件。EndUser 覆盖与新 ID 槽不一致、损坏版本命名空间的历史 Session 继续生成对象也有同类限制。

兼容清单属于受控的迁移制品，包含真实 EndUser 与对象 ID，不进入日志、指标或公开诊断。数据删除后，清单及迁移报告按部署数据删除 / 备份保留政策退役；不能把停写制品变成绕过 ADR-0015 的永久用户目录。没有新增由 Janitor 在线维护的 Session 数据存储。

### 5.4 停写与幂等

仅支持新建单元或停写迁移：盘点并备份 → 停入口及全部写入者（Worker、控制器、Janitor）→ 迁移并校验数量、归属、幂等身份、删除屏障、出站水位及新写入路由 → 部署同一映射与格式版本 → 验证后恢复。只停止 HTTP 不算停写；回滚同样停写并恢复数据与映射的一致版本。旧派生 Memory 和委托 Session 的重试仍须命中原对象，不能只替换字符串创建第二份。

用量匿名化为 `~` 后留原分片，不按 `~` 重哈希。`usage_entries.idempotency_key` 的完整原键与匿名行共同保留原位置；同一原键仍是全局去重身份，不放宽成每用户一笔费用。新写入跟随真实键目标，不独立选库。

### 5.5 历史命名空间预检

`idcheck.ScanHistorical` 在一个只读、可重复读事务中逐行扫描内核 ID 列、沙箱 / 委托包装的 Session ID、删除屏障的 Session 数组、用量原幂等键，以及 Event、投影快照、Inbox Invoke、Sandbox Ledger InvokeResult 内的结构化 ID、工件引用和判断对象。不扫描任意文本、参数或结果正文。命令：

```bash
yanshi id-preflight -pg-dsn "$HISTORICAL_PG_DSN" -historical-snapshot > id-preflight.jsonl
```

源库必须是明确的历史快照 / 历史范围提取结果，且 schema 已符合当前迁移；命令本身不迁移、不写入。混合新旧库不能仅凭外形区分合法新 ID 与历史碰撞，不能对它盲目声明 `-historical-snapshot`。预检报告表、结构化字段路径、ID、固定原因：`historical_namespace_collision`、`invalid_id`、`invalid_protobuf`、`unparsed_protobuf_fields`（契约未知字段，须核实后才能确认覆盖）。历史范围内的合法 v1 外形同样报碰撞；损坏 / 未知版本另报不可解析。读取失败立即失败，有发现时输出报告后非零退出；无发现为零退出。报告须按迁移制品保护与退役。

碰撞项由迁移人员核实后以相应目标类别、原 ID、`historical: true` 登记，定位器才在解析前优先采用。不得自动将全部解析错误标为历史，不静默重命名；普通未知 / 损坏 ID 仍拒绝。正常旧十六进制 / 顺序 ID 无此冲突，内部指定 ID、`mem_ + 外部 call_id` 与旧派生值必须盘点。

不做在线迁槽、自动再平衡、跨地域双活或网关通知归属。新格式上线后降级到旧定位代码不是安全的滚动回退方案。

## 6. `pg.Tables.Offline` 的逐项安排

所有 fan-out 有并发上限、每库超时、总预算和续扫位置；分片失败不能阻止健康分片做独立清理，但依赖全部结果的操作必须显式失败，不把部分结果报成完整结果。扫描先取得键，随后每条事务只处理一个真实键。下表对应 C1 时 `pg.Tables` 的每一项 Offline。`app.fanout` 统一限制为默认并发 4、每片 1 秒、整轮 4 秒，分片号按配置顺序合并，轮次起点公平轮转。`ShardOptions.Fanout` 可覆盖这些值；模拟的 `DeterministicQueues` 使用原顺序、不启动 goroutine、不以墙钟截止。

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
| inbox.RemoveSession | 需要全部结果 | C3a 已支持已知 EndUser 时只清该用户；归属索引已删时离线扫描恢复 EndUser、逐键事务删除，全部成功才完成清理。C5 再装配逐库有界扫描 |
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

Session、工件、Memory、Grant、用量等原本全局唯一的 ID 保持原契约，不能放宽为 `(EndUser, ID)` 可重复存两行；真实键用于定位和等值访问，不改变幂等身份。

| 范围 | 当前状态 / 持续约束 |
|---|---|
| 固定格式与生成器 | 1024 槽、双槽 Session、单槽对象已实现；一次生成只消费一次原始 ID，不改变随机位数或确定性轨迹 |
| 普通 / 委托 Session | 顶层入口只有 service.Create；HTTP / Connection 不暴露指定 ID。内部指定 ID 写前校验真实 EndUser 槽；重试核对 BusinessLine、EndUser、父调用。ChildSessionID 用父 Session / call_id 摘要，真实 EndUser 来自父投影；旧父保持原子 Session 身份，新子独立选自身槽；Delegator 重推导核对 |
| 平台调用、局部 ID | Worker 仍一律改写模型 call_id 为 ForSession；Run、Schedule、Event 随已知 Session，不独立定位，若以后独立定位须用 ForSession / WithSession。删除请求固定库 0；Connection / 进程 ID 不作为持久分片键 |
| Presence / Sandbox Ledger | Touch 按已认证订阅的 Session 逐项续期、退出逐项 Remove。账本带 Session / call_id；BindLedger 在核对 sbx_ 与 Invoke 归属后绑定；started 先落盘再执行。Prune 先扫描真实 Session 再逐键复验删除，Forget 与模拟删除检查均保留；Controller / lifecycle.Guard.Ended 保持索引消失时不计沙箱费用的行为 |
| Node / Inbox | 按真实 EndUser + Node ID 寻址，结果只接受该用户该 Node 的 Inbox 调用；同用户 Node 不能更换 BusinessLine，跨用户同名 Node 的代数 / 状态独立。Worker 取投影 EndUser，网关取认证 Conn.EndUser，不信 Hello 自报值；通知键为 UTF-8 字节长度前缀 `len(EndUser):EndUserNodeID:`，尾冒号防止数字后缀误作版本，notify-shards 不等于数据库槽 |
| 伪 Node | @user 不经目录 / Inbox；@agent/ 直接委托；sbx_ 不登记目录但经 Inbox，控制器读真实投影恢复 EndUser，投影消失释放陈旧队列项交 Janitor 清理。设备 forget 跳过三种伪 Node，HostApp 切账号负责隔离 / 清理本地数据 |
| Session 索引 | Get / Touch / SetNextDue / MarkClosed / Delete 显式带 EndUser，SQL 等值匹配。LookupSession 经 SessionIndex 定位；生产入口为 service.Delete 的日志缺失恢复、janitor.delete、app.SessionData 的 Inbox 清理恢复；sim 删除不变量和 lifecycletest 同步覆盖。裸查后使用真实行授权和带键访问 |
| 工件 | Get / Delete、Service.Stat / Open 带真实 Session，读内容前核对 Meta.SessionID。调用点为 normalizeMedia、runtime.readArtifact、sandbox import_file、card.New、HTTP 鉴权后下载、Put 撤回与 DeleteSession。唯一在线裸查为 HTTP 下载 / meta 共用的 artifact()：Lookup 经 Object 定位，再加载真实 Session 按业务线 / EndUser 鉴权。Janitor 仍先删 Blob 再删元数据，两遍清扫与写后复查不变 |
| Memory / Grant | 全部在线 Get / Delete / Supersede / RecordAccess / Revoke 已带真实 EndUser，业务线 / Grant 条件仍检查，无剩余裸 ID 入口。Memory 派生 token 是 EndUser、BusinessLine、SourceSession、完整 CallID 的逐字段 `十进制字节长度:原字节` 串的 SHA-256；旧 mem_call → 新派生 → 相似度流程，只有 ErrNotFound 继续，任何幂等命中均核对完整归属；capabilities 同时识别两种 ID，不改原 Put 覆盖契约、相似度、历史失效、审批或召回规则。RecordAccess 同时核对 EndUser / Owner，保留单次召回批写；Grant 撤销要求调用业务线属于 From / To，创建时删除屏障检查两方业务线 |
| 用量、其他 EndUser 数据 | Meter 统一将完整原键 SHA-256 后 WithEndUser(use)，不截断含斜杠的键、不继承 Session 槽、不额外消费 IDs。Record 对 ID 或原幂等键冲突不插入，仅新行更新小时聚合；匿名化不恢复归属，写后复查按原 EndUser / 原键撤回。Tier、删除屏障、推送设备已有真实 EndUser；无新增在线裸查 |
| 定位与装配 | pg.Tables.Gaps 全空，Locate 仅登记 artifacts.Lookup、sessions.LookupSession。PG 存储提交 Target 给 pg.PoolSelector；app.Postgres 注入 SinglePoolSelector（默认 Locator，只有库 0），直接构造 PG 存储的默认路径也经 Locator；内存裸查同样执行默认定位。C5a 在 app 层包装全部带键接口，每库 PostgresShard 的 Select 只断言本分片；app.Stores / app.NewCore 仍是唯一装配。多库连接、有界遍历、完整汇总缓存、部分就绪与多分片消费均在 app 装配；普通单库入口不启用这些策略 |
| 预检 / 验证 | Locator 覆盖新旧目标、映射、回退、损坏 ID、配置拒绝与启用校验；artifacttest 覆盖错误 Session 读删拒绝；HTTP 用两份 PG schema 验证槽位 / 映射只决定地点及不逐库试探；PG 历史预检覆盖碰撞、正常、结构化派生引用与损坏载荷。完整 make check / make sim 仍是验收标准；C5a 新增真实双 database 一致性与确定性差分种子、逐库删除不变量，尚待远端运行；断片隔离与恢复由 C5b 测试覆盖，运行结果待远端验收 |

必要迁移注意事项：

- **0032–0033（账本）**：并发唯一索引后切换 `(session_id, call_id)` 主键并令 Session 非空。历史缺失 Session 时失败并保留原记录，不猜归属、不删 started；旧控制器的 ON CONFLICT(call_id) 不兼容，须停写同步切换。
- **0034–0038（Node / Inbox）**：从目录与 Session 索引回填真实 EndUser；冲突、多归属、无法恢复的在途调用失败回滚，不清空调用。仅没有投递且归属已删的版本缓存可在停写时删除重建。三张表并发建索引后切换复合主键，不重写 ID、代数、调用与顺序；回填留足窗口、核对数量，禁止新旧 ON CONFLICT 混跑。
- **0039–0040（用量）**：原 ID 回填非空 idempotency_key，再独立单语句、可重复并发唯一索引；不改历史 ID、金额、聚合或匿名归属。索引完成后才能恢复新写入者，旧写入者缺非空列不得混跑；旧 / 新 / 匿名重复顺序均须保持去重。
- **删除恢复**：Janitor 先用投影 EndUser，投影已删则 LookupSession；清日志与快照后第二遍清扫，再删索引。两者皆无时 Inbox 离线恢复真实 EndUser、逐用户事务，全部成功才完成；Session tombstone 不新增 EndUser。Memory 保留既有离线 DeleteSession，已知归属优先 DeleteSessionForEndUser。不能通过改变删除顺序来规避定位。
- **迁出后的生成与碰撞**：§5.2–§5.5 的映射完整性、同键新写入位置、未来 legacy 对象、碰撞历史 Session 的写入限制必须关闭后才可启用相关多库流量；静态映射的存在不等于可以恢复所有写入。
- **协议与长度**：proto 均为 string；随机 Session 47，sbx_ 后 51，@agent/ 后 54，Docker 名称 62 / 61；工件 43、调用 44，满足 ask_user 64 与 fsblob / FileLedger 128。S3 key 不拼 Session；Persona Session 为 bytea、xinli 为 text / JSONB，无需定长迁移；网页展示截尾不影响完整 ID。没有改 proto、SDK、投影语义或模型文本，C4 不需 gen / mobile / eval；已有 C2/C3b 生成格式变化的评测要求仍保留。
