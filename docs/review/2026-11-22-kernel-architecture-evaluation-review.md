# 内核架构评估复核

> **状态** 评审意见，2026-11-22。评审对象为 [《yanshi 内核架构评估》](../architecture/architecture-evaluation.html)，原报告以 2026-11-21 的源码为基准，本次以收录报告的提交 `23e4dd0` 核对原诊断。初次核对时看到业务线作用域、幂等与公平调度的未提交草稿，成稿复核时这些改动已不在当前工作区。下文的实现状态以该提交为准。本文未重跑测试、模拟、评测或压测。
>
> 术语与现行决定见 [内核术语](../../CONTEXT.md)、[项目章程](../charter.md)和 [ADR 索引](../adr/README.md)。下文沿用原报告的 B、R 编号。

## 0. 结论

Event 日志、Attempt fencing、原子唤醒、确定性 `Worker.Step` 和按键分片为多条业务线提供了可复用的持久执行语义。第二条业务线接入时，最先需要验证的是**同一单元内的定义级授权、服务端异步重试，以及共用队列和模型时的延迟**。对真实 EndUser 开放还须逐项完成现行上线检查清单。 [执行原语](../design/m0-core-primitives.md) [单元部署](../design/deployment.md) [上线检查清单](../launch-checklist.md)

可信内容泛化、Run 内上下文折叠、在线迁槽分别涉及来源推导、请求前缀与出站连续性。原报告提出了对应的限制，实施优先级应由业务需求、目标环境测量和现行 ADR 的重审条件决定。ADR-0043 允许提前完善授权、幂等这类内核既有职责。引入业务规则时，仍须说明第二条业务线的需求和业务侧实现的正确性或安全成本。 [ADR-0043](../adr/0043-kernel-persona-app-layers.md)

## 1. 接入与上线问题

### 1.1 B1 / R1 — 定义级授权

评估基准中，AgentDef、Release 与 Oneshot 按全局名称解析，解析过程没有校验定义归属。MCP 工具已有按业务线筛选，彼时的额外问题是 Server 名全局唯一。同一单元内的 Session 归属、同一 EndUser 经 Grant 共享 Memory、定义的可见性是三个独立的契约。首页的「0 租户隔离」宜改为具体的定义级缺口。 [定义解析](../../internal/agentdef/agentdef.go) [MCP 工具筛选](../../internal/mcpcap/mcpcap.go) [分片键 ADR](../adr/0028-shard-keys.md) [Memory Grant ADR](../adr/0003-memory-owned-by-business-line-shared-by-grant.md)

补齐定义归属时，授权须覆盖建 Session 的默认与指定版本、Worker 读取定义、委托目录、Oneshot 和 MCP 连接解析。验收须使用拟部署的配置，确认未声明归属的旧定义如何迁移、哪些定义有意共享，以及已有 Session 如何解析所引用的版本。 [建 Session 入口](../../internal/service/service.go) [委托入口](../../internal/delegation/delegator.go) [Oneshot 注册表](../../internal/oneshot/registry.go)

### 1.2 B5 / R2 — 建 Session 与输入的幂等契约

xinli 已有创建意图、孤儿回收和日志对账。提交结果未知时，编排器会分页查找 `input_id`，确认提交是否生效，这是可观察的接入成本。HTTP 和 Connection 的公开建 Session 入口尚未支持 `external_key` 或 `labels`，服务端按最近 64 条带 ID 的输入判重。 [xinli 编排恢复](../design/xinli-captures.md) [HTTP 入口](../../internal/httpapi/httpapi.go) [Connection 契约](../../proto/yanshi/v1/node.proto) [输入判重](../../internal/service/service.go)

