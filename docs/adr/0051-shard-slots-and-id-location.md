---
status: accepted
---

# 固定槽位、裸 ID 定位与停写迁移

[ADR-0028](0028-shard-keys.md) 已确定 Session / EndUser 两类键。[Persona 架构](../design/persona-architecture.md) §13 Q109、Q110、Q118 与 §11 Q124 的 C1 要求真实多库之前先固定裸 ID 定位与兼容规则。本 ADR 定稿这些规则；C1 实现纯函数、配置解析、ID API 与工件生成，C2–C4 补齐接口和生成入口，C5 才装配多库。C1 完成不表示已支持分片部署。

## 1. 固定槽位与部署配置

- 固定 **1024** 个槽位，编号 `0..1023`。键的原始 UTF-8 字节经 SHA-256，取前八字节按无符号大端整数解释，再 `% 1024`；不做 Unicode 归一化、大小写转换或业务线拼接。实现为 `internal/shard.Hash`。槽位数和算法属于持久格式，不能作为启动参数修改。
- 两类键共用同一槽位表，但键类别仍由存储契约区分；不同键哈希碰撞是正常的，同槽不意味着同一归属或授权。Session 的具体取槽规则见 §3，不能对带槽位的完整 ID 再调用 Hash。
- 槽位到分片是静态部署表。分片号是非负稳定编号，不是 DSN 数组下标；必须登记兼容库 `0`。每个槽位恰好赋值一次，拒绝遗漏、重叠、越界与未登记的分片；没有配置的零值表不能用于定位。
- 不能用 `hash % 库数`：库数变化会重映射大量没有搬迁的键。同样不能使用 Go 进程随机种子的哈希。扩容迁移的是槽位，完成停写搬迁后才切换表，键到槽位不变。

`shard.ParseTable` 解析下列 JSON（区间首尾均包含），拒绝未知字段、非法值与尾随 JSON；`NewTable` 复制为不可变的表。C1 不接 serve 参数、不打开数据库，C5 将它纳入 `app` 的部署配置校验，确认每个登记分片都有 SQL DSN、直连 DSN、迁移和 Notifier。

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

**选择在 Session ID 中同时携带 EndUser 槽位。** `sessions` 表仍 ByEndUser；`Get / Touch / SetNextDue / MarkClosed / Delete` 只有 Session ID 时用 `uuu` 定位，日志、队列、快照等用 `sss`。旧 Session 的这两种访问都去兼容库 0。代价是 Session ID 多四个字符、归属 EndUser 不能被任意修改；收益是不增加需要一致性维护与删除登记的辅助索引，也不增加每次访问的定位查询。

槽位只能定位到库，不能还原真实 EndUser。C3 的索引接口仍须在调用方已知时传入 EndUser；只有裸 ID 的路径先在唯一目标库取真实索引行，再以真实 EndUser 做等值访问与归属检查，不能把 `uuu` 当真实键或权限。新建 Session 时须校验 `uuu == Hash(真实 EndUser)`；C2 的委托派生必须拿到相同的真实 EndUser，子 Session 自身仍独立选槽，不继承父 Session 数据槽。

## 4. 外部 ID 与授权

以下入口必须补真实键，不能用生成器或对外部 ID 的格式猜测代替：

| 入口 | 必须携带 / 恢复的真实键 |
|---|---|
| Node 注册归属检查、Directory.Get / SetOffline、心跳与断开 | 经认证的 EndUser；Node ID 改为 EndUser 内唯一 |
| Inbox.Put / Remove / Pending / Wait 及版本查询，网关投递与 Worker 撤回 | EndUser + Node ID；结果仍核对 Session 与调用 |
| `call_id`；Sandbox Ledger.Get / Put、重复调用结果 | 所属 Session；不能按 call_id 哈希或相信其外形（模型给的 ID 由 Worker 改写为平台 ID，设备侧仍只把它当去重键） |
| Presence.Touch / 连接退出 | 连接的已认证订阅 Session 集合，逐 Session 续期或删除；不能按 Connection ID 选一个库 |
| EndUser 的用量、档位、Node、Grant、删除屏障、推送登记 | 原始 EndUser；不拼 BusinessLine，也不解析 EndUser ID 的字符串外形 |
| 内部 `CreateRequest.SessionID`（委托派生） | 真实 EndUser 和父 Session / call_id；C2 校验新格式并保留旧派生的幂等身份 |
| 客户端 input_id、card_id、动作 ID、外部 Schedule ID | 已授权 Session 内的去重或定位，不能作为独立分片键 |

