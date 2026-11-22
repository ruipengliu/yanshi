# 评审：内核架构评估报告（architecture-evaluation.html）核查与处置建议

> 状态：评审意见（2026-11-22，基于 `23e4dd0`），不是设计，不改变任何不变量 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准 · 评审对象：[architecture-evaluation.html](../architecture/architecture-evaluation.html) · 关联：[章程](../charter.md)、[ADR 索引](../adr/README.md)、[上线检查清单](../launch-checklist.md)、[ADR 评审](./2026-11-14-adr-review.md)
>
> 说明：原评估报告多处使用了 `CONTEXT.md` 约定避免的泛称（如"租户""客户端""消息队列"）。本评审正文按内核术语统一写作 `BusinessLine`（业务线）、`HostApp`、`Outbox` 等，仅在直接引用原报告段落或标签时保留原文。

## 0. 核心结论

[architecture-evaluation.html](../architecture/architecture-evaluation.html)（2026-11-21）从平台基座视角对 `internal/`、`proto/`、`sdk/` 做了一次全面体检，整体问题意识很强，**指出了两类此前 [ADR 评审](./2026-11-14-adr-review.md) 没有覆盖、但在第二条业务线接入时会立刻暴露的真实结构性缺口**；但报告同时存在**事实/指标失准、混淆"刻意克制"与"边界失守"、以及若干整改方案直接碰撞现行内核不变量**三类偏差，不能直接照其路线图（R1–R12）施工。

核心判断归纳为四点：

1. **真正高价值、应优先采纳的发现（B1 前半、B5 前半、B1/B6 的 `model.Guard` 缺陷）**：
   - **BusinessLine 命名空间与权限校验缺位（B1）**：`AgentDef`、`Release`、`Oneshot` 均无 `BusinessLine` 归属与访问校验，一条业务线的 Token 可以创建/调用另一条业务线的 Agent 与 Oneshot，`delegation` 也会跨业务线列出 Agent；`mcpcap` 虽已校验业务线归属，但强制全局唯一 `Server.Name`，会导致跨业务线命名冲突。这部分是**真实的安全与多业务线共单元阻断项**，改动可控、收益极高，建议立刻排入 P0。
   - **异步编排幂等原语不足（B5 前半）**：`CreateSession` 无幂等键、`input_id` 仅留最近 64 条（仅内部委托有永久去重例外），已被 `apps/xinli` 的 `session_creations`、`ScanOrphans` 与 `FindInput` 从头全扫 Event 日志实证为真实摩擦，值得在明确双分片恢复与删除语义后立 ADR 补齐。
   - **`model.Guard` 的限流顺序缺陷**：`rate.wait` 确实在优先级队列 `gate.acquire` 之前抢令牌，且所有模型提供商共用同一组 CLI 限流参数；这是明确的实现缺陷，可独立低成本修复。

2. **被报告夸大或误判的"越界"（B2、B3、B4 的时机）**：
   - 报告把多条**已写明可观测重审条件的刻意克制**（ADR-0052 暂不泛化旁听命名、ADR-0022 暂只启用 `health` Consent、ADR-0032 保持封闭小卡片集、ADR-0056 暂不实现外部循环与上下文策略、ADR-0051 先用 8 片预分 + 整片搬迁）统一归类为"边界失守"或"假通用"。在第二条业务线尚未落地、未触发重审条件之前按想象中的业务形态提前抽空抽象，违背 [ADR-0043](../adr/0043-kernel-persona-app-layers.md)（2026-11-14 修订）的演进纪律。

3. **四项整改方案（R4、R5、R7、R2）存在不变量冲突或关键工程盲区，不能按原文执行**：
   - **R5 撞穿安全与回放不变量**：建议把 `internal/capture` 渲染退回业务侧 MCP、并由业务线映射来源角色，直接违反 [ADR-0052](../adr/0052-attested-results-and-provenance.md)（拒绝业务侧预渲染以防来源洗白与写前审核绕过）与 [ADR-0004](../adr/0004-event-log-as-source-of-truth.md)（按可配置规则解释历史事件会破坏回放确定性）。
   - **R4 撞穿前缀缓存不变量**：建议在活跃 Run 中途对已消费的历史工具结果做选择性折叠，会直接破坏供应商**前缀缓存**（`AGENTS.md` 硬不变量与 [ADR-0056](../adr/0056-replaceable-context-strategy-and-streaming-loops.md) Q3/Q12 刻意禁止的原因）。
   - **R7 低估了跨分片逻辑定序代价**：建议用逻辑虚拟桶与单调逻辑序号替代物理 `shard` + `xid8`，方向有讨论价值（尤其应在第二条业务线接 Outbox 前评估契约是否去物理化），但原文未回答在 [ADR-0007](../adr/0007-postgresql-as-storage-baseline.md)（单分片事务）与 [ADR-0001](../adr/0001-private-deployment.md)（不强引 MQ）约束下，迁槽时如何保证桶内序号严格单调且不引入热点行锁。
   - **R2 忽略了双分片崩溃恢复与 Erasure 键复用**：`Index` 按 `EndUser` 分片、Event 日志按 `Session` 分片，单纯在 `EndUser` 分片建 `external_key` 唯一索引无法原子覆盖半创建崩溃，也未定义 `DeleteSession` / `Erasure` 后如何释放 `external_key` 及新表如何满足 [ADR-0015](../adr/0015-session-deletion-by-physical-removal.md) 删除闭环。

