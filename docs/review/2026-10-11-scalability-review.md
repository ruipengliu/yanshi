# 评审：面向海量用户的延展性、可用性与可维护性

> 状态：评审意见（2026-10-11），不是设计，不改变任何不变量 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准 · 关联：[章程](../charter.md) §8、[M2 扩容压测](../design/m2-scale-test.md)、ADR-0004、ADR-0007、ADR-0013、ADR-0021
>
> 评审目标：架构的延展性能否支持亿级月活。不要求现状就能承载，只要求扩容不需要推翻设计。
> 结论中凡是要落地的，都须走 ADR、`make sim` 与一致性套件；本文只给出发现与优先级。

## 0. 结论

架构可以延展到海量用户：扩展接缝基本都在，正确性不依赖单点。有三处设计层面的事应当现在补（§3），因为它们是接口与契约，拖晚了改起来要同时动所有实现与模拟测试；其余发现（§4）都能日后在接口后面换实现。

可维护性是明显强项（§5）。按现状直接承载亿级月活不行：单一 PostgreSQL 与"每个进程 LISTEN 全局通知"是硬天花板（§2）。

## 1. 已经具备的延展性

- **BusinessLine 是天然的分格（cell）单位。** Token 由业务线签发，AgentDef、配额、保留策略、内容安全都按业务线配置；一条业务线一套部署是零代码改动的第一道扩容轴。
- **Session 之间没有跨 Session 事务。** Event 日志、工作队列、投影快照、Presence、Artifact、沙箱账本全部以 `session_id` 为键，按它分片到多个 PostgreSQL 集群不改业务代码（ADR-0007 已预留）。
- **存储全部接口化，并有一致性套件与差分模拟。** 换成分片实现时 `eventlog.Log`、`workqueue.Queue` 等接口不动，正确性由同一套测试兜底。
- **Worker 无状态，正确性来自日志 fencing。** 进程数可以无限加，容量只受存储与模型配额约束。
- **模型、内容安全、推送、沙箱都是插件接口。** 超时、准入、多提供商都可以用包装层加上，不碰运行时。
- **请求前缀稳定**（系统指令只含 AgentDef 指令、工具按名排序、召回放在 Run 位置），长 Run 成本有章法。
- 压测已证实 1→4 进程 3.67× 扩展、`kill -9` 不丢 Run，方法论正确，只是规模差两到三个数量级。

## 2. 容量测算：为什么现状不够

按 1 亿月活粗估（2 千万日活、每人每天 3 个 Run、峰值 4 倍），乘以压测报告中的单 Run 开销：

| 指标 | 估算 |
|---|---|
| 峰值 Run | 约 3k runs/s |
| 数据库查询（压测 77 次/Run） | 约 230k qps |
| Event 写入 + NOTIFY（约 15 条/Run） | 约 45k/s |
| 并发长连接（日活 5% 同时在线） | 约 100 万 |
| 网关进程数（每进程 1 万连接） | 约 100 个 |

- 单个 PostgreSQL 主库写入上限通常在 1 万到 3 万事务/秒；每次 Append 都在事务里执行 `pg_notify`，PostgreSQL 提交时对通知队列加全局锁，会进一步串行化提交。
- 100 个进程各自 LISTEN 全部频道，每条 Event 要投递 100 次，通知流量约 450 万条/秒。

这两点决定了现状的上限在数百万日活量级。

## 3. 现在就该补的三件事（接口与契约层面）

1. **把分片键写成契约（新增 ADR）。** 明确两类键：
   - `session_id`：Event 日志、工作队列、投影快照、Presence、Artifact、沙箱账本；
   - `(business_line, end_user)`：Node 目录、Memory、用量、Session 索引、推送设备。
   任何新表必须声明属于哪一类；跨键查询（如按 Session 清 Inbox）只允许走 Janitor 的离线路径。没有这条约束，下一个新表就可能把两类键混在一起，分片时拆不开。
2. **队列接口加上租户与优先级。** `workqueue.Queue.Claim(ctx, holder, ttl)` 没有"认领谁的工作"的参数，全局 FIFO 一旦接入多条业务线就没法做公平与短长任务分池（章程 G6 的低首字延迟问答与小时级 Run 共存）。现在在接口上加 class（业务线、优先级），在内存与 PostgreSQL 实现里落地，并加入 `internal/sim`；分片之后再改接口要同时动所有实现。
3. **通知频道按分片命名。** `pg.Notifier` 的四个频道是全局常量，"每进程 LISTEN 全部频道"的假设散落在 pglog、pgqueue、pgnode、pgpresence 里。现在把频道名改成带分片后缀的函数、订阅按分片进行，之后做通知分片就不必再碰这些实现。

## 4. 规模到了再做（按严重程度）

### 4.1 存储

- **分片层。** 做 `ShardedLog` / `ShardedQueue`（按 `session_id` 一致性哈希），接入一致性套件与差分测试，每个分片独立 LISTEN。
- **`events` 表分区与归档。** 按 `session_id` 哈希分区；快照之前的事件归档到对象存储，只保留尾部。
- **Append 合并往返。** 当前先 `SELECT max(seq)` 再逐条插入再 `pg_notify`；可合并为带条件的单条插入，唯一约束已经给出冲突语义。
- **`pg.Notifier.latest` 改为按键 LRU。** 满 10 万条整体清空（`internal/pg/notify.go`），清空瞬间所有等待者回退查询，活跃 Session 超过 10 万时压测中已解决的空转问题会复现。
- **PostgreSQL 高可用与连接代理。** 压测文档 §6 已记录 PgBouncer 事务模式与 LISTEN 直连的矛盾，待部署形态确定时一起设计。