在线裸工件、Memory、Grant、用量 ID 只能选一个明确目标；禁止逐库试探。读出后仍核对元数据的 Session、EndUser、BusinessLine 或 Grant 条件。伪造、篡改、碰撞的槽位最多改变查询地点，不能改变授权；不存在与不属于请求方走既有拒绝路径。工件输入仍由 `service.normalizeMedia` 比较真实 `Meta.SessionID`，HTTP 下载仍按真实 Session 鉴权。

## 5. 旧 ID 与停写迁移

1. 无槽位的旧 ID **默认全部去兼容库 0**，其数据必须留在那里。`Table.Shard(slot, legacy=true)` 返回库号 0；这与槽位 0 可被配置到任何库是两回事。旧 Session 新建工件也保留旧格式，继续与 Session 同库。
2. EndUser 由业务线指定，不能从它的字符串判断新旧。若旧 EndUser 数据保留在库 0 且没有显式兼容映射，启用分片的配置必须把所有历史 EndUser 对应的哈希槽保留给库 0，保证按 EndUser 查与旧裸 ID 查一致；这些槽可能占满全部 1024 槽，此时必须进行下面的停写迁移，不能直接改取模或把新旧记录拆开。新的 `ForEndUser` / Session 双槽 API 仅在这个前提或迁移已完成时启用。
3. 要把旧数据搬离 0，停写迁移必须同时交付显式兼容映射，覆盖键类别与真实键、旧裸对象 ID、旧 Session 的数据位置和索引位置（两者可能不同），以及旧派生调用的幂等定位。C4 的定位适配必须先查这个权威映射，未覆盖的旧数据经迁移清单证明仍在 0 后才允许默认回退；不能搬走后继续无条件回退，也不能用在线全库搜索补漏。新写入跟随该真实键的目标，不独立选库；已搬迁旧 Session 下新建工件也须建立可恢复的兼容定位，不能只复制一份迁移时的旧 ID 清单后停止维护。C1 不实现兼容映射存储；若 C4 新增含 Session 数据的定位存储，须接入 Session 删除登记与模拟删除不变量。
4. 只支持新建单元，或明确停写流程：盘点并备份 → 停入口及全部写入者（含 Worker、控制器、Janitor）→ 按槽 / 兼容清单迁移并校验数量、归属、幂等身份、删除屏障与出站水位 → 部署同一映射与格式版本 → 验证后恢复流量。只停止 HTTP 不算停写；回滚也必须停写并恢复数据与映射的一致版本。
5. 迁移预检必须检查历史 ID 与保留的 `_v数字_` 命名空间是否碰撞，尤其 `mem_ + 外部 call_id` 和内部指定 ID。碰撞项明确登记为历史对象并由兼容适配优先处理；不静默重命名，不把旧对象误认为新格式。正常生成的旧十六进制 / 顺序 ID 没有此冲突。
6. 老派生 Memory 和委托 Session 的重试须仍命中原对象；不能只替换字符串后创建第二份。用量已匿名化为 `~` 的行留原分片，不按 `~` 重哈希；需要按 ID 匿名化的定位同样保留原位置。

不做在线迁槽、自动再平衡、跨地域双活或网关通知归属。新格式上线后降级到旧定位代码不是安全的滚动回退方案。

## 6. `pg.Tables.Offline` 的逐项安排

所有 fan-out 有并发上限、每库超时、总预算和续扫位置；分片失败不能阻止健康分片做独立清理，但依赖全部结果的操作必须显式失败，不把部分结果报成完整结果。扫描先取得键，随后每条事务只处理一个真实键。下表对应 C1 时 `pg.Tables` 的每一项 Offline；实现留 C5。

