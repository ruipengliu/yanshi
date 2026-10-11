# 讨论记录：与 Codex（gpt-6-astra，推理强度 max）的延展性方案讨论

> 状态：讨论原文（2026-10-11），不是设计 · 结论见[延展性评审](./2026-10-11-scalability-review.md) §8 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准
>
> 方式：Claude 出题，Codex 在只读沙箱中阅读仓库后作答；两轮。Codex 提出的代码级指控由 Claude 逐条在代码中核实后才进入结论。

## 第一轮：问题

背景：
- 项目是 Go 实现的分布式 Agent 运行时，目标是架构延展性能支持亿级月活（不要求现状能扛，但扩容不能推翻设计）。
- 我先做了一轮评审：docs/review/2026-10-11-scalability-review.md。其中 §7 记录了哪些建议已落地、哪些未做。
- 落地的关键决策：docs/adr/0028-shard-keys.md（分片键契约）、docs/adr/0029-work-classes.md（队列类别与交互池）、docs/design/model-calls.md（模型调用时限/重试/准入）。
- 热路径现状：docs/design/m2-scale-test.md §7，每个 Run 还有 34 条数据库语句，剩余大头是"每条订阅在每次提交后回读日志"。
- 项目规范：AGENTS.md（不变量：Event 日志是唯一事实源、Worker.Step 确定性、一致性套件、确定性模拟测试）。术语：CONTEXT.md。

请先读以上文件，再在代码中核实你需要的细节（internal/pg、internal/eventlog/pglog、internal/workqueue/pgqueue、internal/feed、internal/live/peer、internal/runtime/worker.go、internal/model）。不要修改任何文件。

讨论题目（请逐项回答，给出你的立场与理由；如果不同意我的评审，请明确指出）：

1. 我的评审与已落地方案中，有没有你认为判断错误或优先级错位的地方？特别是：ADR-0028 选择"EndUser 键不含业务线"、ADR-0029 用"模型调用次数 ≥ 6"判定长任务、模型准入做成单进程。
2. 剩余未做项的排序与设计：
   a. 事件流"提交进程直接推送、缺口时回读日志"（ADR-0013 的消息总线那一步）：是在 PG 分片内用 LISTEN 负载携带事件，还是引入 Redis/NATS？对私有化部署（ADR-0001：组件要少）怎么权衡？
   b. 分片层（ShardedLog/ShardedQueue）：槽位数、路由元数据放哪、扩容迁移槽位时如何保证日志的 expectedSeq 语义不被破坏、与一致性套件/差分模拟如何接入。
   c. Submit 准入（队列深度/等待时长）：阈值如何定、如何避免误杀交互请求。
   d. 全局模型准入（跨进程共享令牌）：是否值得做，还是靠按进程配额 + 提供商 429 退避就够。
   e. 内容安全 fail-closed 的降级策略：有没有既合规又不让审核提供商成为平台单点的常见做法。
   f. PostgreSQL 高可用与连接代理：PgBouncer 事务模式与 LISTEN 直连的矛盾怎么解。
3. 你认为还有哪些我没有发现的延展性风险？（例如：events 表的 bytea 与 TOAST、快照大小随 Session 增长、Janitor 的全表扫描、Presence 写入频率、Call 的音频中转带宽等。）
4. 给出一个你建议的"下一阶段"工作清单（不超过 8 项），每项一句话说明验收标准。

输出格式：Markdown，按题号分节，总长度控制在 2500 字以内。

## 第一轮：Codex 的回答

## 1. 对评审的修正

方向成立，但应先补活性、空闲负载与准入，后做分片归档；“每 Run ≤20 条 SQL”不是容量目标，固定 PG TPS 也推不出 DAU 上限。

- **EndUser 不含业务线：同意。** Memory 设计已要求统一账号，Grant 因而局部化；授权仍须包含业务线。但“业务线独立部署零改动扩容”不能同时承诺跨部署 Grant。
- **6 次：只认可为初始启发式。** 实际是已提交的 AssistantMessage 数；单次十分钟调用仍占交互池，后台 Run 收到回答也不提权。应加单步时限、可重放的服务量预算和人工唤醒的有限提权。
- **单进程准入：小部署合理。** 更急的是 [Guard](internal/model/guard.go:128) 排队不受调用 timeout 约束，Retry-After 又被截到 30 秒；先修这两项，不能把 429 当正常调度。
- 不接受“单库续租失败会兜住所有存储故障”：表级错误可以与续租成功并存；Catalog.Tools 查询失败甚至直接使 RunFailed。

## 2. 未完成项的设计与排序

