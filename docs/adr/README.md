# ADR 索引

> 建立于 2026-11-14（[ADR 评审](../review/2026-11-14-adr-review.md)）。新增、修订或取代 ADR 时同步更新本表。

## 怎样使用

- **先看本表，再读正文。** "状态"一列说明正文是否仍有效；被取代或已迁出的 ADR 是历史记录，现行规则在"取代关系"一列指向的 ADR 中。
- **"实施状态"** 依据 ADR 自述与代码：已实现、部分实现、未实现，或"未核对"（只读过决定，没有核对代码）。
- **"分级"** 按推翻代价 × 被依赖程度：一级是推翻即重写，二级是结构性约束，三级是局部决定。全部现行 ADR 都有"重审条件"小节（三级的于 2026-11-19 补齐）。
- **"方向，待定稿"** 是已批准方向、尚未实现的 ADR：实施前先定稿其中"待回答的问题"；实现之前照旧按它要修订的 ADR 执行。
- **"单线"** 标为"是"的，是按通用设计、需求与使用却都来自 xinli 的机制，第二条业务线接入时复核（ADR-0043 的修订）。

## 管理规则

- **按层存放。** 内核 ADR 在本目录；Persona 的在 [`persona/docs/adr`](../../persona/docs/adr/)，应用层的在 `apps/<业务线>/docs/adr`（xinli：[`apps/xinli/docs/adr`](../../apps/xinli/docs/adr/)）。业务 ADR 不占内核编号，各目录自己编号；引用时写"Persona ADR-0001""xinli ADR-0001"。
- **纠正事实**（进度、已过时的后果）：直接修改正文，在改动处注明日期。
- **标明边界**（当前能力边界、尚未实现、现行技术选择）：直接在正文中标明，注明日期。
- **架构改变**：写新 ADR，与旧 ADR 双向链接。
- **部分失效**：在页首（标题之下）逐条说明哪些有效、哪些已修订、哪些失效，以及现行规则在哪里。不能只在文末补记，也不能只写"见某某 ADR"。
- **正文与现行规则矛盾时**：用整合后的新 ADR 取代，并附逐条对照表（例如 ADR-0052）。
- **取代之前**：先把被取代 ADR 中仍然有效的保证迁入现行决定（例如 ADR-0040 → ADR-0045）。
- **同步 AGENTS.md**：新增或修订的 ADR 改变了开发时必须遵守的做法（新的保证、必须使用的接口、禁止的路径）时，在同一次改动中更新 `AGENTS.md` 中对应的一行。
- **取代之后，处理引用旧 ADR 的文档**：在相关行把编号改为现行 ADR，或在页首注明"引用这些 ADR 之处以现行 ADR 为准"；若某段描述的是旧 ADR 的机制而新 ADR 已经改变了它，直接改这段内容（注明日期），页首说明不能代替。
- **重审条件**：现行 ADR 都必须有（三级的于 2026-11-19 补齐），写成可观测的触发条件，任一出现即重新评估。

## 内核 ADR

