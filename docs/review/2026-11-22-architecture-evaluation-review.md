# 评审：内核架构评估（2026-11-21）

> 状态：已裁决并实施（2026-11-22，见 §10；A2 按裁决本轮不做）· 评审对象：[内核架构评估：业务支撑灵活性与边界清晰度诊断](../architecture/architecture-evaluation.html)（2026-11-21，提交 23e4dd0）· 依据：[章程](../charter.md) v0.2、[ADR 索引](../adr/README.md)、`docs/design/*.md` 与 2026-11-22 的代码 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准；原评估多处用“租户”“客户端”“消息队列”等泛称，本文统一写作 BusinessLine（业务线）、HostApp、Outbox，只在引用原文时保留
>
> 本文由同日两份独立评审合并而成，两份的事实核对互相印证，分歧之处在文中注明。另一份独立复核见[内核架构评估复核](./2026-11-22-kernel-architecture-evaluation-review.md)。

评估文档回答两个问题：内核能否不改基座就支撑多条业务线；内核与业务线的边界是否清晰。它给出的结论是“下层原语正交，上层边界双向越界”，并据此提出 B1–B6 六个问题和 R1–R12 四阶段路线图。

本文逐条把它的断言对照代码与现行文档，结论是：

- **事实大多属实**：被点名的文件、行号与机制基本都在，但有二十余处细节需要纠正（§3），其中几处影响定级，首屏 KPI 有多处混用测试或缺前提。
- **框架有偏差**：文档点名的“缺陷”大多已写入 ADR 或设计文档，有明确的推迟理由与重审条件；评估没有引用这些条件，也没有按 ADR-0043 的改内核判据衡量，因此把“有意推迟”定成了“边界失守”（§2）。
- **两项建议与现行不变量冲突**：Run 内折叠（R4）与 capture 渲染下放（R5）分别与请求前缀稳定、平台渲染可信页相矛盾，而评估在 §4 正把前者列为优势（§4）。
- **有九项值得单独立项**（§5）：建会话幂等、输入去重窗口、Guard 取令牌顺序三项在现行文档中没有取舍说明；AgentDef 的业务线归属是真实缺口，定级取决于单元内的信任假设。

建议：不整体采纳路线图与定级；把评估当作第二条业务线接入时的复核清单，与 ADR 索引的重审条件对照使用；§5 的条目单独立项。

## 1. 方法

- 按评估的 B1–B6 与首屏数字拆成 34 条断言，分三组核对：租户、幂等与调度；领域语义下沉；Outbox、规模与数字。
- 每条对照代码（`internal/`、`proto/`、`cmd/yanshi/`），并查 `docs/adr/`、`docs/design/`、`docs/launch-checklist.md`、`docs/review/` 中是否已有决定、理由与重审条件。数字追到原始出处（设计文档、`Makefile` 或提交）。
- 没有运行测试或评测；没有核对卡片表单控件的种类数与 `worker.go` 的循环实现细节。

## 2. 框架：有意推迟被定为缺陷

`apps/` 下只有 xinli 一条业务线。ADR-0043 的判据是“第二条业务线也会需要，且业务侧做不到或做了不安全”；在第二条业务线接入之前，“它也会需要”只是待验证的假设，据此泛化内核要在 ADR 中写明依据与撤回方式（2026-11-14 修订）。评估点名的条目几乎都已有对应记载：