建议先明确 `external_key` 的作用域、同键不同请求的结果、删除后的重试语义，以及日志与索引分属不同键时的恢复办法。持久输入索引须能返回首次结果并与 Event 追加和唤醒一致，明确保留期、过期后的行为，以及 Session 删除时的清理。`labels` 可按独立查询需求评估。业务对象与 Session 的绑定仍由业务线负责。原报告的「接入代码下降 70%+」缺少改造前后的测量。 [分片键 ADR](../adr/0028-shard-keys.md) [Session 删除 ADR](../adr/0015-session-deletion-by-physical-removal.md) [评估 R2](../architecture/architecture-evaluation.html#roadmap)

### 1.3 B1 / R8 — 共用队列与模型

同一分片内，队列按优先级和入队顺序认领。模型准入先获取进程内的速率令牌，再按调用类型排队等待并发名额。队列已有按业务线过滤的能力，Oneshot 也有进程内的业务线并发上限。两条业务线共用资源时存在尾延迟相互影响的机制依据，实际影响取决于到达率、调用时长和模型额度。 [队列认领](../../internal/workqueue/pgqueue/pgqueue.go) [模型准入](../../internal/model/guard.go) [Oneshot 限额](../design/deployment.md#9-单次调用部署参数)

共用单元的验收应采用混合负载。保持一条线的交互基线，逐步增加另一条线的交互或批量负载，观察两线的排队延迟、首字 p99、拒绝率及提供商上限。根据瓶颈所在决定工作队列、模型并发和 RPS 分别需要什么保证。原报告给出的公平排队算法和跨副本协调属于待验证的方案。 [容量与准入验收](../design/deployment.md) [评估 R8](../architecture/architecture-evaluation.html#roadmap)

### 1.4 B6 / R3 — 生产验收

真实内容安全提供商、审核故障及草稿和 Call 审核方式的确认、目标集群容量与搬迁验收仍是现行待办。原报告的 R3 还遗漏了合同价格、业务线密钥与保留策略、适用的健康信息同意流程、上线版本评测等独立事项。清单要求按 `run_id` 关联追踪，具体实现可在部署时决定。真实推送和离线唤醒当前为桩，清单列为建议确认项。生产验收以清单逐项记录的证据为准。 [上线检查清单](../launch-checklist.md)

## 2. 边界与演进方案

### 2.1 B2 / R5、R6 — 来源、展示和可扩展性

旁听原文已在业务线保存。`speaker_ref` 是业务线提供的不透明引用，声纹识别在业务线完成。内核的中文日期提示和固定分页间隔确有本地化限制。平台从可信结构化页生成模型看到的文字，写入前审核内容，并由 Event 与内容块推导来源。让业务线配置 `FromUser` 或 `FromSystem` 映射，会改变 Memory 免审条件，并可能使同一日志随配置变化得到不同的来源判断。出现第二类可信内容时，可按 ADR-0052 重审受信适配器，并由平台继续固定推导来源。 [旁听渲染](../design/captures.md) [ADR-0052](../adr/0052-attested-results-and-provenance.md)

Memory 写入闸门检查来源，`schedule` 的写动作也会根据来源要求 Approval。远程 MCP 的写能力已有默认审批，实际缺口是无法声明复用这种条件审批。若有场景需要扩展，该声明只应增加审批条件。新增 Memory 类别或同意项还须明确跨业务线 Grant、撤回与删除的含义。 [定时写入检查](../../internal/runtime/worker.go) [MCP 审批](../adr/0017-mcp-servers-as-capability-providers.md) [健康类 Memory 同意](../adr/0022-health-memories-behind-business-line-consent.md)

固定 Card 组件集服务于跨端显示一致、写日志前审核及动作校验。达到 ADR-0032 的重审条件时，再用具体组件验证扩展载荷的审核、旧端降级和动作语义。将「领域名词零准入」应用于现行旁听契约，会与 ADR-0052 保留现有命名和校验的决定冲突。 [卡片 ADR](../adr/0032-cards-as-show-card-results.md) [评估 §7](../architecture/architecture-evaluation.html#rules)

### 2.2 B3 / R4 — 上下文策略与外部循环

可替换的上下文策略和流式外部循环已定稿，均待实现。当前有 `context.scope: run`、滚动压缩和单条 `ToolResult` 的模型视图截断，业务侧也能通过公开接口确定性编排多个 Session。实际缺口在于业务线按自己的规则选择历史，以及控制单个 Run 的步骤和模型视图。 [长 Run 设计](../design/m2-long-runs.md) [xinli 编排](../design/xinli-captures.md) [外部循环 ADR](../adr/0041-external-loops-decide-platform-acts.md)

ADR-0056 A 期只在 Run 开始时选择更早的历史并追加文字，结果以 Event 记入日志。B 期与外部循环一起实施。R4 的逐步折叠已消费工具结果超出 A 期范围，涉及请求前缀、摘要对已执行动作的保留，以及完整 Session 的来源判断。建议以一条真实长 Run 对比输入 token、缓存命中、任务效果和重放结果，达到重审条件后单独决策。`worker.go` 的行数可帮助定位维护成本，实施顺序应由具体用例决定。 [ADR-0056](../adr/0056-replaceable-context-strategy-and-streaming-loops.md)

### 2.3 B4 / R7 — Outbox 与分片

外部消费者按物理分片拉取 Outbox，游标绑定该分片的事务号空间，同片长事务会推迟发布。当前的整片物理搬迁只冻结搬迁中的分片，保留槽位表和游标，已在演练中验证。改变槽位映射才需要交接受影响的消费位置。原报告关于全体业务线游标失效并全量重扫的推断取决于迁槽与切换方案。 [出站流契约](../design/m7-kernel-primitives.md#6-出站流outbox) [ADR-0051 扩容路线](../adr/0051-shard-slots-and-id-location.md#54-扩容路线停写与幂等) [整片搬迁演练](../design/deployment.md#105-整片搬迁扩容)

逻辑分区如果进入设计，须说明晚提交的出站流信封如何安全发布、迁槽如何交接续读位置、重复信封怎样识别、删除和保留地板如何延续。近期可先用现有水位和最老事务指标验证长事务影响，在触及 ADR-0051 的容量、停写预算或热点条件时重审。 [出站水位与保留](../design/m7-kernel-primitives.md#64-保留与分片故障) [ADR-0051](../adr/0051-shard-slots-and-id-location.md)

### 2.4 B6 / R9–R12 — 容量和生态

网关按需监听通知频道，一条连接会保留订阅过的频道，长时间运行后监听范围可能扩大。原评审的「数十个网关副本」来自通知量预算。工具结果是否造成存储瓶颈，要测真实事件率、结果大小、WAL 和清理成本。确认瓶颈后再决定通知归属路由、结果外置或预热池。结果外置还要覆盖模型读取、审核与按 Session 删除。 [通知实现](../../internal/pg/notify.go) [原容量估算](./2026-10-11-scalability-review.md) [沙箱验收条件](../adr/0053-self-hosted-sandbox-isolation-baseline.md)

动态注册、按 EndUser 的 OAuth、A2A 和弱网 Call 各有独立需求。R10 可先区分声明加载与发布调整，AgentDef 和 Oneshot 的已加载版本已有约 10 秒一轮的 Release 热加载。内核托管用户凭证适用 ADR-0017 的重审条件，三方 Agent 适用 ADR-0048 的逐项授权。R12 的注入阻断还须满足既定的误报率、时延、评测回归和追加审批指标，当前上线决定为影子模式。 [发布设计](../design/m4-agent-rollout.md) [MCP ADR](../adr/0017-mcp-servers-as-capability-providers.md) [三方 Agent ADR](../adr/0048-third-party-agents.md) [注入判断评测](../design/m6-decisions.md)

## 3. 原报告的数据与表述校正

| 原报告位置 | 建议表述 | 依据 |
|---|---|---|
| 首页扩容与强杀 | 1→4 进程为 `3.67×`，四进程吞吐 `185.6 runs/s`。`188.8 runs/s` 来自后续优化复测。强杀实验共完成 6,812 个 Run，实际由其他进程接管 32 个。开发机使用假模型。 | [M2 压测](../design/m2-scale-test.md#4-结果2026-10-09apple-m4-10-核postgresql-16-运行在-docker-desktop-中) |
| 首页问答首字 | p99 约 53 ms 来自固定首字 50 ms 的假模型。对照实验验证同进程、RPS 不受限时 Oneshot 子限额的效果。 | [首字对照实验](../design/m7-kernel-primitives.md#单次调用子限额下的首字延迟q81) |
| 首页沙箱和搬迁 | 667 ms 为单节点 k3s、镜像已缓存时的 gVisor 冷启动 p95。0.4–0.6 s 为 Mac mini 上 1 GB 和 10 GB 整片搬迁的停写窗口。两项均须在目标集群复测。 | [沙箱复测](../adr/0053-self-hosted-sandbox-isolation-baseline.md) [搬迁演练](../design/deployment.md#105-整片搬迁扩容) |
| §4 快照与 SQL | 投影保留至多三个完整 Run，历史摘要和替代关系仍会积累，旧版本快照失效时可触发完整重放。`83.0→34.1` 条语句来自多个优化的累计测量，原压测把入队和 Event 追加的进一步合并另列。 | [投影](../../internal/session/state.go) [快照](../../internal/session/store.go) [SQL 统计](../design/m2-scale-test.md#7-每个-run-的数据库语句2026-10-11) |
| §3 G1 和 G5 | Go 与移动端支持 Node，Web SDK 只作会话客户端。远程 MCP 幂等性由工具注解与业务线登记约束决定。 | [端侧 SDK ADR](../adr/0026-client-sdks-share-the-contract.md) [MCP ADR](../adr/0017-mcp-servers-as-capability-providers.md) |
| §3 灵活度百分比与 §6 收益数字 | G1–G9 应使用可复用能力、当前限制及接入验收条件。灵活度百分比、代码量节省比例及百万 DAU 目标需要评分规则或目标负载数据。 | [章程成功标准](../charter.md#8-成功标准) [目标环境容量要求](../launch-checklist.md) |

## 4. 建议的验收顺序

| 时机 | 验收场景 |
|---|---|
| 第二条业务线进入同一单元前 | 两条业务线定义同名 AgentDef、Oneshot 和 MCP Server，各自解析与调用。检查指定版本、委托通配、灰度和撤回，确认私有定义仅在所属业务线可见，共享定义按约定开放。跨业务线 Memory 只在既有 Grant 下可读。 |
| 接入服务端异步编排时 | 建 Session 或提交输入成功但响应丢失时重发，并覆盖并发重试、超过 64 条输入后重试和与 Session 删除或 EndUser 注销交错。验证首次结果、Event 和唤醒结果一致。 |
| 提升共用单元流量前 | 两条业务线混合压测，复测启动基线、单线突发和副本变动下的排队与首字延迟，按部署容量确定公平性和准入阈值。 |
| 对真实 EndUser 开放前 | 按上线检查清单逐项记录证据，包含目标集群的沙箱、故障和分片演练。 |
| 出现具体扩展需求时 | 按 ADR-0052、0056、0051 等的重审条件分别决策。提交时依据改动范围验证日志重放、授权与删除、单进程运行、分片和模拟不变量。改变模型视图时，同时核对评测结果和前缀缓存。 |