| ADR | 标题 | 状态 | 实施 | 分级 | 重审条件（摘要） | 取代关系 | 单线 |
|---|---|---|---|---|---|---|---|
| [0001](0001-private-deployment.md) | 私有化部署，不依赖公有云托管服务 | accepted | 已实现 | 一 | 不再承接私有化；某项必需能力无法自托管 | — | |
| [0002](0002-device-as-node.md) | 设备即节点 | accepted | 已实现 | 二 | 需接入无法嵌入 HostApp 的端 | — | |
| [0003](0003-memory-owned-by-business-line-shared-by-grant.md) | Memory 归属业务线，经 Grant 共享 | accepted | 已实现 | 二 | 新的分类或以 EndUser 为单位的授权；共享 Memory 的业务线分属不同单元 | — | |
| [0004](0004-event-log-as-source-of-truth.md) | Event 日志是唯一事实源 | accepted | 已实现 | 一 | 重放超出恢复目标；Session 内部分删除的法定要求 | — | |
| [0005](0005-realtime-and-async-split.md) | Call 与 Run 分离 | accepted | 已实现 | 二 | 实时模型能安全地直接调用能力；回注延迟成瓶颈；弱网 | — | |
| [0006](0006-protobuf-as-contract-idl.md) | Protobuf 作跨端与存储契约 | accepted | 已实现 | 一 | 新端无代码生成；破坏性变更反复出现 | — | |
| [0007](0007-postgresql-as-storage-baseline.md) | PostgreSQL 作存储基线 | 已修订（2026-11-14） | 已实现 | 一 | 单分片写入接近压测上限 70%；停写超预算 | — | |
| [0008](0008-sandbox-trust-boundary.md) | 沙箱内无受信组件 | accepted | 已实现 | 一 | 命令通道成瓶颈；隔离技术不支持该结构 | — | |
| [0009](0009-session-scoped-sandbox-workspace.md) | 每个 Session 一个沙箱工作区 | accepted | 已实现 | 三 | 工作区间需共享大文件、工件交接不够；隔离选型无法保留工作区 | — | |
| [0010](0010-k8s-gvisor-for-production-sandboxes.md) | 生产沙箱用 K8s + gVisor | 被取代 | 未实现 | — | — | 被 0053 取代 | |
| [0011](0011-s3-api-as-artifact-storage-contract.md) | S3 API 作工件存储契约 | accepted | 已实现 | 三 | 私有化客户无 S3 兼容存储；需预签名直传 | — | |
| [0012](0012-context-compaction-as-event.md) | 上下文压缩记为 Event | accepted | 已实现 | 三 | 压缩相关评测回归；需多级或选择性压缩 | — | |
| [0013](0013-live-deltas-pulled-from-executing-process.md) | 实时增量从执行进程拉取 | accepted（现行技术选择） | 已实现 | 二 | 进程间无法直连；连接数超线性 | — | |
| [0014](0014-business-line-signed-tokens.md) | 业务线签发令牌 | accepted | 已实现 | 二 | 客户要求统一身份；需要即时吊销 | — | |
| [0015](0015-session-deletion-by-physical-removal.md) | Session 物理删除 | 已修订（ADR-0050） | 已实现 | 一 | 备份须即时失效；Session 内部分删除的法定要求 | — | |
| [0016](0016-memory-recall-as-event-and-category-grants.md) | 召回记为 Event，按类别授权 | 已修订（ADR-0022） | 已实现 | 二 | 新分类；以 EndUser 为单位的授权 | — | |
| [0017](0017-mcp-servers-as-capability-providers.md) | MCP Server 作能力提供者 | accepted | 已实现 | 三 | 三方 MCP 需按用户 OAuth 且业务线无法代理；需云端 stdio | — | |
| [0018](0018-evaluation-with-fixed-suites-and-cross-family-judge.md) | 固定用例集与跨系列评分 | 已修订（2026-10-11） | 已实现 | 三 | 评分模型或与人工一致性漂移；耗时跟不上改动 | — | |
| [0019](0019-usage-metered-outside-the-log-with-soft-quotas.md) | 用量在日志之外计量，软配额 | accepted | 已实现 | 二 | 对账差额超容忍度；硬配额需求 | — | |
| [0020](0020-agent-rollout-by-end-user-with-switch-events.md) | AgentDef 按 EndUser 灰度 | accepted | 已实现 | 三 | 需多进程即时一致；需按 EndUser 外的维度分流；需业务方自助发布 | — | |
| [0021](0021-moderation-before-the-log-fail-closed.md) | 内容安全在写日志前，失败即拒 | 已修订（ADR-0050、ADR-0054） | 部分实现（提供商为模拟） | 二 | 审核故障超过等待上限；流式草稿的监管要求 | — | |
| [0022](0022-health-memories-behind-business-line-consent.md) | 健康类 Memory 须单独同意 | 已修订（2026-11-19：按 EndUser 选择加入） | 已实现 | 二 | 健康之外的敏感类别；评测显示写入指令不可靠 | — | |
| [0023](0023-memory-write-gate.md) | Memory 写入闸门 | 已修订（2026-11-14；缺陷 2026-11-15 已修复） | 已实现 | 二 | 多语言业务线；误报漏报越过阈值 | — | |
| [0024](0024-one-connection-per-host-app.md) | 每个 HostApp 一条 Connection | 已修订（2026-11-14） | 已实现 | 二 | 媒体之外仍需第二条长连接 | — | |
| [0025](0025-questions-as-calls-routed-to-the-user.md) | 提问是路由到用户的调用 | accepted | 已实现 | 二 | 需他人回答；需富交互 | — | |
| [0026](0026-client-sdks-share-the-contract.md) | 端侧 SDK 共享契约 | accepted（现行技术选择） | 已实现 | 二 | 网页需 Node 角色；gomobile 成主要成本 | — | |
| [0027](0027-call-relayed-through-the-gateway.md) | Call 经网关中转，语音模型只经 `run_task` | 已修订（2026-11-14） | 已实现 | 二 | 同 0005；厂商提供设备短期凭证 | — | |
| [0028](0028-shard-keys.md) | 按 Session 或 EndUser 分片 | 已修订（2026-11-14） | 已实现 | 一 | 跨 EndUser 原子需求；停写超预算；热点槽位 | — | |
| [0029](0029-work-classes.md) | 工作队列按类别认领 | 已修订（2026-10-11） | 已实现 | 二 | 需动态调度；长任务误判；准入拒绝率 | — | |
| [0030](0030-decisions-as-events.md) | 判断记为 Event，只收紧不放宽 | accepted | 部分实现 | 三 | 判断需放宽才有价值；只能用托管模型；判断事件成日志主要增长 | — | |
| [0031](0031-delegation-as-routed-calls.md) | 委托是路由到子 Session 的调用 | 已修订（2026-11-14 标明当前边界；2026-11-16 串行与只传文字的边界由 0055 两期取代） | 已实现 | 二 | 有界并行；工件交接；异步委托 | — | |
| [0032](0032-cards-as-show-card-results.md) | 卡片是 `show_card` 的结果 | accepted | 已实现 | 三 | 固定组件集外的需求超过两次；需模型直出结构化内容 | — | |
| [0033](0033-server-side-fault-injection.md) | 服务端故障注入只在调试部署 | accepted | 已实现 | 三 | 生产需演练式故障注入 | — | |
| [0034](0034-reasoning-streamed-and-recorded.md) | 思考过程流式推送并写日志 | accepted | 已实现 | 三 | 截断妨碍排障；思考审核成主要成本 | — | |
| [0035](0035-background-calls-for-offline-devices.md) | 离线设备的调用转入后台 | 已修订（2026-11-19：委托调用可转后台，见 0058） | 已实现 | 三 | 离线常超后台时限；子 Session 设备调用需转后台；宽限期明显不合适 | — | |
| [0036](0036-captures-are-text-only.md) | 旁听只收文字，信任按说话人区分 | 被取代（正文与现行实现不符） | — | — | — | 被 0052 取代 | |
| [0037](0037-tiered-retention-by-daily-capture-sessions.md) | 以每日旁听 Session 分级保留 | 被取代 / 已迁出 | — | — | — | 内核部分被 0052 取代；业务部分迁至 xinli ADR-0001 | |
| [0038](0038-superseded-memories-are-kept.md) | 被取代的 Memory 保留历史 | accepted | 已实现 | 三 | 历史挤占存储或召回；法规要求更正即删 | — | |
| [0039](0039-captures-by-stream-provenance-derived.md) | 采集按流登记，来源由平台推导 | 被取代 | — | — | — | 被 0052 取代 | |
| [0040](0040-platform-triggers-declared-in-agentdef.md) | 平台按 AgentDef 的声明触发 Run | 被取代，不实施 | 未实现 | — | — | 被 0045、0050（现 0052）取代 | |
| [0041](0041-external-loops-decide-platform-acts.md) | 外部循环只做决定，平台执行 | accepted（设计已认可） | 未实现 | 二 | 上下文策略不够用；需要流式；开始实现 | — | |
| [0042](0042-xinli-entities-by-changeset.md) | xinli 的知识按实体维护、以变更集写入 | 已迁出 | — | — | — | 迁至 Persona ADR-0001 | |
| [0043](0043-kernel-persona-app-layers.md) | 内核、Persona 与应用分三层 | 已修订（2026-11-14） | 已实现 | 一 | 第二条业务线接入；业务侧连续两次绕开内核 | — | |
| [0044](0044-capture-text-store-and-erasure.md) | Capture 正文移出日志，按引用擦除 | 被取代 | 未实施 | — | — | 被 0050（现 0052）取代 | |
| [0045](0045-schedules-in-the-kernel.md) | 定时是内核的原语 | 已修订（ADR-0050、P2、2026-11-14） | 已实现 | 二 | 第二条业务线接入；xinli 需求超出现有语义 | 承接 0040 的有效保证 | 是 |
| [0046](0046-business-line-outbox.md) | 拉取式出站流 | 已修订（ADR-0050、P4；2026-11-22 补充迁槽与游标，2026-11-23 补充顺序） | 已实现 | 二 | 同上 | — | 是 |
| [0047](0047-oneshot-model-calls.md) | 无会话的单次调用 | 已修订（2026-10-28） | 已实现 | 二 | 同上 | — | 是 |
| [0048](0048-third-party-agents.md) | 三方 Agent | accepted | 未实现 | 二 | 三方托管实施前；"听过他人旁听"开始实现 | — | |
| [0049](0049-read-positions-advance-on-business-acks.md) | 交付确认，读取位置按回执推进 | 被取代 | 已删除 | — | — | 被 0050（现 0052）取代 | |
| [0050](0050-capture-text-in-the-business-line.md) | 旁听原文存放在业务线 | 被取代 | — | — | — | 被 0052 整合并取代 | |
| [0051](0051-shard-slots-and-id-location.md) | 固定槽位、裸 ID 定位与停写迁移 | 已修订（2026-11-15 删除旧格式 ID 兼容；2026-11-18 扩容默认整片搬迁） | 已实现（整片搬迁演练待目标环境） | 一 | 同 0028；停写超 15 分钟；8 片不够 | — | |
| [0052](0052-attested-results-and-provenance.md) | 旁听原文在业务线，内核只采信结构化片段 | accepted | 已实现（"听过他人旁听"未实现） | 二 | 第二类可信结构化内容；说话人角色扩展；注入判断改为阻断；容量 | 整合并取代 0036、0039、0050 与 0037 的内核部分 | |
| [0053](0053-self-hosted-sandbox-isolation-baseline.md) | 生产沙箱以自托管隔离为基线；默认 gVisor，经 RuntimeClass 选择 | accepted（2026-11-21 修订：选型完成） | 已实现（K8s Provider `internal/sandbox/k8s`，2026-11-21；开发用 Docker） | 二 | 不再承接私有化；gVisor 与 Kata 都无法部署；gVisor 逃逸未及时修复；进程或小文件密集负载占比显著；目标集群冷启动 p95 超过 3 秒；apiserver 成为 exec 瓶颈 | 取代 0010 | |
| [0054](0054-recoverable-wait-on-moderation-outage.md) | 审核基础设施故障时，Run 可恢复地等待 | accepted（2026-11-15） | 已实现 | 二 | 决定保存待审结果时另立 | 修订 0021 | |
| [0055](0055-bounded-parallel-delegation-and-artifact-handoff.md) | 有界并行委托与工件交接 | accepted（2026-11-16 定稿，分两期） | 已实现（两期均于 2026-11-16） | 二 | 同上；设备调用也需并行时另立 | 实现时修订 0031 | |
| [0056](0056-replaceable-context-strategy-and-streaming-loops.md) | 可替换的上下文策略与流式外部循环 | accepted（2026-11-16 定稿） | 未实现 | 二 | 业务线需要上下文策略（A 期）；0041 开始实现（B 期）；需要 Run 内调整、排序或非文字追加 | 实施时修订 0041 | |
| [0057](0057-legacy-shard-migration.md) | 存量分片迁移 | 不实施（2026-11-15：旧格式 ID 兼容已删除，前提不成立） | 不实施 | 二 | — | 被 0051 的 2026-11-15 修订取代 | |
| [0058](0058-background-delegation.md) | 委托在宽限期后转入后台 | accepted（2026-11-19 定稿） | 已实现 | 二 | 子 Session 设备调用也需转后台；需显式前台/后台选择；后台委托成主要成本 | 修订 0035 第 7 条、0031 | |
| [0059](0059-idempotent-session-creation-by-derived-id.md) | 建会话以幂等键派生 Session ID | accepted（2026-11-22 定稿） | 已实现 | 三 | 需按键查询或键过期；需平滑轮换密钥；线上观察到复查失败消耗键 | — | |
| [0060](0060-business-line-definition-access.md) | 业务线声明可用的定义；MCP Server 以 (业务线, 名称) 为键 | accepted（2026-11-22 定稿） | 已实现 | 二 | 需同名而内容不同的定义；需自助或分别发布共享定义；需撤销已有 Session 的使用权 | 修订 M4 灰度发布 §1 与部署 §1 | |
| [0061](0061-taint-gated-mcp-tools.md) | MCP 写工具按来源条件审批（`taint_gated`） | accepted（2026-11-22 定稿） | 已实现 | 二 | 业务线反馈先查后写审批过多且有可信结果判据；注入判断退出影子模式 | 补充 ADR-0017、ADR-0023；修订 MCP 适配 §2、§3 | |

## 业务层 ADR

| ADR | 标题 | 来源 |
|---|---|---|
| [Persona ADR-0001](../../persona/docs/adr/0001-entities-by-changeset.md) | xinli 的知识按实体维护、以变更集写入，交谈由模型划定 | 原内核 ADR-0042（2026-11-14 迁入，修订"人不建模"） |
| [xinli ADR-0001](../../apps/xinli/docs/adr/0001-daily-work-sessions-and-projects.md) | 按业务日分工作 Session，摘要归集到长期的 Project | 拆自内核 ADR-0037 的业务部分 |
| [Persona ADR-0002](../../persona/docs/adr/0002-on-demand-reads-and-incremental-materialization.md) | 按需读取与增量物化，变更集仍是事实源（accepted；第一期按需读取已实现 2026-11-16，第二期待复测决定） | ADR 评审 §3.2；实现时修订 Persona 架构 §13 Q93 |