| 评估的指控 | 现行记载 |
|---|---|
| AgentDef、Release 无业务线归属（B1） | `docs/design/m4-agent-rollout.md:9` 把“按业务线配置不同的发布（AgentDef 本身不分业务线）”列为不做；单元是共享 EndUser 的一组业务线，默认全在同一单元（`docs/design/deployment.md:11-13`） |
| MCP Server 名全局唯一（B1） | `docs/design/deployment.md:14` 已列为现有限制，同名须分属不同单元 |
| 队列同优先级全局 FIFO，无业务线公平调度（B1） | ADR-0029 第 24 行列为未做，用池隔离替代（`pgqueue.go:132` 的池可限定业务线）；重审条件“静态的业务线与优先级不够用” |
| 模型准入只在单进程，各提供商共用参数（B6） | `docs/design/model-calls.md:53,55` 已写明与改进方向 |
| QueueAdmission 默认关闭、各网关扫全分片（B6） | ADR-0029 第 40 行：阈值依据线上分布确定；`docs/review/2026-10-11-codex-discussion.md` D 点：规模到了再做 |
| `WEARER` / `OTHER`、`speaker_ref` 写进 proto（B2） | ADR-0052 重审条件：“第二类可信结构化内容”“说话人角色扩展”，在此之前不改内核中的命名与校验 |
| 记忆类别、health 同意项、中文健康词表（B2） | 固定类别是为了跨业务线 Grant 时 EndUser 看得懂自己授权了什么（ADR-0016）；ADR-0022 重审条件“健康之外的敏感类别”（`consent.Kind` 已留位置）；ADR-0023 重审条件“多语言业务线” |
| 卡片组件集封闭（B2） | ADR-0032 重审条件“固定组件集外的需求超过两次” |
| 循环与上下文锁死在 runtime（B3） | ADR-0041、ADR-0056 已定稿、未实现；ADR-0056 Q1：A 期等业务线提出需要时实施，B 期等 0041 开始实现时实施 |
| MCP 无用户级 OAuth（B5） | ADR-0017 重审条件；`docs/design/m5-mcp.md:98` 已写好将来的登记方式与删除流程；[2026-11-14 ADR 评审](./2026-11-14-adr-review.md) 附录 C 复核过：业务线自有 MCP 经 `X-Yanshi-End-User` 自管用户令牌 |
| AgentDef 等需改文件重启（B5） | ADR-0020 重审条件“需业务方自助发布”；`releases.yaml` 每 10 秒热加载（`cmd/yanshi/serve.go:519,524`、`internal/release/release.go:101-128`） |
| Outbox 绑定物理分片，无迁槽（B4） | ADR-0046 标为“单线”；ADR-0051 第 128 行明确推迟迁槽工具并给出触发条件；`docs/launch-checklist.md:39` |
| 网关通知无归属路由（B6） | ADR-0051 第 139 行、`docs/design/deployment.md:164` 列为不做；`internal/pg/notify.go` 不做 `UNLISTEN` 是为了避免取消订阅间隙丢通知 |
| 大 ToolResult 外置、`events` 分区（B6） | [2026-11-14 ADR 评审](./2026-11-14-adr-review.md) §4.2 归为容量触发问题 |
| 沙箱无预热池（B6） | ADR-0053：冷启动 p95 远低于 3 秒，预热池不是上线前提；重审条件“目标集群冷启动 p95 超过 3 秒”“apiserver 成为 exec 瓶颈” |

所以评估实际在主张“现在就提前触发这些重审”。这可以成立，但要逐条说明为什么不等触发条件出现，以及假设不成立时怎样撤回；评估没有做这一步。部分建议还要推翻现行决定：例如 R1 以 `(business_line, name)` 为 AgentDef 的复合键，与 `m4-agent-rollout.md` 的“AgentDef 本身不分业务线”相反，须先写 ADR，评估没有提到。

定级同样受影响。B1 被定为 P0“安全与多租户阻断”，前提是同一单元内的业务线互不信任；现行文档没有这一要求，而现有的隔离层评估也没有计入（§3 第 1 条）。两份评审对此有分歧，处理见 §5 A4。

## 3. 事实勘误

### 3.1 首屏 KPI 与 §4 的数字