| 表与 Offline 路径 | 分片后的执行 |
|---|---|
| outbox.List / Read / Head | 按显式分片读各自信封与安全起点，游标绑定分片 / epoch；List 的完整分片枚举有界汇总，失败不隐藏分片 |
| outbox.Maintain | 每库扫描候选、逐 Session 分批清理；删除信封按该库地板处理 |
| outbox.Stats | 各库统计后汇总；缺一库不报告完整空间用量 |
| work_items.Claim | 公平轮转逐分片认领，租约与后续操作回原分片；坏库超时后让其他库继续 |
| work_dues.Promote | 逐库读到期候选，逐 Session 事务移回本库队列；共享总预算并公平推进 |
| janitor_items.Claim | 与工作队列一样逐分片认领、续租、完成 |
| sandbox_items.Claim | 与工作队列一样逐分片认领；由沙箱 Node ID 还原 Session |
| sandbox_activity.ClaimIdle | 每库按时间认领空闲候选，在所属 Session 的分片完成回收 |
| sandbox_ledger.Prune | 逐库有界扫描更新时间，按 Session 分批清理 |
| presence.Prune | 逐库扫描过期项，逐 Session 清理并更新本库版本 |
| session_tombstones.Pending | 各库独立列出未完成删除并入本库 Janitor 队列，可重复执行 |
| session_tombstones.Request | 先在库 0 读取并授权删除请求，再到每库按 request_id 统计，累计 Sessions / Completed；去掉与 deletion_requests 的跨库 JOIN，缺库时报不可用，不报告完成 |
| inbox.RemoveSession | Janitor 逐 EndUser 分片清除指定 Session 的投递副本；扫描恢复 EndUser 后逐键删除，全部成功才完成 Session 清理 |
| sessions.List（未指定 EndUser） | 低频业务线服务端查询，各库取有界分页，以稳定排序键合并；分页位置保留各库进度，缺库不返回完整列表 |
| sessions.IdleBefore / ClosedBefore / DueBefore / ScheduledAfter | 每库扫描保留策略或绝对期限候选；按真实 EndUser 更新索引，按 Session 槽执行关闭 / 删除 / 定时，多步可重复 |
| memories.DeleteSession | 逐 EndUser 分片查来源 Session，再按真实 EndUser 删除；所有分片完成后才确认该清理步骤 |
| usage_entries.Report | 各库按业务线 / 时间返回部分统计或分页，完整合并后返回；缺库不补零 |
| usage_entries.Prune | 逐库按时间扫描、按真实键有界清理；匿名行留原分片 |
| usage_entries 匿名化后的 `~` | 不搬库；按原 EndUser 在原库匿名化，随后按 ID 定位或逐库离线清理；不能再用匿名值求槽 |
| usage_hourly.Prune | 各库有界清理历史聚合行 |
| usage_hourly 的 `end_user = ''` 合计 / Spent | 每库保留业务线部分和，汇总完整部分和后缓存供在线配额判断；不把全部业务线写入一个热点库 |
| end_user_tombstones.PendingEndUsers | 各 EndUser 分片认领注销确认工作；其关联 Session 删除状态按 Session 分片检查，缺库就保持未完成 |
| end_user_tombstones.PruneEndUsers | 各库扫描宽限期候选，在 EndUser 所属库事务清除迟到数据并删屏障；需要的跨库 Session 清理未确认时不删屏障 |

业务线配额是软限制（ADR-0019）：缓存项包含完整分片集合 / 配置版本及截至时间，设置有限有效期；映射切换后失效。只发布所有相关分片成功的部分和。分片失联时可用仍在有效期内的完整缓存；没有有效完整缓存则有界失败，不能用缺失用量当零放行。EndUser 配额在其所属库读取，不为查询本用户用量扫描其他库。

`pg.WithTx` 依 pool 身份复用事务，不是跨库事务管理器。Log / Queue / Waking 必须选同一 Session 的同一库；EndUser 屏障与该键的迟到数据清理仍须同库。跨键与跨库的多步操作保留幂等和恢复进度。

## 7. 部分可服务

按 Q118，健康分片的读取、写入、认领和出站消费继续。仅依赖失联分片的请求有界失败；汇总不得把部分成功当作完整成功。就绪检查报告每个分片的 SQL、迁移与 LISTEN 状态，结合进程角色和可服务分片决定是否接受流量，不能简单对所有分片取 AND；请求处理仍按其实际依赖拒绝不可用目标。控制库 0 失联可阻塞删除请求查询等依赖它的操作，但不能因此关闭不依赖它的健康 Session。

## 8. C1 / C2 交付边界与后续生成点

C1 只接 `artifact.Service.Put`：有槽 Session 生成同槽工件，旧 Session 的 ID 与生成器调用次数完全沿用。新增包没有存储、事务、网络、时间或新的随机源；不改 `app.Postgres`、`pg.Tables.Gaps`、投影或协议。C1 阶段生产 Session 仍是旧格式；C2 接通下面的 Session 生成点后，正常新建 Session 与其工件开始使用新格式。