### 4.2 模型调用（建议顺手做，改动小、收益大）

- **无超时。** `openaicompat` 使用 `http.DefaultClient`，无请求 deadline；`Worker.during` 只在被接管或续约失败时取消。上游卡住时 Worker 永久持有租约并持续续约，Run 停在 running，`StepErrorBudget` 管不到。
- **无退避、无准入。** 429/5xx 不退避，没有按提供商的并发上限与排队。亿级规模下模型配额是首要故障源，需要每个提供商一个准入器（并发上限 + 令牌桶 + 优先级队列），每次调用带 deadline。

### 4.3 热路径冗余查询

目标：每 Run 查询从 77 次降到 20 次以内。

- 每次模型调用前 `Catalog.Tools` 都查 `nodes` 表（`internal/capability/catalog.go`）。
- 每次 `Service.Load` 都查删除记录；每个事件流订阅每 15 秒再查一次（`internal/feed/feed.go`），100 万订阅者即 6.6 万 qps 的纯检查。改为进程内缓存并由 NOTIFY 失效，删除时主动通知流结束。
- 每次 Submit 还要 `Index.Touch` 与配额求和。

### 4.4 队列与背压

- 工作队列无上限，Submit 不拒绝，延迟 SLO 无保护；应按队列深度或等待时长做准入。
- Worker 在整个 Run 期间持有租约（`internal/runtime/worker.go` 只在无活跃 Run 时释放），小时级任务长期占住 Worker 槽位；与 §3.2 的分池配合解决。

### 4.5 高可用

- **优雅停机不释放租约。** SIGTERM 后进行中的模型调用被取消但租约未释放，接管要等满 TTL，并重做整次模型调用。应在退出时 Park 或 Release，最好等当前 Step 写完日志。
- **内容安全 fail-closed 是最大的单点依赖。** ADR-0021 的取舍正确，但审核提供商的可用性等于平台的可用性。需要多提供商、合规批准的降级策略（暂存不展示而非直接失败），并把 `yanshi_moderation_mock` 为 1 列为上线阻断项。
- **连接风暴。** 每次 Connection 接入都对 `nodes` 表做 `FOR UPDATE` 事务与同用户标签扫描（`internal/node/pgnode/pgnode.go`）；网关滚动重启后百万连接同时重连会压垮数据库。SDK 需要带抖动的指数退避，网关需要接入限速。
- **跨进程增量流。** 每个订阅各开一条内部 HTTP 流（`internal/live/peer/peer.go`），应按（进程，Session）复用。
- **存储故障与确定性错误未区分。** 存储短暂不可用超过 `StepErrorBudget`（2 分钟）会把 Run 判为失败；故障切换应当重试或停放，而不是失败。

### 4.6 运维

- 迁移随每个进程启动在事务内执行，大表上无法 `CREATE INDEX CONCURRENTLY`；拆成独立 migrate Job。
- AgentDef 与发布配置从本地目录读取，几百个进程的配置一致性要靠外部保证。
- 有指标无链路追踪；跨进程排查一个 Run 需要按 `run_id` / `attempt` 的 trace。
- 仓库里没有生产部署清单。

## 5. 可维护性（强项）

- Event 日志为唯一事实源 + 乐观并发 + Attempt fencing：正确性不依赖锁，租约只管活性。
- 确定性模拟测试覆盖崩溃、租约过期、存储故障、模型故障，并对内存与 PostgreSQL 实现做差分；一致性套件约束所有存储实现。换存储、加分片有安全网。
- 文档纪律：术语表、ADR、每个里程碑的设计文档、上线清单、压测报告与改动记录；Agent 或新人凭仓库即可完成模块级改动。
- 日志只记 ID，个人数据只放在可删除的地方（ADR-0015）。

## 6. 建议的落地顺序

1. §3 的三件事：分片键 ADR、队列接口加租户与优先级、通知频道按分片命名。
2. 模型调用超时、退避与按提供商准入；停机时释放租约。
3. 热路径去冗余查询（目标每 Run ≤ 20 次）。
4. 通知分片与 LRU 缓存。
5. 存储分片层、`events` 分区与归档。
6. Submit 准入与短长任务分池。
7. 生产部署设计：PostgreSQL 高可用、连接代理、独立迁移 Job、配置分发、链路追踪；在目标环境重做压测，目标 1 万 runs/s。

## 7. 落地情况（2026-10-11）