| # | 评估位置 | 评估写法 | 实际 |
|---|---|---|---|
| 1 | 首屏 | “6812 在途 Run 全接管” | 6812 是强杀压测中全部完成的 Run 数；被杀进程当时在途的是 32 个，全部被其他进程接管（`m2-scale-test.md:78`） |
| 2 | 首屏 | `3.67×` 与 `188.8 runs/s` 并列 | 混用两次测试：`3.67×` 对应 50.5 → 185.6 runs/s；188.8 是加 `live.Follower` / `HeadHinter` 后的复测（`m2-scale-test.md:58-68,99`）；开启计量配额后约 177 runs/s（`m4-quota-usage.md:145`） |
| 3 | 首屏 | 问答 TTFT p99 53 ms | 单进程内存存储、首字固定 50 ms 的假模型（`scripts/bench-ttft.sh`，`m2-scale-test.md:183`、`m7-kernel-primitives.md:514,530-536`），衡量的是内核约 3 ms 的开销，不是真实模型的首字延迟 |
| 4 | 首屏、S3 | 2200+ 差分模拟种子 | 一次 `make sim` 的合计：2000 内存 + 100 单库 PG + 100 双库三方差分（`Makefile:103`，`day-acceptance.md:210`）；`make check` 默认只跑 3 个双库种子 |
| 5 | 首屏、S4 | 1→4 进程扩展、gVisor 冷启动 667 ms、10 GB 搬迁停写 573 ms | 数字有出处（`m2-scale-test.md:68,78,99`、ADR-0053:79、`deployment.md:318-321`），但都在开发环境测得（Mac mini 同机容器、单节点 k3s 且镜像已缓存）；目标环境验收未完成（`launch-checklist.md:16,41`），评估未注明 |
| 6 | §4 S1 | 内存只保留最近 4 个完整 Run | `KeepRuns = 2`，至多 3 个（`session/state.go:235,384`） |
| 7 | §4 S1 | `CommitAndWake` 使每 Run SQL 从 83 条降至 34.1 条 | 单条合并写入只到 45.7；45.7 → 34.1 来自 `service.WithAccess`、`usage.SpentCache` 与 `lifecycle.TouchEvery` 的叠加（`m2-scale-test.md:108-112`） |
| 8 | §4 S6 | 长 Run 缓存命中 40% → 70% | 只是 `long-run-ledger` 一个用例（`docs/design/m2-long-runs.md:207`）；`compaction-recall` 为 60% → 72%，`multi-turn-memory-chat` 约 33% |
| 9 | §3、R2 | G1–G9 灵活度 80%、72%、94%…；“异步编排接入代码量下降 70%+” | 没有给出计算方法或出处 |

### 3.2 代码实证中的偏差