4. **多处 KPI 与代码事实失准，留作仓库参考前须勘误**：
   - 最明显的是页头将压测总 Run 数误写成"`6812 在途 Run 全接管`"（实际被杀进程在途接管为 **32 个**），将 `2000 内存 + 100 单库 PG + 100 分片三方差分` 笼统写成"`2200+ 差分模拟种子`"，以及多处 `file:line` 引用错误、遗漏已修复项（如 `memgate` Unicode 修复、`schedule` 污染闸门、`releases.yaml` 热重载）。本项目主要由 Agent 开发，文档中的不准确事实会直接误导后续会话，建议先勘误或加注指向本评审。

---

## 1. 事实、代码引用与 KPI 勘误

以下逐条对照截至 `23e4dd0` 的源码与文档核查，区分"KPI 失实/缺前提"与"源码描述/行号偏差"。

### 1.1 页头 KPI 与 §4 核心优势中的指标勘误

| 原报告位置与表述 | 核查结论 | 仓库实际依据与缺失前提 |
|---|---|---|
| 页头 KPI：`6812 在途 Run 全接管`（L671） | **失实（概念误植）** | [m2-scale-test.md:78](../design/m2-scale-test.md#L78)：6812 是强杀压测中**全程完成的总 Run 数**；被杀进程当时正在执行的**在途 Run 为 32 个**，全部被其他进程接管。 |
| 页头 KPI：`3.67×` 与 `188.8 runs/s` 并列（L669-671） | **混用两次测试** | [m2-scale-test.md:58-68,99](../design/m2-scale-test.md#L58-L68)：`3.67×` 对应 `50.5 → 185.6 runs/s`（Apple M4 + Docker Desktop PG + 200ms 假模型）；`188.8 runs/s` 是后续加 `live.Follower` / `HeadHinter` 后的复测；开启计量配额后约为 `177 runs/s`（[m4-quota-usage.md:145](../design/m4-quota-usage.md#L145)）。 |
| 页头 KPI：`53 ms` 问答 TTFT p99（L674） | **缺关键前提** | [m7-kernel-primitives.md:514,530-536](../design/m7-kernel-primitives.md#L514)：该数字来自**单进程内存存储 + 50 ms 固定 TTFT 假模型**（`-model-rps=0`）的混合负载隔离测试，只说明内核自身开销约 3 ms，不是真实模型与 PG 下的问答 TTFT。 |
| 页头 KPI：`667 ms` gVisor 冷启动 p95（L674） | **数值准确，缺前提** | [ADR-0053:79](../adr/0053-self-hosted-sandbox-isolation-baseline.md#L79)、[launch-checklist.md:16](../launch-checklist.md#L16)：基于单节点 k3s + 预缓存镜像；目标生产集群与首次拉镜像实测尚未完成。 |
| 页头 KPI：`0.4–0.6 s`（§4 `573 ms`）10GB 在线搬迁停写窗口（L679, L991） | **数值准确，缺范围** | [ADR-0051:127](../adr/0051-shard-slots-and-id-location.md#L127)、[deployment.md:319](../design/deployment.md#L319)：为 Mac mini 本地演练的**整片物理流复制提升**耗时，不含目标生产环境 HA 控制器隔离与代理改指时间（[launch-checklist.md:41](../launch-checklist.md#L41) 仍未勾选）。 |
| 页头 KPI / §4：`2200+ 差分模拟种子`（L681, L983） | **范围夸大** | `Makefile:103` 为 `-sim.seeds=2000 -sim.pgseeds=100 -sim.shardseeds=100`：其中 2000 组仅跑纯内存模拟，100 组跑单 PG vs 内存，**真正做三方差分（单库内存 / 双库内存 / 双库 PG）的是 100 组**，不是 2200+ 组。 |
| §4 S1：`内存仅保留最近 4 个完整 Run`（L965） | **数值错误** | `internal/session/state.go:235,384` 定义 `KeepRuns = 2`，投影中至多保留 `KeepRuns + 1 = 3` 个完整 Run，不是 4 个。（`ProjectionVersion = 27` 准确，见 `internal/session/snapshot.go:18`。） |
| §4 S1：`CommitAndWake ... 每 Run SQL 从 83 条降至 34.1 条`（L966） | **归因不准确** | [m2-scale-test.md:108-112](../design/m2-scale-test.md#L108-L112)：单条合并写入把 SQL 从 `83.0` 降至 `45.7`；从 `45.7` 降至 `34.1` 来自 `service.WithAccess`、`usage.SpentCache` 与 `lifecycle.TouchEvery` 的叠加优化，并非仅靠 `CommitAndWake`。 |
| §4 S6：`实测长 Run 缓存命中率从 40% 提升至 70%`（L1006） | **缺范围** | [m2-long-runs.md:207](../design/m2-long-runs.md#L207)：特指评测用例 `long-run-ledger` 的单用例实测（`40% → 69%–70%`）。 |

### 1.2 代码实证（§2 / §5 / §6）中的事实偏差与行号勘误

| 原报告位置与表述 | 核查结论 | 源码实际情况 |
|---|---|---|
| B1 引用 `release.go:69-81` 与 `agentdef` 混写（L1030） | **路径省略致歧义** | `internal/agentdef/release.go` 不存在；实际分属 `internal/agentdef/releases.go:24-69` 与 `internal/release/release.go:69-81`。核心结论（无 `BusinessLine` 隔离）成立。 |
| B1：`oneshot` 全局争抢、无业务线隔离（L1031） | **遗漏已有层** | 声明注册表与 `"name:"+d.Name` 槽锁确无 `BusinessLine` 隔离（`internal/oneshot/registry.go:37-51`、`service.go:119-123`）；但 `oneshot.Service` **已经支持**按业务线的总并发上限 `BusinessLineMax`（`businesslines/*.yaml` 的 `oneshots.max_concurrency`，`internal/auth/auth.go:86`）。 |
| B1：`mcpcap` 租户隔离（L1031） | **需澄清边界** | `mcpcap.LoadDir`（`mcpcap.go:67-80`）与 `Connector.conns`（`mcpcap.go:158`）确实强制 `Server.Name` 全局唯一；但 `mcpcap.Server` **已有必填 `BusinessLine` 字段**，`Tools()`、`CiteCaptures`、`AttestsSpeakers` 均严格按 `s.BusinessLine == businessLine` 隔离（`mcpcap.go:36,102,267-272,354,378`），**不存在跨业务线越权调用 MCP 工具的问题**。 |
| B1：`pgqueue.go:126` 仅全局 FIFO（L1032） | **遗漏已有原语** | `ORDER BY priority DESC, ord` 确无公平调度；但 `work_items` 表**已存 `business_line`**，`pgqueue.Claim`（`pgqueue.go:125`）与 `workqueue.Pool.BusinessLines`（`workqueue.go:93`）已在存储层支持按业务线过滤认领，只是 `cmd/yanshi/serve.go:548-557` 未暴露配置开关。 |
| B1 引用 `model/guard.go:106-132` 讨论 `rate.wait` 顺序（L1032） | **行号偏差** | `106-132` 只是 `admissionClass` / `classOf` 定义；`rate.wait` 先于 `gate.acquire` 抢令牌的实际位置在 `internal/model/guard.go:204-219`。另外，每个提供商运行时拥有独立的 `Guard` 实例（`cmd/yanshi/serve.go:197-223`），问题在于启动参数仅提供一套共享的 `modelLimits`（`:303-310`）。 |
| B2：`internal/capture/capture.go:63-120` 硬写 5 分钟交谈间隔（L1051） | **文件/行号错误** | 中文星期与 `"之后七天"` 在 `capture.go:63-79`；5 分钟间隔 `DefaultExchangeGap` 实际在 `internal/capture/page.go:20`，且仅作为默认分页参数经 `_meta["yanshi/page_options"]` 传给业务线。 |
| B2：`mcpcap/cite.go:18-114` 递归扫描任意参数全日志回扫（L1051） | **遗漏触发门控** | 机制属实（`cite.go:34-103`），但**仅对显式声明 `cite_captures: true` 且同业务线的 MCP Server 生效**（`mcpcap.go:48,354-366`），且参数不含 `#N.M` 引用时不读日志（`cite.go:74`）。 |
| B2：`health.go:13-41` 硬编码中文健康词表（如"心理咨询师"）、18 位身份证正则与 `Asia/Shanghai`（L1052） | **多处细节错误** | ① `"心理咨询师"` 在 `health.go:28` 的 `healthExcepts` 中是**排除词**（避免职业误报），触发词是 `"心理咨询"`（`:21`）；② 18 位身份证正则 `idCardRE` 与 `Asia/Shanghai` 不在 `health.go`，而在 `internal/memory/service.go:41-42,75-108,129-142`；③ `Service.Location` **可配置**，`Asia/Shanghai` 只是零值默认。 |
| B2：`memgate.go` 仅保护内置 `memory_save`（L1052） | **部分失实** | `tainted(w.st)` 同样保护实现 `capability.ApprovalSummarizer` 的本地能力——当前包含 `schedule`（`internal/runtime/worker.go:783-791`、`internal/service/schedule_capability.go:111-130`）。此外，按 [ADR-0017](../adr/0017-mcp-servers-as-capability-providers.md)，未声明只读的 MCP 工具**默认无条件需要审批**（`mcpcap.go:302-316`），真正的细粒度缺口只是：被 `tools.no_approval` 豁免的 MCP 写工具无法声明"仅受外部污染时审批"。 |
| B3：业务线在 `AgentDef` 中只能调窗口大小与压缩比例（L1072） | **轻度低估** | `internal/agentdef/agentdef.go:80-94` 还支持 `scope: run`（单 Run 隔离）、`max_tool_result`（单条工具结果截断）、`keep_recent`、`summary_model`。 |
| R2：`突破内存 64 条环形窗口上限`（L1250） | **表述不准确** | `MaxRecentInputs = 64`（`internal/session/state.go:470`）是**持久化 Event 日志投影并写入 `SessionSnapshot`** 的状态（`snapshot.go:102-104`），进程重启不丢失；限 64 条是为了让快照大小保持 $O(1)$，而非易失内存。 |
| B5：`新增/修改 AgentDef、MCP、Oneshot 或调整并发配额均需改挂载文件重启`（L1116） | **遗漏热重载** | 静态 YAML 新增/改写与 CLI 并发参数确实需重启；但已有版本的灰度与撤回配置 `releases.yaml` **已支持每 10 秒热轮询加载，无需重启**（`cmd/yanshi/serve.go:519,524`、`internal/release/release.go:101-128`）。 |

---

## 2. 六大问题（B1–B6）逐项深度评审

### B1：多业务线命名空间、权限校验与跨业务线公平调度

- **事实成立度**：**高度成立**（本次评估价值最高的新发现）。
- **两层拆分评判**：
  1. **安全与命名空间隔离（必须立即补齐，P0）**：
     - 现有 Session 级 API 已通过 `service.WithAccess` 严格校验 `(BusinessLine, EndUser)` 归属（`internal/service/service.go:226`），但**定义层（`AgentDef`、`Release`、`Oneshot`）与 `mcpcap` 名称索引漏掉了 `BusinessLine` 作用域**。
     - 尽管跨业务线调用 `AgentDef` 时新建的 Session 仍落在调用方自己的 `BusinessLine` 下（不会读到对方业务线的 Session、Memory 或 MCP 数据），但仍会**泄露对方 AgentDef 的指令、模型策略、内置能力配置，且 `releases.yaml` 全局撤回与 `oneshot` 并发槽锁会互相干扰**；`mcpcap` 的全局唯一 `Server.Name` 也会在两条业务线各自注册如 `crm` / `persona` 同名 MCP 时直接冲突。
     - 这类缺口属于内核既有治理职责的明显遗漏，按 [ADR-0043](../adr/0043-kernel-persona-app-layers.md) 2026-11-14 修订，**无需等待第二条业务线实际落地即可直接修复**。
  2. **`model.Guard` 限流顺序缺陷（必须立即修复，P0）**：
     - `internal/model/guard.go:204-219` 中 `g.rate.wait` 放在 `g.gate.acquire` 之前，在触发 RPS 限流时后台请求会与交互问答平权抢令牌，直接削弱 [ADR-0029](../adr/0029-work-classes.md) 的快慢分离保护。调整顺序（并支持按提供商分别配置并发/RPS）属于低风险、高收益的 bug 修复。
  3. **跨业务线公平调度（问题真实，但 R8 方案过重，建议先简后繁，P1）**：
     - 共享单元下确实存在"吵闹邻居"风险，但 R8 一上来就在 PostgreSQL `workqueue.Claim` 的 `SELECT ... FOR UPDATE SKIP LOCKED` 内引入 DRR/WFQ，以及引入跨副本动态配额协调中心，复杂度和 PG 热点锁风险极高。
     - **更务实的渐进路径**：① 先把已实现于 SQL 层的 `workqueue.Pool.BusinessLines` 暴露为 `serve` 配置（大业务线或批量任务业务线可独立分配 Worker 池，零 SQL 改造）；② 借鉴 `oneshot.BusinessLineMax`，在 `model.Guard` 中增加按 `BusinessLine` 的软并发占比上限（单进程内即可做，保持单二进制可跑）；③ 待多业务线真实共池压测出现调度饥饿时，再按 [ADR-0029](../adr/0029-work-classes.md) 的重审条件（"认领类别多到需要动态权重或公平调度"）升级公平队列。

### B2：首条业务线语义下沉内核

- **事实成立度**：**代码现象基本存在，但归因夸大，且 R5 整改方案直接违背现行安全不变量**。
- **须把报告混为一谈的三类问题拆开看**：
  1. **不应归类为"首条业务线泄漏"的通用克制（维持现状）**：
     - **Card（`internal/card`）**：5 类元素、6 种表单控件、23 个通用图标（`alarm`/`shopping`/`car`/`weather` 等）是通用的 C 端结构化交互原语，并非某条业务线的领域概念。[ADR-0032](../adr/0032-cards-as-show-card-results.md) 明确拒绝开放任意扩展组件，以守住三端 SDK 一致性、内容安全审核（`card.Text()`）与禁止外部追踪图片链接；重审条件清晰——"固定组件集外的需求超过两次"。**反对 R6 现在开放 `custom_component { type, version, data_json }`**，因为非结构化 JSON 载荷一旦进入卡片，内核无法保证审核覆盖与防追踪，等于绕开 ADR-0032。
     - **Memory 6 分类与 Consent 仅 `health`**：固定分类是为了跨业务线 `Grant` 时 EndUser 能理解自己在授权什么（[ADR-0016](../adr/0016-memory-recall-as-event-and-category-grants.md)）；`consent.Store` 与数据库表 `end_user_consents` 已按 `kind` 参数化预留扩展位（[ADR-0022](../adr/0022-health-memories-behind-business-line-consent.md) 2026-11-19 修订）。在出现第二类需要单独同意的敏感信息或第二条业务线之前，没有必要提前做成动态注册体系。
  2. **确实带有旁听/中文印记、但已被 ADR 明确暂缓泛化的部分（严格按重审条件走，否决 R5 核心改法）**：
     - `CaptureSegment`、`SPEAKER_WEARER/OTHER`、`speaker_ref`、`internal/capture` 中文日期头、`cite_captures` 的确源于旁听场景。但 [ADR-0052](../adr/0052-attested-results-and-provenance.md)（以及 [2026-11-14 ADR 评审](./2026-11-14-adr-review.md) §2.2、§4.1）已经审慎权衡并拍板：**在第二类可信结构化内容出现前，不借泛化之机改名或重做抽象**。
     - **强烈反对 R5 将渲染退回业务侧 MCP 及开放业务线声明角色→来源映射**：
       - ADR-0052 §3 与 Considered Options 已明确否定由业务侧 MCP 返回渲染文本：这会把受信任的平台框架文字与不可信的外部内容混在同一文本流中，破坏写前审核、原文逐字保真与来源隔离。
       - ADR-0052 §4 与 `2026-11-14-adr-review.md` §2.2 已明确否定由业务线配置角色到 `Provenance` 的映射：一来放宽 `memory_save` 免审边界，二来配置一旦变更，重放旧 Event 日志会得到不同的 `Provenance`，直接破坏 [ADR-0004](../adr/0004-event-log-as-source-of-truth.md)。
     - 至于 `internal/capture` 中的中文相对日期提示与 `memory/health.go` 的中文词表，若未来接入多语种业务线，可在保留平台渲染与封闭来源映射的前提下，按 locale 模板化（触发 [ADR-0023](../adr/0023-memory-write-gate.md) / [ADR-0052](../adr/0052-attested-results-and-provenance.md) 重审条件时再做）。
  3. **值得吸收的轻量安全增强（独立于泛化）**：
     - 允许业务线在 `mcpcap` 的 `tools.no_approval` 配置中，为特定写工具选择 `taint_gated: true`（正常对话免审，但一旦 Session 接触过外部/他人内容即收紧为需要审批）。该能力**只收紧不放宽**，复用现有 `tainted(w.st)` 与 `ApprovalSummarizer` 机制，不碰任何核心不变量。

### B3："薄 Harness"未落地，循环与上下文锁死在内核

- **事实成立度**：**现状描述准确**（ADR-0041、ADR-0056、M6 工具折叠确实尚未编码，`worker.go` 达 1642 行），**但优先级判断偏急，且 R4 中"Run 内选择性折叠历史工具结果"直接破坏前缀缓存不变量**。
- **具体评判**：
  - `internal/runtime/worker.go` 拆分为职责更清晰的子文件（如按步骤阶段拆分）对"Agent 可开发"（章程原则 7–8）有益，可作为常规工程整洁任务，但必须保持 `Worker.Step` 确定性单步同步路径，不能破坏 `make sim` 故障注入粒度。
  - ADR-0056 A 期（上下文策略）与 B 期（流式外部循环）已于 2026-11-16 定稿，明确写明**"等业务线提出需要时实施"**。在没有真实 SOP 型或重度上下文定制业务线验证协议细节前，提前编码落地的风险是造出无人使用的协议。
  - **否决 R4 在现阶段扩展"活跃 Run 内历史工具结果折叠"**：
    - `AGENTS.md` 明确将**"请求前缀保持稳定（Run 内的上下文只在尾部追加）"**列为核心不变量，也是长 Run 成本的最大杠杆。
    - [ADR-0056](../adr/0056-replaceable-context-strategy-and-streaming-loops.md) Q3 与 Q12 正是为此才把 A 期刻意限定在"每个 Run 开始时、按整 Run 粒度选择"，避免中途改写前文导致每轮全部打穿供应商前缀缓存，也避免摘要改写破坏来源推导。
    - 当前已有 `max_tool_result` 截断与复用主前缀的 Compaction 兜底；未来若某长任务业务确实因 Run 内工具结果膨胀导致成本不可接受，应先触发 ADR-0056 重审条件，并通过 `make eval` 的"缓存命中"与总成本双指标验证净收益，而不是直接塞进 R4。

### B4：Outbox 暴露物理分片与 `xid8`

- **事实成立度**：**代码机制完全成立**（`/v1/outbox?shard=N`、`/v1/outbox/shards` 暴露 `0..1023` 槽位与物理 `shard`、游标绑定单机 `xid8`、同分片共用 `pg_snapshot_xmin` 水位）。
- **核心评判（这是本次评估中**最值得提前思考契约演进**的长期架构议题）**：
  - 必须先补全报告淡化的背景：2026-11-21 刚完成的 [ADR-0051](../adr/0051-shard-slots-and-id-location.md) 预分 8 片 + 整片物理流复制搬迁（`make shard-move`）在 8 片容量内**不会破坏 `xid8` 游标**，这是为了严守 [ADR-0001](../adr/0001-private-deployment.md)（不引入 Kafka/MQ）与快速上线做出的清醒权衡，[ADR-0046](../adr/0046-business-line-outbox.md) 也已把 Outbox 标为"只在一条业务线上验证过"。
  - **但报告指出的长期锁定效应非常真实**：Outbox 是对外 HTTP/游标契约。如果今天接入 1 条业务线（`xinli`），改契约代价还小；如果未来接入 5 条业务线，每条都在业务库里存了按物理 `shard=0..7` 索引的游标，再想扩至 16/32 片或拆热点槽位，停写与全业务线重扫的协调成本会成倍放大。
  - **对 R7 方案的技术修正**：
    - R7 提出的"按固定逻辑虚拟桶（例如 64 个虚拟分区，每桶固定对应 16 个槽位，由网关路由到承载该槽位段的物理分片）对外暴露，屏蔽物理 `shard` 号"**方向正确且轻量**——因为每个虚拟桶始终整体落在单个物理分片内，天然兼容单分片查询，且迁槽时只要以虚拟桶为最小搬迁单位，消费方甚至不感知物理库数量从 8 变到 16 或 64。
    - **但 R7 未解决的核心难点是游标序号与跨库延续**：去掉 `xid8` 换用"桶内单调逻辑序号"并不白给——如果用 PG `SEQUENCE` 或每桶计数行，并发事务提交乱序仍可能跳号（导致漏读未提交的低序号行），还是需要某种提交可见性屏障（`xmin` 或显式发布者）；而如果在迁槽停写窗口内把旧库该虚拟桶的最大已提交序号作为新库起点，理论上可以保持逻辑序号单调且游标不断，但必须在独立 ADR 中严谨推导并经 `make sim` 验证（包括回滚、备份恢复、慢事务阻塞 `xmin`）。
    - **运维侧立即可做**：在 PG 参数与部署清单中落实 [m7-kernel-primitives.md §6](../design/m7-kernel-primitives.md#L268) 建议的 `statement_timeout`、`idle_in_transaction_session_timeout`、`max_prepared_transactions=0`，并将只读分析/备份全部走备库，即可立刻大幅缓解跨业务线慢事务卡死 `xmin` 水位的风险。

### B5：会话幂等、业务索引与动态配置原语缺失

- **事实成立度**：**前半（幂等缺口）高度成立且已被 `apps/xinli` 实证；后半（静态配置）对现阶段属合理取舍**。
- **具体评判**：
  1. **建 Session 幂等（`external_key`）与持久 `input_id` 去重（支持立 ADR，但 R2 方案必须补齐三大设计缺口）**：
     - `apps/xinli/orchestrator/work.go`、`orphans.go` 与 `apps/xinli/kernel/inputs.go:68` 已经证明：任何需要可靠异步编排的业务线都会撞上这两堵墙，属于典型的"第二条业务线也会需要、业务侧做起来既复杂又脆弱"的内核原语（完全符合 [ADR-0043](../adr/0043-kernel-persona-app-layers.md) 判据）。
     - **但 R2 原文把实现写得过于轻巧，真正设计 ADR 时必须解决三个硬问题**：
       - **双分片崩溃恢复**：当前 `service.Create`（`internal/service/service.go:167-187`）先写 `EndUser` 分片的 `Index`，再写 `Session` 分片的 `events`。若 `external_key` 唯一键落在 `EndUser` 分片，而写完 `Index` 后崩溃、尚未写入 `SessionCreated`，后续带同一 `external_key` 重试时，不能直接返回指向空日志的 `session_id`，而须利用 `Index` 中已绑定的确定性 `session_id` 续建 `SessionCreated`（复用内部 `ErrConflict` 幂等路径）。
       - **删除与 Erasure 后的键生命周期**：当一个带 `external_key` 的 Session 被 `DeleteSession` 或 `Erasure` 删除后（例如 `xinli` 擦除当日工作 Session 后重新创建当日 Session），必须明确 `external_key` 是随逻辑删除标记释放、允许重新绑定新 Session，还是永久占用；若不释放，擦除将直接卡死业务重跑。
       - **物理删除与隐私合规闭环（[ADR-0015](../adr/0015-session-deletion-by-physical-removal.md)）**：新增的 `session_inputs` 去重表必须按 `Session` 分片登记在 `internal/pg/shards.go`（`pg.Tables`）与 `app.SessionData`，写入入口执行 `lifecycle.Recheck`，并纳入 `internal/sim` 删除不变量与一致性套件；同时在契约中约定 `external_key` / `input_id` 为不透明标识，不承载用户敏感原文。
  2. **轻量业务 `labels` 查询**：建议保持克制，先看实际过滤需求（如仅按 `agent` 过滤或按 `external_key` 精确查找往往已足够），避免在 `EndUser` 分片的大表上引入任意 JSONB/KV 索引膨胀。
  3. **动态注册中心（R10）与用户级 OAuth**：
     - 目前 `releases.yaml` 已支持 10 秒热更新，足以覆盖日常灰度与秒级回滚；在业务线达到个位数前，GitOps + CI 评测门禁（`make eval`）发布 `AgentDef` YAML 反而比急建一套带鉴权审计的控制面数据库更可靠、更贴合"评测即基础设施"（章程原则 10）。
     - 用户级 MCP OAuth 已在 [2026-11-14 ADR 评审](./2026-11-14-adr-review.md) 附录 C（第三轮）专门复核：业务线自有 MCP Server 已通过 `X-Yanshi-End-User` 自行保管用户令牌，只有"业务线无法代理的纯三方 MCP"才需要内核托管，维持 [ADR-0017](../adr/0017-mcp-servers-as-capability-providers.md) 重审条件即可。

### B6：单进程局部协调态、大事件内嵌与生产外联桩

- **事实成立度**：**代码现状均属实，但把"上线硬阻断项"与"10 万+ DAU 规模优化项"混为一谈**。
- **分层处置**：
  1. **上线硬阻断项（已由 [launch-checklist.md](../launch-checklist.md) 完整跟踪）**：真实多模态 `Moderation` 容灾链、流式草稿/Call 审核合规确认、真实 `Pusher`/`Waker`、合同价格表、脱敏 OTel Trace、目标 K8s + Patroni + gVisor 集群实测。这部分直接按既有上线检查清单收口，无需重复建项。
  2. **高规模优化项（保留为指标触发项，不做提前优化）**：
     - **网关通知归属与 `UNLISTEN`**：`internal/pg/notify.go:74-76` 明确注释不做 `UNLISTEN` 是为了防止取消订阅间隙丢通知导致 `latest` 缓存失真；强行改动极易引入隐蔽的实时流卡死 bug，应等网关副本压测显示 PG `NOTIFY` 成为瓶颈后再专项设计并过 `make sim`。
     - **大 `ToolResult` 自动外置与 `events` 分区**：已在 [2026-11-14 ADR 评审](./2026-11-14-adr-review.md) §4.2 明确归为容量触发问题，且必须保证外置对象同样过内容安全审核、支持回放读取、随 Session 物理删除，且不得削弱 `Erasure` 语义。
     - **K8s 沙箱预热池与 exec 通道**：[ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 已设可观测重审阈值（目标集群冷启动 p95 > 3s 或 apiserver 成为 exec 瓶颈）；单节点 k3s 实测冷启动 p95 仅 667 ms、30 路并发 exec p95 208 ms，当前远未触发。
     - **单进程限流/配额协调**：上线初期可通过"账号上限 ÷ 副本数"配合部署模板自动注入解决（[launch-checklist.md:32](../launch-checklist.md#L32)），待 HPA 频繁弹性伸缩导致额度浪费或超限时，再引入基于控制库或轻量租约的副本数感知配额分配（注意仍需保单二进制零外部依赖）。

---

## 3. 路线图（R1–R12）与准入标尺（§7）逐项裁决

### 3.1 R1–R12 处置矩阵

| 编号 | 原优先级 | 评审裁决 | 建议阶段 | 关键修正与风险说明 |
|---|---|---|---|---|
| **R1** | P0 | **采纳（并合入 `model.Guard` bug 修复）** | **立即（P0）** | 补齐 `AgentDef`/`Release`/`Oneshot`/`MCP` 的 `(BusinessLine, Name)` 作用域与访问校验（覆盖 `service.Create`、`delegation.Directory`、`httpapi/oneshot`、`oneshot` 槽锁、`mcpcap.LoadDir` 与 `conns`）；同步修正 `model.Guard` 中 `rate.wait` 先于 `gate.acquire` 的顺序缺陷，支持按提供商分别设限。 |
| **R2** | P0 | **调整范围后采纳** | **近期立 ADR（P1）** | 聚焦 `CreateSession` `external_key` 幂等与持久 `session_inputs` 去重（通用 `labels` 查询暂缓）；ADR 须写明双分片半创建续建、`DeleteSession`/`Erasure` 释放 `external_key` 及 [ADR-0015](../adr/0015-session-deletion-by-physical-removal.md) 删除闭环。 |
| **R3** | P0 | **采纳（合并至上线清单）** | **立即（P0）** | 直接按 [launch-checklist.md](../launch-checklist.md) 推进，避免维护两套并行清单。 |
| **R4** | P1 | **拆解：重构可做、ADR-0056 延后、Run 内折叠否决** | **按需 / 延后** | `worker.go` 按阶段拆文件可作代码整洁重构；ADR-0056 A/B 期等真实业务需求再实现；**明确否决**在 A 期加入活跃 Run 内历史工具结果折叠（会打穿前缀缓存）。 |
| **R5** | P1 | **大部分否决，仅采纳 `taint_gated` 一项** | **轻量项 P1 / 其余延后** | **否决**把 capture 渲染退回业务 MCP 及开放来源映射配置（违背 ADR-0052、ADR-0004）；Memory 分类、健康词表、Consent 扩展维持既有 ADR 重审条件；**采纳**为 `no_approval` MCP 写工具增加可选 `taint_gated: true` 条件审批。 |
| **R6** | P1 | **拆解：卡片自定义否决、服务端 SDK 延后** | **第二条业务线接入时** | **反对**开放 `Card` `custom_component + data_json`（维持 ADR-0032 审核与三端一致边界）；轻量服务端辅助 SDK 可在第二条业务线接入时沉淀，不必提前造壳。 |
| **R7** | P1 | **方向有价值，但须重做存储设计后评估** | **第二条业务线接 Outbox 前** | 趁只有 `xinli` 一条业务线消费 Outbox，评估逻辑虚拟桶契约是否值得前置以切断物理 `shard` 绑定；但必须先出独立 ADR 解决迁槽时桶内单调序号与无热点锁问题，不能简化为一句话改造。慢事务防护参数可立刻落进运维基线。 |
| **R8** | P1 | **降级收敛为两步走** | **P0 修序 / P1 池化 / P2 公平调度** | `model.Guard` 顺序与按提供商分限前移至 R1；近期先暴露已有的 `workqueue.Pool.BusinessLines` + 单进程 per-`BusinessLine` 模型并发软上限；PG 内 DRR/WFQ 与跨副本全局协调留待多线共池压测触发。 |
| **R9** | P1 | **延后至容量/压测触发** | **P2（指标触发）** | 网关通知归属、大 `ToolResult` 外置、`events` 分区、K8s 预热池均以目标集群压测与容量阈值触发，不抢占上线前资源。 |
| **R10** | P2 | **延后** | **P2** | 多业务线高频迭代前维持 GitOps + `releases.yaml` 10s 热加载；用户级 OAuth 按 ADR-0017 重审条件触发。 |
| **R11** | P2 | **延后** | **P2** | 维持 [ADR-0048](../adr/0048-third-party-agents.md) 既定排期。 |
| **R12** | P2 | **维持（私有化推理前置到首个客户交付前）** | **P2 / 交付前置** | 开源推理引擎（vLLM/SGLang）适配随首个私有化交付立项；Opus/RTC 维持 ADR-0024/0027 放开的口子与重审条件；中文注入分类器按 [m6-decisions.md §9](../design/m6-decisions.md) 条件复测。 |

### 3.2 六条内核准入标尺（§7）评审与修正

原报告 §7 提出的六条标尺多数可直接吸收，但需修正两条并补充一条缺失的核心不变量：

- **直接采纳**：标尺 2（BusinessLine 复合键强制测试）、标尺 4（治理不可绕过、策略可替换）、标尺 5（外部动作幂等测试）、标尺 6（单进程可跑与差分模拟不退化）。
- **修正标尺 1（领域名词零准入测试）**：过于绝对，易诱导 Agent 违反 [ADR-0052](../adr/0052-attested-results-and-provenance.md) 提前做无验证支撑的改名或空抽象。建议改为：**"新机制进入内核须满足 ADR-0043 判据并登记单线状态与重审条件；既有已评审的过渡命名在触发重审条件前不得私自泛化改名。"**
- **修正标尺 3（物理拓扑零暴露测试）**：作为新契约准入门禁成立；但须注明现行 `Outbox` 是 [ADR-0046](../adr/0046-business-line-outbox.md) / [ADR-0051](../adr/0051-shard-slots-and-id-location.md) 登记在案的阶段性妥协，在专立 ADR 迁移契约前不得以此阻塞日常维护。
- **补充第 7 条标尺（原报告遗漏）**：**前缀缓存稳定与确定性回放测试（Cache & Replay Invariance Test）**——任何涉及事件结构、来源推导、上下文拼装或工具结果外置的内核改动，都必须保持 [ADR-0004](../adr/0004-event-log-as-source-of-truth.md) 历史日志确定性重放不改变结果，且经 `make eval` 确认长 Run 请求前缀缓存命中率无退化。

---

## 4. 原报告的三个架构盲区

除核查报告已列条目外，结合 `yanshi` 当前代码与上线路径，还发现三个报告未覆盖、但与"多业务线灵活支撑及边界清晰度"直接相关的问题：

1. **多业务线共单元下的 `EndUser` 身份空间契约未显式校验**：
   - [2026-11-14 ADR 评审](./2026-11-14-adr-review.md) §2.3 已修订部署单元定义为"一组共享 EndUser 身份与 Memory 的业务线（默认同一单元）"。但在代码侧，令牌由各业务线分别签发（[ADR-0014](../adr/0014-business-line-signed-tokens.md)），内核并不验证两条业务线签出的 `end_user` 是否来自同一公司级账号体系。如果第二条业务线使用独立的本地用户 ID（如自增整数 `"10001"`）与第一条业务线撞号，一旦开启跨线 Grant 就会发生严重的跨人数据串号。应在 `businesslines/*.yaml` 与接入规范中显式声明 `EndUser` 身份域（或禁止未共域的业务线互相 Grant）。
2. **多业务线场景下的 `evals/` 评测资产治理（章程原则 10）**：
   - 报告讨论了多业务线静态部署 `AgentDef` 的运维瓶颈（B5/R10），却遗漏了同样耦合在内核仓库的 `evals/`。当前 `make eval` 基线与用例集中在同一仓库，未来接入多条业务线后，必须划分**内核能力通用回归集**（换模型、改压缩/闸门/委托时必跑）与**业务线独立评测集**（由业务线各自维护基线、作为其 AgentDef 灰度准入条件），避免内核团队被迫为十条业务线的 Prompt 改动维护基线。
3. **重架构重构与首条业务线上线闭环的节奏冲突**：
   - 当前 [launch-checklist.md](../launch-checklist.md) 仍有 10 余项上线前置未闭环（真实审核、合规确认、合同价、目标 K8s 集群压测与 10GB 搬迁实测等）。此时如果同时铺开 R4–R9 六项大型内核改造，会显著拉长首次真实生产验证周期；而真实 C 端流量跑出来的问题（延迟、审核误杀、Token 成本分布、慢事务形态）往往比纸面推演更能校准后续优先级。

---

## 5. 建议处置顺序与待负责人确认事项

本文只提出分析与建议，不改动代码或 ADR。建议负责人逐项拍板后再分批立项实施：

**文档处置（先清误导）**
1. 决定对 [architecture-evaluation.html](../architecture/architecture-evaluation.html) 的处理方式：建议在其页首补注本评审链接并修正 §1.1–§1.2 中的硬性事实错误（尤其是 `6812` 在途 Run、`2200+` 差分种子、`KeepRuns`、`memgate` 与 `mcpcap` 隔离现状），避免后续 Agent 将失实数字当作代码基线。

**第一批：立即立项（P0，安全补洞与上线阻断，不改核心不变量）**
2. **R1（命名空间与权限校验）**：为 `AgentDef`、`Release`、`Oneshot` 增加 `BusinessLine` 作用域并在 `service.Create`、`delegation.Directory`、`httpapi/oneshot` 与 `oneshot` 槽锁强制隔离；`mcpcap` 改为按 `(BusinessLine, Name)` 建键，消除跨业务线命名冲突。
3. **`model.Guard` 限流修复**：调整 `rate.wait` 与 `gate.acquire` 顺序（保护高优先级问答不被后台 RPS 抢占），并支持按提供商独立配置并发与 RPS 上限。
4. **R3（对齐上线检查清单）**：继续按 [launch-checklist.md](../launch-checklist.md) 完成真实审核、合规确认、推送唤醒、脱敏链路追踪与目标 K8s 集群压测验收。

**第二批：先立 ADR 设计，经评审后再编码（P1，面向第二条业务线接入前）**
5. **R2（收敛版）**：立 ADR 设计 `CreateSession` 的 `external_key` 幂等键与持久 `session_inputs` 去重（须写清双分片半创建恢复、删除/擦除后键复用、[ADR-0015](../adr/0015-session-deletion-by-physical-removal.md) 删除登记与 `make sim` 验证）；通用 `labels` 暂不纳入首期。
6. **Outbox 契约去物理化评估（R7 前置决策）**：在第二条业务线正式消费 Outbox 之前，出一份精简 ADR 评估"固定逻辑虚拟桶 + 迁槽序号延续"方案与现行物理分片契约的成本收益，拍板是否切换契约；同时立即把防长事务 PG 参数纳入生产部署清单。
7. **轻量跨业务线隔离（R8 收敛版）**：暴露 `workqueue.Pool.BusinessLines` 配置，并评估单进程 `model.Guard` 按 `BusinessLine` 软并发占比保护；PG 内 DRR/WFQ 与跨副本全局配额留待压测触发。
8. **可选安全增强（R5 唯一保留项）**：为 `mcpcap` 的 `tools.no_approval` 写工具增加 `taint_gated: true` 条件审批开关。

**暂不批准 / 继续按既有 ADR 重审条件触发**
9. **否决** R4 的活跃 Run 内工具结果折叠、R5 的 capture 渲染退回业务 MCP 与可配置来源映射、R6 的 `Card` 自定义扩展载荷。
10. **延后** ADR-0056 实施（R4）、旁听/记忆/同意泛化（R5 其余项）、服务端辅助 SDK（R6）、网关通知归属/大结果外置/预热池（R9）、动态注册中心与用户级 OAuth（R10）、A2A（R11）、Opus/RTC 与私有化推理（R12，其中开源推理随首个私有化客户交付提前）。