| 构造点 | C2 后的状态 / 后续安排 |
|---|---|
| service.Create | C2 已接 `Generator.Session(EndUser)`，仍只消费一个原始 ID；写入前校验新格式的真实 EndUser 槽，legacy 接受 |
| delegation.ChildSessionID：父 Session / call_id 的 SHA-256 派生 | C2 已补真实 EndUser：旧父保留 `ses_<hex>`，新父接 `SessionID`；子自身槽来自原 token，EndUser 来自父投影；派发重推导并核对目标 |
| artifact.Service.Put | C1 已接 ForSession；C4 才接裸 ID 定位 |
| memory.Service.Save：`mem_ + CallID` 或 IDs；capabilities 的重复判断 | C3/C4 接 WithEndUser / ForEndUser，并同步修改重复判断、旧幂等定位与真实归属检查 |
| memory.Service.CreateGrant | C3/C4 接 ForEndUser，读 / 撤销仍核对真实 EndUser 与业务线 |
| runtime.Worker 的模型 / 决策计量、runtime.compact | C3 接真实 EndUser 的用量 ID，不继承 Session 槽 |
| oneshot.Service、speech.Service 的用量生成 | C3 接 ForEndUser；保持调用次数与匿名化重查的定位 |
| sandbox.Controller 的 `sbx_ + call_id` 用量、call.Call 的 `call_<meterID>/<轮次>` 用量 | C3 把确定性去重 token 编码为安全字符并嵌 EndUser 槽；保留历史幂等键兼容 |
| runtime.Worker 改写模型给的 call_id | C2 已接 `ForSession("call", SessionID)`：模型给的 ID 可能为空或跨轮、跨 Run 重复，仍一律改写为平台 ID（每个调用消费一次生成器，与原来相同），新 ID 继承 Session 槽位 |
| service.checkpoint / background 的 Run、schedule 的 Schedule / Run、schedule_capability 派生 ID；session.Store 的 Event | 已随 Session 传递，无独立裸 ID 查库需求，可保持旧局部格式；以后若独立定位，必须用 WithSession / ForSession |
| service 的删除请求 ID | 控制数据固定库 0，不根据请求 ID 随机分片 |
| channel 的 Connection ID、serve 的进程 ID | 非持久对象定位键，继续原生成器；Presence 从订阅集合取得真实 Session |

C2 已清 Session 接口（Presence、Ledger）并接普通 / 委托 Session；C3 清 EndUser 接口（Node / Inbox / 索引 / Memory / Grant / 用量 / Tier / 删除屏障）；C4 接裸 ID 与显式兼容定位；C5 只在这些阻断项关闭后装配多库、fan-out、完整缓存与部分就绪。存储接口已有真实键就直接传，不拿新增 ID API 代替缺口清理。

已发现的后续缺口：Memory.Save 当前命中 `Store.Get(mem_ + CallID)` 后直接返回，未核对 EndUser / BusinessLine；C3/C4 必须补归属校验，并覆盖跨用户相同外部 call_id 与同槽碰撞。旧用量 token 含 `/`、旧派生幂等 ID、历史 EndUser 槽固定在 0 的约束，均是多库启用前的阻断项，不能仅靠格式单测宣称已解决。

proto 的 ID 均为 string；SDK 没有 Session 固定长度解析。新随机工件 43 字符满足 ask_user 的 64 字符上限与 fsblob 的 128 字符上限；字符集兼容 nodesdk Ledger。C1 不修改 proto、SDK 或 mobile，不需要 `make gen` / `make mobile`。C2 仅增加内部 `sandbox.Ledger` 的 Session 键与绑定适配器，未改 `sdk/nodesdk.Ledger`、Executor 或节点协议，不需要 `make gen` / `make mobile`。

验证覆盖：固定哈希向量、1024 槽全量编码往返、未知版本 / 非法槽位拒绝、Session 双槽与真实键继承、legacy 库 0 与槽位 0 分离、配置遗漏 / 重叠 / 越界拒绝、生成器确定性与消费次数、工件 URI / 文件存储往返、伪造槽位仍被真实归属拒绝且不写日志。真实单 / 双库差分与故障注入属 C5；本地不具备 Go 工具链，C1 的 `make check` 与 `make sim` 由远端验证后才能确认通过。

### C2 入口、迁移与格式检查