| # | 评估位置 | 评估写法 | 实际 |
|---|---|---|---|
| 10 | 首屏、B1 | “0 租户隔离”，业务线 A 可越权调用业务线 B | 已有的隔离：令牌只能用于本业务线（`httpapi.go:270-287`）；Session 级 API 经 `service.WithAccess` 校验 `(BusinessLine, EndUser)`；`mcpcap.Server` 有必填的 `BusinessLine`，工具、`cite_captures`、`attests_speakers` 都按业务线过滤（`capability/catalog.go:191`、`mcpcap.go:36,102,267-272`）；委托的子 Session 继承父的业务线与 EndUser（`delegation/delegator.go:121`）。确实没有的是：AgentDef、Release、Oneshot 按全局名称解析，不校验归属（`service.go:100-128` 只调 `Agents.Resolve`；`agent:*` 经 `delegator.go:464-481` 列出全部 Agent）。后果是可以使用别的业务线的指令、模型策略与能力声明，`releases.yaml` 的撤回与 Oneshot 声明名槽位互相影响；数据、配额与 MCP 工具仍在本业务线内 |
| 11 | B1 | `release.go:69-81` | `internal/agentdef/release.go` 不存在；分属 `internal/agentdef/releases.go:24-69` 与 `internal/release/release.go:69-81`，结论不变 |
| 12 | B1 | Oneshot 并发槽键是 `"name:"+d.Name`，跨业务线争抢 | 键分 `total`、`business_line:<bl>`、`name:<name>` 三层（`oneshot/service.go:119-123`，`docs/design/model-calls.md:30`），业务线一层来自 `businesslines/*.yaml` 的 `oneshots.max_concurrency`；只有声明名一层跨业务线共享 |
| 13 | B1 | 队列只有全局 FIFO | `ORDER BY priority DESC, ord` 确无公平调度；但 `work_items` 存了 `business_line`，`workqueue.Pool.BusinessLines` 已支持按业务线认领（`workqueue.go:92-100`），只是 `serve` 没有暴露配置 |
| 14 | B1 | RPS 先于优先级抢令牌（引 `guard.go:106-132`） | 行号应为 `model/guard.go:204-219`（106-132 是类别定义）。顺序属实（先 `rate.wait` 再 `gate.acquire`），但并发门按类别排队，低优先级不会插到高优先级前面。准确的说法是：RPS 层不区分类别，令牌在排队前即被消耗；各提供商各有 `Guard` 实例，但启动参数只有一套共用的 `modelLimits`（`cmd/yanshi/serve.go:303-310`） |
| 15 | B4 | 改槽位表后全部游标报 403/410 作废 | 游标只含业务线、分片、epoch、过滤版本与位置（`outbox/stream.go:61-69`），不含槽位表；410 来自 epoch 轮换或低于保留地板，403 是游标与请求的业务线或分片不符（`httpapi/outbox.go:64-75`）。游标失效后的恢复流程已设计并实现（`docs/design/m7-kernel-primitives.md` §6.5，`apps/xinli/recovery`）。设计中缺的是重分片之后游标怎样映射 |
| 16 | B2 | 中文星期与 5 分钟交谈间隔在 `capture.go:63-120` | 中文星期与“之后七天”在 `capture.go:63-79`；`DefaultExchangeGap` 在 `capture/page.go:20`，只是默认分页参数，经 `_meta["yanshi/page_options"]` 传给业务线 |
| 17 | B2 | `health.go` 硬编码“心理咨询师”、身份证正则与 Asia/Shanghai 时区 | “心理咨询师”是排除词（`health.go:28`，避免职业误报），触发词是“心理咨询”（`:21`）；身份证正则与时区在 `memory/service.go:76,137`，时区是默认值，可经 `Service.Location` 覆盖 |
| 18 | B2 | `cite.go` 递归扫描任意参数中的 `#57.3`、全日志回扫 | 只对登记了 `cite_captures: true` 的同业务线 MCP 启用（`docs/design/captures.md:36`，`mcpcap.go:354-366`）；参数中没有 `#N.M` 引用时不读日志（`cite.go:74`） |
| 19 | B2、§2 表第 4 行 | 写闸门只保护 `memory_save`，业务的 MCP 写工具没有保护 | 实现了 `ApprovalSummarizer` 的本地能力（目前是 `schedule`）受同一来源判断约束（`runtime/worker.go:783-791`）；未声明只读的 MCP 工具默认需要审批（ADR-0017）。缺的是：被 `tools.no_approval` 豁免的 MCP 写工具无法声明“受外部内容影响时才审批” |
| 20 | B2 | `speaker_ref` 是“声纹引用” | 它是业务线填入的不透明实体 ID，内核只校验语法，来源推导不看它（ADR-0052） |
| 21 | B3 | AgentDef 的 context 只能调窗口大小与压缩比例 | 还有 `scope: run`、`max_tool_result`、`keep_recent`、`summary_model`（`agentdef/agentdef.go:79-100`） |
| 22 | R2 | 突破内存 64 条环形窗口 | `MaxRecentInputs = 64` 是 Event 日志的投影，随 `SessionSnapshot` 持久化（`session/state.go:470`、`snapshot.go:102-104`），重启不丢；限 64 条是为了让投影与快照有界 |
| 23 | B6 | 大 ToolResult 过期靠逐行 DELETE | `events` 没有过期清理路径，只有按 Session 删除（`eventlog/pglog/pglog.go:192`）；“全文内嵌、无分区、无自动外置”属实 |