| 建议 | 状态 | 依据 |
|---|---|---|
| §3.1 分片键写成契约 | 已完成 | ADR-0028。`pg.Tables` 登记每张表的键、离线路径与缺口，测试要求新表登记。EndUser 键不含业务线：Grant 让一条业务线读取同一 EndUser 在另一条业务线的 Memory，按 `(业务线, EndUser)` 分片会使召回跨分片 |
| §3.2 队列接口加租户与优先级 | 已完成 | ADR-0029。入队带业务线与优先级，认领按池；活跃 Run 调用模型达 6 次即为长任务，以 Background 排队 |
| §3.3 通知频道按分片命名 | 已完成 | `pg.Notifier.Channel`，`serve -notify-shards`；进程只 LISTEN 有订阅者的频道 |
| §4.1 Append 合并往返 | 已完成 | 一条语句完成检查、插入与通知；每个 Run 的语句 83.0 → 45.7 |
| §4.1 `latest` 改为 LRU | 已完成 | `pg.Notifier` |
| §4.1 分片层、`events` 分区与归档、PostgreSQL 高可用与连接代理 | 未做 | 规模到了再做；部署形态列入上线检查清单 |
| §4.2 模型调用的时限、退避与准入 | 已完成 | `model.Guard`，见[模型调用](../design/model-calls.md) |
| §4.3 热路径冗余查询 | 部分完成 | 每个 Run 的语句 83.0 → 34.1，未达到 20。剩下的主要是每次提交的追加与事件流读日志，见 [M2 扩容压测](../design/m2-scale-test.md) §7 |
| §4.4 Submit 准入 | 未做 | 拒绝的阈值要依据队列深度与等待时长的线上指标来定 |
| §4.4 长任务长期占住 Worker | 已完成 | 交互 Worker 池，长任务移交给其他 Worker（ADR-0029） |
| §4.5 优雅停机不释放租约 | 已完成 | Worker 完成当前这一步（`-drain-timeout`）后以 `handoff` 挂起并归还，恢复不算接管；沙箱控制器与 Janitor 归还租约 |
| §4.5 内容安全单点 | 未做 | 多提供商与降级策略须与合规确认；`yanshi_moderation_mock` 为 1 已是上线阻断项 |
| §4.5 连接风暴 | 已完成 | 网关握手上限，超出时返回 503 与带抖动的 Retry-After；两个 SDK 原本就有带抖动的退避，Go SDK 另外遵从 Retry-After |
| §4.5 跨进程增量流复用 | 已完成 | 按（执行进程, Session）共用一条连接 |
| §4.5 存储故障与确定性错误未区分 | 未改 | 单库部署中，存储不可用超过 TTL/3 时续约失败，Worker 放下租约并清零出错计时，2 分钟的预算不会被存储故障耗尽。分片后日志与队列可能不在同一个库，到时再区分 |
| §4.6 迁移拆成独立任务 | 已完成 | `yanshi migrate`，`serve -migrate=false`；支持 `CREATE INDEX CONCURRENTLY`（首行 `-- yanshi:no-transaction`） |
| §4.6 配置分发、链路追踪、部署清单 | 未做 | 列入上线检查清单 |

## 8. 与 Codex 的讨论结论（2026-10-11）

与 Codex（gpt-6-astra，推理强度 max）就 §7 剩余项做了两轮讨论，原文见[讨论记录](./2026-10-11-codex-discussion.md)。Codex 提出的代码级问题已逐条在代码中核实；下面只记录核实过的事实、达成的共识与仍存的分歧。

### 8.1 核实属实的新问题（§4 未发现）

| 问题 | 位置 | 后果 |
|---|---|---|
| `Catalog.Tools` 出错直接 `RunFailed` | `internal/runtime/worker.go:657` | 查 Node 列表的一次瞬时数据库错误就让 Run 终态；存储故障与确定性错误未区分（§4.5）在这里有具体反例 |
| 模型准入的排队等待不计入调用时限；`Retry-After` 被 `MaxBackoff`（30 秒）截断 | `internal/model/guard.go` | 排队无上限；提供商要求 60 秒后重试时 30 秒就重试，白白消耗一次调用 |
| 每个等待者每 2 秒兜底查询日志末尾 | `internal/pg/notify.go` `WaitAbove` | 空闲订阅的开销与订阅数成正比：百万空闲订阅约 50 万 qps，§2 的测算漏算了这一项 |
| 提交与入队不在同一事务 | `internal/service/service.go` Submit | 代码注释已承认的丢失唤醒窗口：Run 可能停在 queued 直到下一次输入 |
| 快照与 `State.Clone` 随 Run 数无界增长 | `internal/session/snapshot.go`、`state.go` | 每次 Commit 的 `Check` 都 Clone 全部未终态 Run 与全部 callIDs；长 Session 的推进成本随历史线性增长 |
| 一致性套件要求同优先级内全局按入队顺序 | `internal/workqueue/workqueuetest` | 分片后每个分片一个队列，全局顺序无法保证；契约须先改为"分片内有序" |
| Presence 心跳与 Call 音频带宽未纳入容量测算 | `internal/channel` | 百万连接约 3.3 万次 UPDATE/s；每路 Call 在网关收发约 160KB/s |

### 8.2 共识