- **顶层生成点**只有 `service.Create`。HTTP `create`、Connection `channel.Handler.do`、eval 的 `(*instance).trial`、sim 初始化与关闭 / 注销后的替换 Session 都调用它，没有另一个生产顶层生成器。外部 HTTP / Connection 的创建契约不暴露指定 ID；`CreateRequest.SessionID` 只由内部委托及测试指定。内部指定 ID 也在任何索引或日志写入前校验；幂等重试同时核对真实 BusinessLine / EndUser 与父调用。
- **委托**：`ChildSessionID(parentSession, callID, endUser)` 返回 `(string, error)`；Worker 用父投影的 EndUser，Delegator 再从父投影推导并核对目标，不接受从子 ID 反推归属。sim 不变量同时核对父子真实归属。旧父的在途委托仍命中原子 Session，两类槽均按 legacy 留在兼容库 0。
- **账本**：内部 `sandbox.Ledger.Get / Put` 带 `(SessionID, callID)`，`BindLedger` 给单次沙箱执行固定 Session。控制器先核对 `sbx_<SessionID>` 与 Invoke 归属，再构造 Executor；SDK 的 started 写入与执行顺序不变。内存与 PG 使用同一一致性套件；`HasSession` 让模拟删除不变量同时检查两种实现。`app.SessionData` 的 Forget 登记继续有效。
- **迁移**：`0032` 独立、可重复的 `CREATE UNIQUE INDEX CONCURRENTLY` 建立 `(session_id, call_id)` 索引；`0033` 将 Session 设为非空，并用该索引替换原 call_id 主键，不重写历史 ID 或结果。升级前须核实 `sandbox_ledger.session_id IS NULL` 的历史行；有此类行时迁移失败并保留原记录，不能猜归属、直接删除或丢掉 started。切换主键后旧控制器的 `ON CONFLICT (call_id)` 已不适用，迁移与控制器版本须按停写流程同步切换。
- **Presence**：`Touch(ctx, sessionID, connID, expires)` 只续期已存在的单条订阅；连接心跳复制 `joined` 的已认证 Session 集合后逐个续期，退出仍由每个订阅逐 Session Remove。Presence 与账本的 Prune 先离线扫描 Session，再逐 Session 删除并复验时间条件；有界多库扫描与续扫仍留 C5。`pg.Tables` 仅清掉本步的两个 Gap，artifacts 与 EndUser Gaps 保留。

| 检查项 | C2 结论 |
|---|---|
| Session、沙箱 Node、委托 Node | 随机 Session 47 字符，`sbx_` 后为 51；委托实际是 `@agent/`，为 54。均完整传递，无定长截断 |
| Docker 名称与标签 | `yanshi-sbx-` + Node 共 62，`yanshi-ws-` + Node 共 61；新字符集不会被名称清洗替换，标签保留完整 ID |
| 工件、S3 与 ask_user | 工件 ID 43，blob key 不拼 Session；S3 为配置前缀加工件 key。满足 fsblob 128 与 ask_user 图片 ID 64 的上限 |
| 平台补的 call_id 与设备账本 | 新随机 call ID 44，满足 FileLedger 128 字符与字符集约束；Session 存储长度为 uint16，可容纳 47 |
| Persona / xinli | Persona 的 Session 列为 bytea，xinli 为 text，其余相关值在 JSONB；没有定长列，不需业务库迁移 |
| 网页 SDK / 控制台 | 委托只去掉完整 `@agent/` 前缀；显示末 8 位不影响路由与存储，未按 ses_hex 解析 |
| eval / e2e | 没有 Session 固定长度假设；脚本与评测替身给的固定 ID（如 `x`）照旧由 Worker 改写 |

C2 新增 / 更新的验证：`TestCreateSessionIDs`、`TestChildSessionIDFollowsParentFormat`、`TestDispatchUsesParentEndUserAndRetriesSameChild`、`TestWorkerRewritesCallIDs`、`sandboxtest.RunLedger`（内存 / PG：跨 Session 同 call_id、状态结果、Forget / Prune、started 先落盘、接管重试与写入失败）、`TestLedgerMigrationPreservesStartedAndRequiresSession`、`presencetest` 的 Touch 隔离与迟到心跳、`TestHeartbeatTouchesOnlyJoinedSubscriptions`；已有委托 e2e / Janitor 调用点同步更新。远端须执行 `make check`、`make sim`，包含 `TestEveryTableHasShardKey`、`TestSimulationIsDeterministic` 与 `TestPostgresMatchesMemory`；本机未运行 Go 测试，不据静态检查宣称已通过。

仍有范围外缺口：设备 SDK 的账本与 Inbox、Memory 派生（`mem_ + call_id`）与沙箱用量去重以 call_id 为键，跨 Session 的唯一性依赖平台改写出的全局唯一 ID；分片后这些存储仍须带真实键（C3 / C4）。FileLedger 限制 ID 的字符集和长度，平台 ID 满足。本步不扩展三端 SDK，没有修改投影或 `ProjectionVersion`。