## 4. 与现行不变量冲突的建议

**R4 的“长 Run 内已消费工具结果的声明式折叠”。** ADR-0056 Q3、Q4 有意规定策略每个 Run 只调用一次、Run 内上下文只在尾部追加、当前 Run 总是保留，理由是 Run 内的请求前缀保持稳定（AGENTS.md“请求前缀保持稳定”）。在 Run 内折叠会让此后每一步的前缀缓存失效，而评估在 §4 S6 正把前缀缓存列为核心优势；改写已执行动作的摘要还会影响来源推导。B3 引用 ADR-0056 第 49-50 行，把这一定稿当作缺口，却没有讨论它的理由。要做，须先满足 ADR-0056 的重审条件“需要 Run 内调整”，并用评测报告的缓存命中与总成本两列证明代价可以接受。现有的手段是 `MaxToolResult` 截断（只作用于模型视图）与压缩。

**R5 的“把 internal/capture 的渲染下放回业务侧 MCP”与“业务线声明角色到来源的映射”。** ADR-0052 的安全边界是平台渲染可信页、写日志前审核：业务侧给出的文字不能作为来源推导的依据，否则可信的平台框架文字与不可信的外部内容混在同一文本流里。中文星期、“之后七天”日期头与 5 分钟交谈间隔这类渲染参数可以改为可配置；把渲染整体交给业务线，则是拆掉旁听的信任边界，不只是“泛化”。可配置的角色映射另有两个后果：放宽 `memory_save` 的免审边界；配置一变，重放旧日志会得到不同的 `Provenance`，违反 ADR-0004。说话人角色的扩展已是 ADR-0052 的重审条件，届时须同时修订来源推导，由平台继续固定推导规则。

## 5. 值得采纳的部分

下列条目或在现行文档中没有取舍说明，或属于 ADR-0043 修订所说的“内核既有职责的完善”，不必等第二条业务线接入。