- **原子提交与唤醒。** `events` 与 `work_items` 同为 Session 键，ADR-0028 允许同一事务。做成显式的 `CommitAndWake`，只用于 Submit、审批、外部结果写入；Worker 自己的提交不唤醒，否则 Worker 提交 `RunSuspended` 时置 dirty 会让紧接的 `Park` 失效。内存实现与 PostgreSQL 实现都实现同一原子契约，不允许"没实现就退回两步"的静默降级；新增组合套件覆盖追加冲突、提交成功但回包丢失、与 Park 和删除的交错。
- **取消逐等待者的兜底轮询，但活性不能靠抽样。** 前提是 LISTEN 确认生效后再读末尾、连接健康由心跳超时确认、连接或频道的"代"变化时重查；兜底改为限速轮转且有最大校准间隔，重连后的补查分批执行。PostgreSQL 的事实：同一事务、同频道、同 payload 只合并一次（末尾 seq 不受影响）；通知队列满会使提交失败而不是成功后丢通知；监听方的长事务会延迟交付。
- **事件流扇出先在 PostgreSQL 内做。** 每个进程为每个 Session 维护一个共享 feed，本地订阅者都挂在它上面，稳态下订阅数增加不增加回读。无论用什么推送，提交事务里的 NOTIFY 末尾对账都必须保留，否则"提交成功但发布失败、又恰好是最后一条"既无缺口也无断线，订阅者永远收不到。NATS 后置。
- **长任务判定。** 服务量取已提交的 `AssistantMessage` 与 `ContextCompacted` 次数，另设累计输出 token 上限，阈值随 AgentDef 版本固定；总 deadline 包含排队与重试。用户做出审批或回答后给一个不可叠加的恢复名额，由下一个 `AttemptStarted` 消费，按事件重放；它只提高 Background 内的认领次序、不回交互池，因此不与 ADR-0029 的"每个长任务至多移交一次"冲突。
- **队列契约改为分片内有序。** 测试固定键路由与分片数参数化之外，还要验证单片故障隔离与跨片无饥饿。
- **内容安全容灾。** 故障域独立、经合规确认的等价提供商只在超时或错误时切换，备用方的 allow 不能覆盖主方的 block；全部不可用时挂起或拒绝。允许暂存时，内容加密隔离、按 Session 可删、有 TTL，恢复后只重试检查、不重新生成。流式草稿与 Call 音频"先展示后检查"须另定策略，日志 fail-closed 不等于展示合规。

### 8.3 分歧与裁决

分歧处以 Claude 的意见为准（2026-10-11 决定）。

- **无活跃 Attempt 时由谁推送事件。** Codex 主张独立于 Attempt 的 Session 转发层（本质上是 NATS 提供的路由）。裁决：不引入新组件。Worker 提交的大事件（`AssistantMessage`）走现有 peer 流；其他进程提交的事件（输入、审批、Node 结果）通常很小，内联在 NOTIFY 负载中（上限 4KB），超限时退回"提示 + 回读"。提交事务里的 NOTIFY 末尾对账照旧保留，兜住"提交成功但发布失败"的末条。负载上限与监听方的消费速度要有指标与压测，通知队列的占用率列入告警。
  **前提（2026-10-11 补充）**：每个通知分片的监听进程数必须有上限。每个 PostgreSQL 主库每秒的通知投递次数等于该库的事件速率乘以监听它的进程数；现在 Session 随机落在各进程上、频道在连接存续期间只增不减（`internal/pg/notify.go`），每个进程最终监听全部分片，监听数等于进程总数 P。按集群 4.5 万事件/秒、单库每秒 10 万次投递的量级预算，提示型通知下 P 卡在两位数，内联负载再除以字节放大倍数。因此内联方案要求把 Session 按通知分片固定到少数归属网关进程（一致性哈希，每个分片 2 到 3 个副本），每个进程只监听自己分片的频道；客户端按 Session 路由到归属进程，或由任意网关转发到归属进程再拉取。满足这一前提后，监听数是副本数而非 P，P 随分片数线性增长。peer 流本身不构成 P 的上限：流的总数与活跃 Session 乘以远端观看进程数成正比，与 P 无关，P 上千时改为按执行进程复用的 HTTP/2 即可。
- **快照裁剪。** Claude 原提议 Compaction 时裁掉压缩点之前的终态 Run、callIDs 只保留最近 N 个；Codex 指出随机 ID 不是去重证明，历史 API、旧回答的幂等与 Janitor 的设备清除（`internal/janitor/janitor.go:254`）都依赖历史 Runs。裁决：Codex 的反对成立，采纳。先把历史访问改为日志分页扫描或可重建的索引，再做有界投影；裁剪不依赖必然发生的 Compaction；等价性要证明的是"新 Reduce 全量重放 = 新快照 + 尾部"与外部行为兼容。
- **PostgreSQL 高可用的优先级。** Codex 排在第二位。裁决：它取决于部署形态，排在准入之后、分片之前；但验收标准现在就定（已确认 Event 不丢、seq 不倒退、LISTEN 在约定 RTO 内恢复），形态确定后按此执行。

### 8.4 下一阶段清单（取代 §6）

按"先正确性、再空闲负载、再准入、最后分片"排序；每项的验收标准都以 `make check`、`make sim` 全绿为底线。