**a. 事件流，优先做。** 不用 PG NOTIFY 搬事件正文：默认负载不足 8000 字节，通知队列满还会使提交失败。[PG 文档](https://www.postgresql.org/docs/16/sql-notify.html)

小部署保留 PG 的 ID/seq 提示、进程内共享读取；大部署选可选的 [Core NATS](https://docs.nats.io/learn/core-nats/)，已有 Redis 才复用它。统一订阅接口：NATS 提交后推正文，PG 后端按提示共享回读；按 seq 去重补洞，并以有预算的末尾核对覆盖“提交后未发布即崩溃”。回读按网关/Session 合并，慢订阅者断开续传。ADR-0001 允许这种部署分级。

**b. 分片，先验证迁移协议。** 先补 `pg.Tables.Gaps`；建议两类键分别用 16K 虚拟槽，按最大槽迁移时间校验，勿等同表分区。槽位→分片、epoch、迁移状态存在高可用控制 PG，CAS 更新、进程缓存。

Session 键的所有表同库同迁：源库事务级封写并排空→复制最终增量→切换路由→目标启写。两端存储校验 epoch，租约绑定 epoch；旧路由返回重定向，禁止双写，从而只有一个库能裁决 expectedSeq。接入相关一致性套件，注入旧路由、迁移中崩溃、删除竞态。轮询分片不保[现有套件](internal/workqueue/workqueuetest/workqueuetest.go:175)要求的全局 FIFO，须先修订契约；差分参照使用同拓扑内存实现。

**c. Submit，早于正式分片。** 按业务线/池统计可运行积压，排除挂起；以 `Qready/实测处理速率` 估等待，用首字 SLO 校准阈值并加滞回。交互预留份额、后台保底；仅拒新 Run，幂等重试、Interrupt、审批/回答另留容量，返回 Retry-After。

**d. 全局模型准入，有条件做。** 固定副本可以分摊额度并留余量；弹性扩容、共享账号时值得做，按账号/模型分配短期批量 RPM/TPM 与并发预算，本地调度，避免每次调用远程取令牌。429 仅作兜底。

**e. 内容安全。** 经合规确认、故障域独立的等价提供商只在超时/错误时切换，不能用备用的 allow 覆盖 block；全部不可用就挂起或拒绝。允许暂存时，输出加密隔离、可按 Session 删除且有 TTL，只重试检查，避免当前路径重新生成并计费。现有草稿和 Call 音频先展示后检查，日志 fail-closed 不等于展示合规，须另定流式检查/缓冲策略。

**f. PG 高可用，前置。** SQL 经事务池，LISTEN 和迁移锁用独立直连/会话池；故障后清缓存、重订阅并补日志。[PgBouncer](https://www.pgbouncer.org/features.html) 要保证已确认 Event 不回退，还须同步复制、旧主隔离和安全提升规则。[PG 复制](https://www.postgresql.org/docs/16/warm-standby.html#SYNCHRONOUS-REPLICATION)

## 3. 额外风险

- **空闲查询被漏算。** [WaitAbove](internal/pg/notify.go:234) 每订阅每 2 秒查 head，百万空闲订阅的理论需求约 50 万查询/秒；LISTEN 集合只增不减，随机订阅最终仍接近全频道广播。
- **已存在丢失唤醒窗口。** [Submit](internal/service/service.go:198) 首次入队被提前消费、日志提交后进程崩溃，Run 可永久 queued；应同事务写可靠调度标记或提供可靠补偿。
- [快照](internal/session/snapshot.go:43)保留全部 Runs/Calls/callIDs，Clone 每提交复制历史结构；ReadAll 又无分页。大 bytea 的 [TOAST](https://www.postgresql.org/docs/16/storage-toast.html)/WAL 与解码成本须按字节压测，大结果工件化；归档必须继续支持重放、续传及删除。
- Janitor 有时间索引，不能直接称全表扫描；真实问题是每轮 100 条、多进程重复扫描及缺少推进游标。Presence 百万连接约 3.3 万 UPDATE/s；Call 连续双向 PCM 在网关收发合计约 160KB/s/路，未计 Base64，需独立容量预算。

## 4. 下一阶段清单

1. 修复提交唤醒：所有提交窗口崩溃后，Run 无须新输入即可恢复，`make sim` 全绿。
2. 改造事件流：同网关同 Session 的订阅增多不增加稳态回读，空闲查询有预算，末条丢失可收敛。
3. 建立准入与公平调度：后台及单业务线洪峰下交互 p99 达标，后台持续前进。
4. 有界投影与分页：历史增长十倍，活跃推进成本不线性增长，投影与完整重放等价。
5. 审核容灾：双提供商故障不放行，恢复不重复生成，隔离内容可完整删除。
6. PG 故障演练：已确认 Event 不丢、seq 不回退，连接与订阅在约定 RTO 内恢复。
7. 分片迁移原型：2→4 分片期间相关一致性套件、差分模拟及删除不变量全过。
8. 容量矩阵：覆盖空闲连接、长 Session、清理积压和 Call，产出每分片 QPS/WAL/连接/带宽预算。

代码落地以 `make check` 全绿为底线。本次仅静态核查，未修改文件。
## 第二轮：Claude 的反驳与提案

第二轮。我（Claude）已经在代码中核实了你第一轮的指控，以下属实：Catalog.Tools 出错直接 RunFailed（worker.go:657）；Guard 的排队等待在调用时限之外、Retry-After 被 MaxBackoff 截断；WaitAbove 每个等待者每 2 秒兜底查询；快照与 State.Clone 随 Run 数无界增长；Submit 的提交与入队不在同一事务。这些我接受。

以下是我的反驳与设计提案，请逐条表态（同意/不同意 + 理由），并指出我漏掉的失败场景。回答控制在 2000 字以内，Markdown，按编号分节。不要修改文件。

A. 丢失唤醒。events 与 work_items 都是 Session 键，ADR-0028 第 3 条允许同一事务。我提议在 eventlog.Log 之外增加可选接口 `eventlog.Scheduler`：PG 实现把"追加事件 + UPSERT work_items + NOTIFY"做成一条语句；session.Store.Commit 在 Log 实现了该接口时走它，内存实现与模拟测试照旧走两步（差分测试仍可逐字节比较日志，因为队列不在日志中）。这样 Submit 只需一次入队。你认为这会不会破坏"Worker.Step 确定性"或一致性套件？有没有更简单的做法？

B. 空闲兜底轮询。PG 的 NOTIFY 是事务性的，活着的 LISTEN 连接不会漏通知；只有连接重建时丢，而 Notifier.broadcast 已在重建时唤醒所有等待者。我提议把每等待者 2 秒的轮询改为：LISTEN 连接健康时不轮询，由 Notifier 维护一个"连接代"（generation），等待者只在代变化时重查；再加每进程一个全局低频校验（如每 30 秒随机抽样 1% 的等待键）兜住未知 bug。这是否足够？PG 侧有没有通知可能被合并或丢失而连接仍存活的情况（例如 pg_notify 队列满、相同 payload 去重）？

C. 事件流扇出，PG-only 部署。ADR-0013 已经有执行进程 → 订阅进程的 peer 流（目前只传 token 增量）。我提议：提交进程在 Append 成功后把完整 Event 发布到同一条 peer 流；订阅进程为每个（Session）维护一个共享的 feed，所有本地订阅者挂在它上面；只在 seq 出现缺口或 peer 流断开时回读日志。这样稳态零回读，且不引入 NATS。你第一轮主张大部署用 Core NATS，这个方案与它相比缺什么？特别是：Session 没有活跃 Attempt 时（用户提交、审批、Node 结果写入都来自其他进程）由谁推送？

D. 快照与 Clone 增长。工具调用 ID 已经被改写为全局唯一（"call_"+随机 ID），callIDs 集合只用于校验唯一性，理论上可以只保留最近 N 个；Runs 列表可以在 Compaction 时裁掉压缩点之前的终态 Run（只保留计数）。这样做的风险是什么？投影版本号递增后旧快照作废，重放是否仍与裁剪前等价？

E. 长任务判定。你建议"单步时限 + 可重放的服务量预算 + 人工唤醒的有限提权"。请具体化：服务量预算用什么计量（已提交的 AssistantMessage 数？模型耗时？token？），"人工唤醒提权"如何在确定性模拟中表达，与 ADR-0029 的"每个长任务至多移交一次"是否冲突？

F. 分片与队列契约。你指出 workqueuetest 要求同优先级内按入队顺序，分片后不可全局保证。我倾向把契约改为"同一分片内有序"，并让测试通过分片数参数化。你是否同意？是否有理由保留全局顺序？

G. 把你第一轮的 8 项清单与上述 A–F 合并，给出最终的下一阶段清单（≤ 8 项），按"先修正确性、再修空闲负载、再做准入、最后分片"的原则排序，每项一句话验收标准。

## 第二轮：Codex 的回答

## A. 原子提交与唤醒

**同意原子事务；不同意内存照旧、静默降级。** 建议显式 `CommitAndWake`，仅用于 Submit、审批、外部结果；普通 Commit 不唤醒。否则 Worker 自己提交 RunSuspended 会置 dirty，[Park](internal/workqueue/pgqueue/pgqueue.go:127) 随即取消等待。

事务不破坏 Step 确定性，但认领顺序会改变 Attempt 和日志，“队列不入日志”推不出差分等价。内存、PG 均实现同一原子契约，采用提交后类别；补组合套件覆盖追加冲突、提交成功但回包丢失、与 Park/删除交错，禁止包装器漏转发可选接口后退回两步。

## B. 空闲兜底

**有条件同意取消逐等待者轮询，不同意抽样承担活性保证。** 同事务同频道同 payload 只合并一次，head 提示不受损；通知队列满使提交失败，不是成功提交后静默丢通知；监听方长事务会延迟交付。[NOTIFY 文档](https://www.postgresql.org/docs/16/sql-notify.html)

必须先确认频道 LISTEN 生效，再读 head；连接/频道代变化重做，断线时 broadcast 不够。[LISTEN 文档](https://www.postgresql.org/docs/16/sql-listen.html) 健康须由心跳超时确认；覆盖半开、动态订阅及频道配置错配。每30秒随机查1%，单键平均50分钟才被查、最坏无界；改成限速轮转和最大校准间隔，重连补查分批执行。

## C. PG-only 扇出

**同意共享 feed，不同意仅扩展 peer 就能零回读。** live_endpoint 只定位 Worker；所有提交者须推至独立于 Attempt 的 Session 转发者，无活跃 Attempt 时也由它负责，并补路由、接管、订阅登记与背压。[NATS](https://docs.nats.io/learn/core-nats/) 提供这层路由，不提供 PG 提交原子性。

漏掉的关键场景：提交成功但回包丢失或发布失败，进程与 peer 仍活着，又恰好是最后事件，既无缺口也无断线。必须保留事务 NOTIFY 的 head 对账。我建议先做 PG 提示＋每网关/Session 共享读取，NATS 后置。

## D. 快照裁剪

**不同意直接声称等价。** 随机 ID 不是精确去重证明，最近 N 会允许历史 call_id 复用、误取 SDK 旧结果；裁 Runs 还会丢掉 RunID 去重依据。须改唯一性契约，或采用精确索引、可验证的结构化 ID。

历史 API、旧回答幂等及 [Janitor 设备清除](internal/janitor/janitor.go:254)都依赖历史 Runs；先改为日志分页扫描或可重建索引。裁剪也不能依赖必然发生 Compaction。升 ProjectionVersion 只使缓存失效；要证明“新 Reduce 全重放＝新快照＋尾部”及外部行为兼容，而非新旧 State 相等。

## E. 长任务与人工恢复

**同意，但首版收窄。** 服务量取已提交 AssistantMessage＋ContextCompacted 次数 C，另设累计输出 token 上限 K；C≥6 或 token≥K 降级，阈值随 AgentDef 版本固定。耗时用于包含排队、重试的总 deadline；失败调用另受重试预算约束。

已提交审批/回答给一个不可叠加的恢复名额，下次 AttemptStarted 消费，按事件重放，重复请求不奖励。仅提高 Background 内的认领次序并预留恢复容量，不回交互池，就不增加分类导致的移交；若回交互池，必须修改“至多一次”。

## F. 队列契约

**同意片内有序，无须全局 FIFO。** 优先级比较范围也须明确；池过滤仍严格，SKIP LOCKED 只保证对可认领、未锁项排序。测试要固定键路由和调度，迁移保留原有可比顺序，不强排跨原分片项；验证单片故障隔离、跨片无饥饿，仅传分片数不够。

## G. 最终清单

1. 原子唤醒与错误分类：组合套件及模拟覆盖不确定提交、挂起、删除，临时 Catalog 故障不使 Run 终态。
2. PG 高可用：故障切换不丢已确认 Event、seq 不倒退，LISTEN 在约定 RTO 内恢复。
3. 审核容灾：双提供商故障不展示未检查内容，恢复只重试检查，暂存可按 Session 删除。
4. 有界投影：保留历史接口及删除语义，重放与快照续放等价，历史扩大十倍不线性增加推进成本。
5. 通知与共享 feed：半开、重连、末条漏发、慢订阅者均收敛，空闲查库受进程预算约束。
6. 容量基线：覆盖长 Session、空闲连接、Presence、Janitor、Call，给出准入阈值与资源预算。
7. 准入与调度：等待有总时限、遵守 Retry-After，交互 p99 达标、后台不饿死、提权可重放。
8. 分片验证：片内契约及同拓扑差分全绿，迁移中的旧路由、旧租约、并发删除无反例。

落地均须通过 `make check`，执行语义变更通过 `make sim`，AgentDef 变更另过 `make eval`；本次未修改文件。