| # | 条目 | 现状 | 为什么值得做 | 下一步 |
|---|---|---|---|---|
| A1 | 建会话幂等 | 创建 Session 不接受幂等键（`httpapi.go:371-408`、`node.proto:182-186`）；`docs/` 中没有找到取舍说明 | 输入早有 `input_id` 去重，建会话却没有，客户端重试会产生孤儿 Session；xinli 为此自建了创建意图、孤儿回收与日志对账（`apps/xinli/orchestrator`） | 已立项（2026-11-22），设计见[建会话幂等](../design/session-create-idempotency.md)：由幂等键派生 Session ID，不新增存储；半成品续建（§4.3）、删除后键永久消耗（§5.3）已在设计中处理 |
| A2 | 输入去重窗口 | `MaxRecentInputs = 64`（`session/state.go:468-470`），注释的前提是“重提通常在几秒内” | 业务线的异步编排在超时后重提，可能晚于窗口，前提不成立；xinli 结果未知时分页查找 `input_id`（`apps/xinli/kernel/inputs.go:68`）；委托已有不受窗口限制的例外（`State.Delegated`） | 与 A1 一起评估：持久去重表或延长窗口。持久表须按 Session 分片登记 `pg.Tables` 与 `app.SessionData`、写入入口 `lifecycle.Recheck`、进 `internal/sim` 删除不变量；延长窗口须保持投影有界并递增 `ProjectionVersion` |
| A3 | Guard 取令牌顺序 | `guard.go:204-219` 先取 RPS 令牌再进并发门排队；RPS 层不区分类别；各提供商共用一套启动参数 | 令牌在排队前被消耗；RPS 紧张时交互类与后台类同等竞争，与 ADR-0029 的分类意图不一致；没有文档说明这一顺序 | 局部修改，同时支持按提供商分设并发与 RPS；改动 `model-calls.md` 的准入一节；需 `make sim` 与 TTFT 压测对照 |
| A4 | AgentDef 的业务线归属 | 代码中 AgentDef、Release、Oneshot 不分业务线，`m4-agent-rollout.md:9` 也这样写；但 `deployment.md:11` 写“Token、配额、AgentDef……都以 BusinessLine 为单位” | 两份设计文档互相矛盾；MCP 工具按业务线过滤而 AgentDef 目录（含 `agent:*`）不过滤，口径不一 | 先裁决单元内业务线之间的信任假设并写入 `deployment.md`。一份评审认为这是内核治理职责的遗漏，应直接按 P0 补齐；另一份认为现行文档没有“互不信任”的前提，P0 依据不足。若裁决为要求隔离，按 ADR 流程修订 m4-agent-rollout，范围覆盖建 Session 的默认与指定版本、Worker 读取定义、委托目录、`httpapi` 与 Oneshot 槽位、`mcpcap` 以 `(BusinessLine, Name)` 建键；并写明未声明归属的旧定义怎样迁移、哪些定义有意共享 |
| A5 | Outbox 重分片后的游标 | 游标绑定物理分片与 xid8；整片搬迁能保住游标，改槽位表时的游标处理未设计（ADR-0051 §5.4 只要求校验出站水位） | 对外契约每多一个消费方就更难改；同库长事务拖住所有业务线的水位，ADR-0046 已承认，对策只是监控与事务超时 | 第二条业务线接入 Outbox 之前，在 ADR-0046 补写重分片的游标映射或换代方案；不必先实现迁槽工具（见 §6 R7） |
| A6 | 大 ToolResult 全文存入 events | `MaxToolResult` 只截断模型视图，日志存全文；`events` 无分区 | `docs/review/2026-10-11-codex-discussion.md:66` 提过“大结果工件化”，一直没有形成决定 | 按真实事件率、结果大小、WAL 与清理成本压测后决定是否外置为所属 Session 的工件；须保持重放、续传、审核与删除语义 |
| A7 | MCP 写工具的条件审批 | 被 `tools.no_approval` 豁免的写工具无法复用 `schedule` 与 `memory_save` 的来源判断（§3 第 19 条） | 只收紧不放宽，复用现有 `tainted` 与 `ApprovalSummarizer` | 新的平台能力（如 `taint_gated: true`），须写 ADR 并补评测 |
| A8 | 按业务线的工作池 | `workqueue.Pool.BusinessLines` 已实现，`serve` 未暴露（§3 第 13 条） | 共用单元时大业务线或批量业务线可独立分配 Worker，零 SQL 改造 | 暴露为 `serve` 参数，改动 `deployment.md`；公平调度仍按 ADR-0029 的重审条件 |
| A9 | 防长事务的 PG 参数 | 长事务推迟所有业务线的出站水位（ADR-0046） | `statement_timeout`、`idle_in_transaction_session_timeout`、`max_prepared_transactions=0`，只读分析与备份走备库，可立刻缓解 | 写入生产部署基线与 `launch-checklist.md`；用现有水位与最老事务指标验证 |

R3（真实审核提供商、推送与唤醒、链路追踪、目标集群验收）与 `docs/launch-checklist.md` 的阻断项重合，定为 P0 没有问题，按清单推进即可，不需要另立；评估的 R3 还漏了合同价格、业务线密钥与保留策略、健康信息同意流程、上线版本评测等清单中的独立事项，以清单为准。

## 6. 对路线图的逐项意见