1. **原子唤醒与错误分类。** 显式 `CommitAndWake`（Submit、审批、外部结果），内存与 PostgreSQL 实现同一原子契约；所有提交窗口崩溃后 Run 无须新输入即可恢复；临时的 Catalog 或存储故障不使 Run 终态；组合套件与模拟覆盖不确定提交、挂起、删除。
2. **有界投影。** 历史访问改为日志分页或可重建索引后再裁剪投影；保留历史接口与删除语义；全量重放与"快照 + 尾部"等价；历史扩大十倍时活跃推进成本不线性增长。
3. **通知与共享 feed。** 取消逐等待者轮询，改为连接代变化时重查加限速轮转兜底；每进程每 Session 一个共享 feed；Session 按通知分片归属到少数网关进程，每个进程只监听自己分片的频道；小事件内联 NOTIFY、大事件走 peer 流、末尾对账保留。验收：半开连接、重连、末条漏发、慢订阅者都能收敛；空闲查询受每进程预算约束；稳态下订阅数增加不增加回读；每个通知分片的监听进程数不随 P 增长。
4. **容量基线。** 压测矩阵覆盖长 Session、空闲连接、Presence、Janitor 积压、Call 与 NOTIFY 负载内联，产出每分片的 QPS、WAL、连接、通知队列与带宽预算，据此定准入阈值。实测项须包含每个主库每秒的通知投递次数上限（分别测提示型负载与 4KB 内联负载、不同监听进程数），§8.3 的 P 上限估算以实测值替换。
5. **准入与调度。** 模型排队有总时限并遵守 `Retry-After`；Submit 按可运行积压与实测处理速率估等待、带滞回，只拒新 Run，幂等重试、中断、审批与回答另留容量；长任务按已提交消息数与输出 token 判定，审批后的恢复名额可重放。验收：后台与单业务线洪峰下交互 p99 达标、后台不饿死。
6. **内容安全容灾。** 故障域独立的等价提供商只在超时或错误时切换，备用方的 allow 不覆盖主方的 block；暂存内容加密隔离、按 Session 可删、有 TTL；恢复后只重试检查。须与合规确认，可与前五项并行。
7. **PostgreSQL 高可用演练。** 部署形态确定后执行：故障切换不丢已确认 Event、seq 不倒退；连接与 LISTEN 在约定 RTO 内恢复；SQL 经事务池，LISTEN 与迁移锁用独立直连。
8. **分片验证。** 队列契约改为分片内有序并参数化分片数；2 → 4 分片迁移期间旧路由、旧租约、并发删除无反例；两端存储校验 epoch、租约绑定 epoch、禁止双写；单片故障隔离、跨片无饥饿。

## 9. 代码结构（2026-10-11）

问题：除延展性之外，代码架构本身是否需要优化。结论：不需要大改。包边界清楚、接口放在消费方（`runtime`、`service`、`node` 各自声明所需接口）、12 套一致性套件、Step 模型都是对的。值得做的是三处随规模会变疼的结构问题，其余是整理。

量化依据（不含测试与生成代码）：

| 指标 | 数值 |
|---|---|
| `internal` 包数 | 71 |
| 超过 80 行的函数 | 32 / 1172 |
| 最长函数 | `State.apply` 296 行（21 个 case）、`serve` 248 行 |
| 最大文件 | `sim.go` 1308、`httpapi.go` 1119、`worker.go` 931 |
| `runtime.Config` 字段数 | 29 |
| `Janitor` 依赖数 | 26 |
| 携带 `Deletions` 字段的结构体 | 10 |
| "先写后查"删除检查的调用点 | 21 处、14 个文件 |
| 构造同一对象图的位置 | `serve.go`、`internal/sim`、`internal/eval`、e2e 测试 |

### 9.1 建议做的（按必要性）

1. **装配集中化（分片前的必做项）。** 同一张对象图在 `cmd/yanshi/serve.go`（`backends` 17 个字段）、`internal/sim`、`internal/eval` 与 e2e 测试里各装一遍。ADR-0028 的分片层会把每个存储再乘以分片数，到时四处都要改。新增 `internal/app` 之类的装配包，提供"给定存储实现与配置，返回整套组件"的唯一入口；serve、sim、eval、e2e 都经它构造。验收：新增一个存储实现只改装配包与存储包。
2. **Session 数据存放处改为登记制。** ADR-0015 要求每个存放 Session 数据的地方都能被 Janitor 按 Session 删除，现在新增一处要改 Janitor、serve、sim 三处。每个存储实现一个"按 Session 删除"的小接口并登记到列表，Janitor 只遍历列表；提供统一的"写后复查"辅助函数，使 21 处写法一致、可被 grep 与 `internal/sim` 的删除不变量覆盖。验收：Janitor 的依赖只剩队列、列表与时钟；删除不变量对登记列表中的每个存储自动检查。
3. **Worker 的模型回合改成固定顺序的阶段。** `callModel` 串着配额、目录、压缩、审核、计量、增量发布、提醒七种横切逻辑，每加一种策略都改 `worker.go`。保留 Step 作为确定性单步，把"组装请求 → 调用 → 审核 → 计量 → 提交"拆成阶段，策略以阶段的形式接入。不急，在下一个横切需求到来时顺手做。验收：新增一种横切策略不改 `worker.go`；`make sim` 全绿且日志逐字节不变。

### 9.2 可以不做的

- `State.apply` 296 行、21 个 case：它是 reducer，长 switch 便于逐事件审计，不拆。
- `httpapi.go` 1119 行、27 条路由：按资源拆文件只是整理。`channel` 与 HTTP 各自翻译到 `service` 是 ADR-0024 的设计，不是重复。
- `sim.go` 1308 行、`World` 52 个字段：测试基础设施随故障类型增长是正常的，已按 call、lifecycle、rollout 分文件。

### 9.3 不要做的

拆微服务、引入依赖注入框架、把 Step 改成异步流水线。这些会破坏章程的"单进程可运行"与"确定性模拟"两条原则，收益为零。

## 10. §8.4 与 §9.1 的落地情况（2026-10-11）