| 项 | 意见 |
|---|---|
| R1 租户命名空间 | 先做 A4 的裁决；复合键要推翻现行设计，须写 ADR。MCP 同名限制按 `deployment.md:14` 保持，裁决要求隔离时随 A4 一起改 |
| R2 会话幂等与持久去重 | 采纳，见 A1、A2；不需要 labels 查询之前不做 labels |
| R3 上线闭环 | 采纳，即 launch-checklist |
| R4 上下文策略与外循环 | A 期、B 期按 ADR-0056 的触发条件实施；Run 内折叠不采纳（§4）。拆分 `worker.go` 可以单独做，但不改执行语义、保持 `Worker.Step` 的同步路径，须过 `make sim` |
| R5 泛化可信内容、记忆分类、写闸门 | 渲染参数可配置化可以做；渲染下放与可配置来源映射不采纳（§4）；类别与同意项按 ADR-0022、ADR-0023 的重审条件处理，新增时须写明跨业务线 Grant、撤回与删除的含义。`taint_gated` 见 A7 |
| R6 卡片扩展槽与服务端 SDK | 按 ADR-0032 的重审条件；`custom_component + data_json` 这类非结构化载荷内核无法保证审核覆盖与防追踪，届时要用具体组件验证审核、旧端降级与动作语义。服务端 SDK 等第二条业务线接入时再评估 |
| R7 Outbox 逻辑流 | 先做 A5 的设计，实现等迁槽需求出现。固定逻辑分区（每个分区整体落在单个物理分片、以分区为最小搬迁单位）方向可行、兼容单分片查询；难点是去掉 xid8 后的序号：序列或计数行在并发提交乱序时会跳号漏读，仍要可见性屏障；迁槽时须交接续读位置、识别重复信封、延续删除与保留地板，并经 `make sim` 验证回滚、备份恢复与慢事务 |
| R8 公平调度与全局准入 | A3 现在做；A8 暴露按业务线的池；按业务线加权公平认领与跨副本协调按 ADR-0029 的重审条件，触发依据是混合负载压测（一条线保持交互基线，逐步加另一条线的负载，看两线排队延迟、首字 p99 与拒绝率）；共享令牌服务按 `model-calls.md:55` |
| R9 通知归属、结果外置、预热池 | 结果外置见 A6；通知归属与预热池按 ADR-0051、ADR-0053 的重审条件 |
| R10 注册中心与用户级 OAuth | 按 ADR-0020、ADR-0017 的重审条件；业务线个位数之前，GitOps 加 `make eval` 门禁发布比控制面数据库可靠 |
| R11 A2A | 已有 ADR-0048 与设计，按其重审条件实施 |
| R12 私有化推理与弱网通话 | 章程 §10 的待决问题与 M3 待做项，不属于边界问题；开源推理引擎适配随首个私有化交付立项；注入分类器的阈值只在留出集上确定（ADR-0030），当前上线决定为影子模式 |

## 7. 对内核准入标尺（评估 §7）的意见

- 可以采纳：标尺 2（业务线复合键测试，前提是 A4 裁决要求隔离）、标尺 4（治理不可绕过、策略可替换）、标尺 5（外部动作幂等测试）、标尺 6（单进程可跑、差分模拟不退化）。
- 修正标尺 1（领域名词零准入）：用于现行旁听契约会与 ADR-0052 保留现有命名与校验的决定冲突。改为：新机制进入内核须满足 ADR-0043 判据并登记单线状态与重审条件；已评审的过渡命名在重审条件触发前不改名。
- 修正标尺 3（物理拓扑零暴露）：作为新契约的准入条件成立；现行 Outbox 是 ADR-0046、ADR-0051 登记在案的阶段性取舍，在专门的 ADR 迁移契约之前不据此阻塞日常维护。
- 补充一条：涉及事件结构、来源推导、上下文组装或工具结果外置的改动，须保持历史日志重放结果不变（ADR-0004），并用评测报告的缓存命中列确认没有退化。

## 8. 评估没有覆盖的问题