| 项 | 状态 | 依据 |
|---|---|---|
| 1. 原子唤醒 | 已完成 | `Store.CommitAndWake`：输入、审批、回答、Node 结果在追加的同一语句中入队（`eventlog.WakingLog`，内存与 PostgreSQL 实现同一契约，无原子实现时报错而不退回两步）；Submit 的前后两次入队删除。组合套件 `eventlogtest.RunWaking` 覆盖冲突、重试、挂起、释放与删除的交错；模拟测试向外部写入注入"未生效"与"已生效但报告失败"，客户端以同一输入 ID 重试须得到 duplicate，停止注入后 Run 无须新输入即推进（把实现改回两步，模拟测试即报活性失败） |
| 1. 错误分类 | 已完成 | `capability.ErrConfig`：只有白名单本身有误才立即 `RunFailed`；查询设备目录或 MCP 的故障本步重试，由 StepErrorBudget 兜底。模拟测试注入目录故障，不变量：目录故障不使 Run 立即失败 |
| 2. 有界投影 | 已完成 | 旧 Run 只剩摘要，调用 ID 分层共享，历史访问（重复审批与回答、设备清除）改读日志；见 [M2 扩容压测](../design/m2-scale-test.md) §9 |
| 3. 取消逐等待者轮询 | 已完成 | LISTEN 生效后再检查、连接心跳、重连后按速率分批重查、限速轮转校准；见 [M2 扩容压测](../design/m2-scale-test.md) §8 |
| 3. 共享 feed | 已完成 | `feed.Hub`：每进程每 Session 一个读者；慢订阅者自己补读 |
| 3. 通知分片归属到少数网关、小事件内联 NOTIFY | 未做 | 需要客户端按 Session 路由到归属进程，与部署形态一起做 |
| 4. 容量基线 | 未做 | 须在目标环境压测；队列等待与积压的指标已具备（`yanshi_queue_wait_seconds`、`yanshi_queue_ready`） |
| 5. 模型排队总时限、遵守 Retry-After | 已完成 | `model.Limits.Deadline`（`serve -model-deadline`，默认 15 分钟）计入排队与重试；Retry-After 不再被 MaxBackoff 截断，等不到下一次重试时立即放弃 |
| 5. 长任务按服务量判定、恢复名额 | 已完成 | 回合与压缩合计 6 次或输出 16000 token；用户审批或回答后以 `Resumed` 优先级在后台池中先被认领（ADR-0029 修订） |
| 5. Submit 准入 | 已完成（默认关闭） | `service.QueueAdmission`（`serve -admission-max-wait`）：按交互工作的排队等待、带滞回，只拦新 Run；阈值待线上指标确定 |
| 6. 内容安全容灾 | 部分完成 | `moderation.Failover`（`-moderation 主,备`）：只在超时或出错时切换，备用方的放行不覆盖主方的拦截。暂存与重试检查须先与合规确认 |
| 7. PostgreSQL 高可用演练 | 未做 | 待部署形态确定；代码侧已具备两个 DSN（`-pg-listen-dsn`）、心跳与就绪检查 |
| 8. 分片验证 | 部分完成 | 队列契约改为分片内有序；分片层本身未做 |
| §9.1 装配集中化 | 部分完成 | `internal/app`：一套存储由 `app.Memory`、`app.Postgres` 构造，serve、模拟测试、评测与 e2e 共用；组件图（Hub、Service、Worker）仍在各处装配 |
| §9.1 Session 数据登记制 | 部分完成 | `app.SessionData` 登记各处数据，Janitor 按列表清除；写入入口统一经 `lifecycle.Recheck` 复查。删除不变量尚未对列表自动检查 |
| §9.1 Worker 阶段化 | 未做 | 按评审，在下一个横切需求到来时做 |
| 部署设计 §6 | 已完成（清单未在集群上验证） | `serve -roles`、`-worker-pool`、`-pg-listen-dsn`、`/readyz`；`deploy/Dockerfile` 与 `deploy/k8s`（Kustomize） |

## 11. 第三轮：对实现的评审结论（2026-10-11）

对 §10 所列实现（提交 `3ec1a4a`、`425a63d`、`e29eace`）与 Codex 的第三轮讨论（[讨论记录](./2026-10-11-codex-discussion.md)第三轮）。方法同前：Codex 的每条指控都在代码中核实，不属实或程度不符的按核实结果裁定。本轮没有修改代码；`go build`、`go vet`、改动包的单元测试与 `go test ./internal/sim` 在 HEAD 上全绿。

### 11.1 核实属实的问题

| # | 问题 | 程度 | 证据 | 改法 |
|---|---|---|---|---|
| 1 | **委托输入绕过 Memory 写入闸门。** 子 Session 的任务文本由父 Run 的模型写成，可能含父 Run 读到的外部内容；子 Session 的闸门把它当作 EndUser 原话，既不算外部内容，又计入"用户原话"依据，子 Run 可以不经审批写 Memory（违反 ADR-0023） | 阻断（委托对真实 EndUser 开放之前） | `internal/runtime/memgate.go` 的 `tainted`、`userText`；`internal/service/service.go` 的 `SubmitDelegated` | `Created.parent` 非空的 Session 一律视为受外部内容影响，委托输入不计入用户原话；只收紧 |
| 2 | **子 Session 可被复活。** 派发先查子 Session 的删除记录再创建；二者之间子 Session 被删除（用户删除或保留策略）时，创建会重建它的索引与日志，事后只复查父 Session；已完成的删除不会再被 Sweep 扫到（违反"先写、后查"，ADR-0015） | 阻断（同上） | `internal/delegation/delegator.go` 的 `Dispatch`：删除检查在创建之前，`Recheck` 只对父 | 创建之后对子 Session 本身 `lifecycle.Recheck`，命中即删除并向父调用报告；`internal/sim` 覆盖"派发与删除子 Session 交错" |
| 3 | **委托的身份依赖去重窗口。** 写回、撤回与重复派发都经 `State.Input("delegate_<调用 ID>")` 找委托 Run，窗口只有 64 条输入；用户可以向子 Session 提交输入，挤出窗口后写回找不到 Run（父调用等到超时）、撤回无效、重新派发会再提交一次任务 | 高 | `delegator.go` 的 `Dispatch`、`Cancel`、`Report`；`session.MaxRecentInputs` | 投影在固定位置记录委托 Run（按输入 ID 前缀识别，递增 `ProjectionVersion`），三处共用；派发幂等改为"子 Session 已有 Run 即已提交" |
| 4 | **共享 feed 的游标可被客户端带偏。** `Stream` 把客户端的 `after` 原样交给 `Hub.join`，第一个订阅者的 `after` 成为公共读者的起点；它大于日志末尾时读者一直等待，之后加入的订阅者收不到任何新事件，直到日志超过那个值 | 高 | `internal/feed/feed.go` 的 `Stream`，`internal/feed/shared.go` 的 `join`；HTTP API 不校验 `after` | `Stream` 中 `after = min(after, st.Seq)`；测试"越界游标不影响其他订阅者" |
| 5 | **注入判断不续约、无整步预算。** `judgeResults` 不在 `during` 内执行；每步至多 8 条、每条 2 秒时限，判断端点迟缓时一步可达 16 秒，超过默认 10 秒租约；接管后判断结果未提交，新 Worker 重做，反复接管直至 Run 失败。影子模式同样伤及执行 | 高 | `internal/runtime/worker.go` 的 `judgeResults`（对比 `callModel` 在 `during` 内）；`serve -lease-ttl` 默认 10s | 判断放进 `during`；整步总预算替代逐条时限；连续失败后熔断一段时间，直接记 `unavailable` |
| 6 | **通知器新增频道的 `LISTEN` 没有时限。** 连接半开时卡在 `Exec` 上；心跳检查在同一循环里，永远走不到 | 高 | `internal/pg/notify.go` 的 `listen`：心跳 `NOTIFY` 有 `HeartbeatTimeout`，`LISTEN` 用进程 ctx | 连接上的所有语句统一以 `HeartbeatTimeout` 限时 |
| 7 | **归还租约时覆盖队列类别。** 审批或回答在租约期间入队为 `Resumed`（项变 `dirty`），Worker 随后按自己可能未同步的投影 `Park` 或 `Release`，无条件写回 `Background`，恢复名额丢失一个周期 | 中 | `internal/workqueue/pgqueue/pgqueue.go` 的 `Park`、`Release`，`memqueue` 同；`runtime.Worker.release` 以 `w.st.WorkClass()` 覆盖 | `dirty` 的项只提升不降低优先级，两个实现同改，`workqueuetest` 增加用例 |
| 8 | **判断的计量归属错误。** 置信度不足退回规则层时，模型用量挂在 `rules/injection-v1` 名下；模型不返回 logprobs 时返回错误，用量被丢弃（违反 ADR-0019） | 中 | `internal/decision/decision.go` 的 `Logits.Decide` | `Answer` 分开记"作出判断的来源"与"计费的模型"；出错路径也返回已产生的用量 |
| 9 | **委托深度差一。** `delegationRefused` 以 `len(ancestors)+1 > max_depth` 判定，`max_depth = 2` 时允许根、子、孙三层；`DelegationConfig.MaxDepth` 注释说"含本层"，按它子 Run 不应再委托；sim 不变量照实现写，没有发现 | 中 | `worker.go` 的 `delegationRefused`，`internal/agentdef/agentdef.go`，`internal/sim/delegation.go` | 按注释：目标链长 `len(ancestors)+2 > max_depth` 即拒绝；sim 不变量同步 |
| 10 | **级联中断留下孤儿子 Run。** 父 Run 中断后撤回子 Run 失败即放弃；重试 `Interrupt` 遇到已终态直接返回，不再撤回；子 Run 跑完消耗配额，结果作废 | 中 | `internal/service/service.go` 的 `Interrupt`、`cancelDispatched`；`delegator.go` 的 `Report` | `Interrupt` 对已终态的 Run 仍撤回未完成的派发；Janitor `Sweep` 对账"父调用已终态而子 Run 仍活跃"并中断 |
| 11 | **祖先白名单交集。** 祖先 AgentDef 版本取不到时 `allow` 为空，子 Run 静默失去全部工具；按工具名求交集会屏蔽祖先本可用、之后才接入的设备 | 中 | `worker.go` 的 `tools` | 祖先版本缺失为显式错误（本步重试，持续失败由 StepErrorBudget 兜底）；对同一份目录结果按每个祖先的白名单模式过滤，不再按名取交 |
| 12 | **准入统计的聚合扫描随进程数增长。** 每个有请求的进程每 2 秒对 `work_items` 做一次分组聚合 | 低 | `internal/service/admission.go`，`pgqueue.Depths` | 规模到了再做：一处计算、各处读取（Janitor 写小表或指标） |

### 11.2 不采纳或降级的指控