1. **同一单元内 EndUser 身份域未校验。** 部署单元已定义为共享 EndUser 身份与 Memory 的一组业务线（[2026-11-14 ADR 评审](./2026-11-14-adr-review.md) §2.3），但令牌由各业务线分别签发（ADR-0014），内核不验证两条业务线签出的 `end_user` 是否来自同一账号体系。第二条业务线若用独立的本地用户 ID 且与第一条撞号，开启跨线 Grant 就会串人。应在 `businesslines/*.yaml` 与接入规范中声明身份域，或禁止不同域的业务线互相 Grant。（2026-11-26 已由 [ADR-0064](../adr/0064-identity-domains-for-cross-line-grants.md) 处理。）
2. **评测资产的归属。** 多条业务线接入后，`evals/` 须分为内核通用回归集（换模型、改压缩、闸门、委托时必跑）与业务线自有评测集（各自维护基线，作为其 AgentDef 灰度的准入条件），避免内核为每条业务线的提示词维护基线。
3. **节奏。** `launch-checklist.md` 仍有十余项未闭环；此时铺开 R4–R9 会拉长首次生产验证，真实流量暴露的问题（延迟、审核误杀、成本分布、慢事务形态）比纸面推演更能校准后续优先级。

## 9. 对评估原文的处理

评估文档保留为 2026-11-21 的历史记录，不改正文；其页首已加一行（2026-11-22），指向本文，并说明“路线图与定级以本文 §5、§6 为准”。§5 各项是否立项，待裁决（A1 已立项）。

## 10. 裁决与实施（2026-11-22）

| # | 裁决 | 状态 |
|---|---|---|
| A1 | 采纳 | 已实现：[ADR-0059](../adr/0059-idempotent-session-creation-by-derived-id.md)、[设计](../design/session-create-idempotency.md)；实现中补了三处设计漏洞（并发请求以写索引后读回的绑定为准；期限过后的重放返回 404 而非 400；撤回当次即返回 404），并改为 `-auth jwt` 的网关缺少密钥即拒绝启动（SDK 每次创建都带键） |
| A2 | 本轮不做 | 取舍与重审条件写入[多端双工通道](../design/m3-duplex-channel.md) §5 |
| A3 | 采纳 | 已实现：先取并发名额、再等 RPS 令牌；`-model-limit` 按提供商覆盖（[模型调用](../design/model-calls.md) §5） |
| A4 | 单元内业务线按互不信任对待 | 已实现：[ADR-0060](../adr/0060-business-line-definition-access.md)。采用"业务线配置声明可用定义"（`agents.allow`、`oneshots.allow`），不用复合键；MCP Server 改以 (业务线, 名称) 为键，取消同名须分属不同单元的限制 |
| A5 | 采纳"迁槽即换代" | 已写入 [ADR-0046](../adr/0046-business-line-outbox.md)“迁槽与游标”与 ADR-0051 §5.4：出站行不搬、停写后先等消费方追平、受影响分片换代后走既有的游标过期恢复；不做游标映射与固定逻辑分区（R7）。迁槽工具仍推迟，实现时须补的运维入口、消费端缺口与模拟测试列在该节 |
| A6 | 压测后决定现在不外置 | 数据、理由与重审条件见[工件](../design/m2-artifacts.md) §6：有上限的来源（≤ 64 KiB）每条提交延迟增加不到 1 ms，主要放大点是投影快照存未压缩历史；没有独立上限的是设备、后台与委托结果。压测 `TestToolResultSizeProfile` 随代码提交，默认跳过 |
| A7 | 采纳 | 已实现：[ADR-0061](../adr/0061-taint-gated-mcp-tools.md)，MCP 登记的 `tools.taint_gated`；评测 `mcp-crm-taint-gated-direct`、`mcp-crm-taint-gated-after-search` |
| A8 | 采纳 | 已实现：`serve -worker-business-lines`（[部署](../design/deployment.md) §3） |
| A9 | 采纳 | 已写入[部署](../design/deployment.md) §4 的生产基线与上线检查清单 |