- **有界投影仍有线性路径**（Codex 评为高）。`Clone` 对 `Past` 做 `slices.Clip`，新 Run 校验时追加摘要会复制全部摘要；调用 ID 每 256 个合并一次时重拷旧层。属实，但成本只在新 Run 请求时与 Run 数成正比、在合并时每 256 个调用一次，不是每个事件；`Clip` 是有意的，克隆之间不能共享可追加的底层数组。裁定：低，不改；复杂度测试补一条"新 Run 校验"的用例即可。
- **按键唤醒全部等待者、每人各自查询**（Codex 评为中）。事件主题的等待者在共享 feed 之后每进程每 Session 只有一个，其他主题的键通常只有一个等待者。裁定：不改。
- **两项"阻断"的范围。** 问题 1、2 阻断的是"委托对真实 EndUser 开放"，不阻断平台其他部分；委托尚未开放，列入[上线检查清单](../launch-checklist.md)。
- **原子唤醒、`Retry-After`、模型总时限、快照与全量重放的等价性**：Codex 与本轮核实都没有发现问题。

### 11.3 本轮清单（取代 §8.4 中已完成的项，按顺序）

1. 子 Session 一律视为受外部内容影响（问题 1）；创建后复查子 Session 的删除记录（问题 2）；sim 覆盖派发与删除的交错。
2. 委托 Run 的固定身份（问题 3）；深度判定对齐注释（问题 9）。
3. `Stream` 收紧游标（问题 4）。
4. 判断进入 `during`、整步预算与熔断（问题 5）；计量归属（问题 8）。
5. `LISTEN` 限时（问题 6）；归还不降级 `dirty` 项（问题 7）。
6. `Interrupt` 对终态 Run 仍撤回派发，Janitor 对账孤儿子 Run（问题 10）；祖先白名单按模式过滤、缺失显式报错（问题 11）。
7. 规模到了再做：准入统计集中计算（问题 12）。

验收仍以 `make check`、`make sim` 全绿为底线；第 1、2、6 项改动执行语义，须过 `make sim`；没有一项改动给模型看的文本，不需要重跑 `make eval`。

### 11.4 落地情况（2026-10-11）

| 问题 | 状态 | 做法与验证 |
|---|---|---|
| 1 Memory 写入闸门 | 已修复 | `runtime.tainted` 对子 Session 返回 true，`userText` 不计入委托的任务；单元测试 `TestDelegatedTaskIsNotUserText` |
| 2 子 Session 复活 | 已修复 | 创建后对子 Session 本身 `lifecycle.Recheck`；清理已完成时撤回刚写的索引与日志，未完成时再交给 Janitor。模拟测试在检查与创建之间删除并清理子 Session（`ChildDeletionRaces`）；去掉复查，删除不变量即报"已删除的 Session 有事件" |
| 3 委托身份 | 已修复 | 投影 `State.Delegated`（快照字段 `delegated_run`，投影版本 10）；写回、撤回、重复派发共用；单元测试 `TestDelegatedRunOutlivesInputWindow` |
| 4 共享 feed 游标 | 已修复 | 共享读者以 `min(after, st.Seq)` 为起点，订阅者自己仍按 after 过滤；`TestHubIgnoresCursorBeyondHead`（去掉修复即失败） |
| 5 判断续约与预算 | 已修复 | `during` 内判断、整步 4 秒预算、`decision.Breaker`；`TestBreaker` |
| 6 `LISTEN` 时限 | 已修复 | 连接上的语句统一以 `HeartbeatTimeout` 限时。顺带修正 `TestWaiterConvergesAfterListenerLoss` 的竞态：等待者可能先由校准收敛，测试改为等待重连完成 |
| 7 归还降级 `Resumed` | 已修复 | `workqueue.ReturnedClass`：租约期间以 `Resumed` 入队、持有者以 `Background` 归还时保留 `Resumed`；两个实现一致，一致性用例 `ReturnKeepsResumedEnqueuedDuringLease`（内存与 PostgreSQL） |
| 8 判断计量 | 已修复 | `Answer.Billed`；`TestLogits` 覆盖退路与出错时的计费 |
| 9 深度差一 | 已修复 | `len(ancestors)+2 > max_depth`；sim 不变量与 AgentDef 注释一致 |
| 10 孤儿子 Run | 已修复 | 中断已终态的 Run 时仍撤回派发；子 Worker 调用模型前 `Delegator.Reconcile`。模拟测试注入撤回失败（`CascadeFaults`）；模拟中的 Run 太短，自行停下的路径由 e2e `TestDelegationOrphanStops` 覆盖（去掉检查即失败）。**顺带发现**：`Service.Close` 在撤回失败时跳过索引与 Janitor，重试又因"已关闭"直接返回，沙箱工作区永远不会销毁；改为关闭之后的步骤幂等、重试时补做（`afterClose`），模拟测试的关闭在撤回失败后重试 |
| 11 祖先白名单 | 部分采纳 | 祖先版本缺失改为 `capability.ErrConfig`（Run 失败）。不改为按模式过滤：MCP 工具只留下合成后的名称，按模式过滤要在 Catalog 之外重复一遍各类工具的匹配规则，容易与 `Catalog.Tools` 不一致；设备与 MCP 目录已有缓存，按名取交的代价只是 CPU，且对之后接入的设备同样正确 |
| 12 准入统计 | 未做 | 规模到了再做 |

验证：`make check` 全绿；`make sim`（2000 个种子、100 个 PostgreSQL 种子）全绿。`make sim` 在改动之前就已经超时：全量种子在 HEAD 上需 601 秒，超过 `go test` 默认的 10 分钟时限，Makefile 的 sim 目标改为 `-timeout 40m`。改动前后各测试耗时相当（PostgreSQL 差分 412 → 391 秒，长 Run 113 → 131 秒）。
