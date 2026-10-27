# xinli 与 persona agent 的整体架构

> 状态：设计（2026-10-27），经逐题讨论确认（§13），再经 Codex 两轮评审修订（[评审记录](../review/2026-10-27-persona-architecture-review.md)），未实现 · ADR-0043（三层）、ADR-0044（Capture 正文存储与擦除）、ADR-0045（定时）、ADR-0046（出站流）、ADR-0047（单次调用）、ADR-0048（三方 Agent）、ADR-0049（交付确认）· 内核的新增原语见 [M7 内核补充](./m7-kernel-primitives.md)，三方 Agent 见 [M7 Agent 生态](./m7-agent-ecosystem.md) · 承接：[旁听](./captures.md)、[旁听业务线 xinli](./xinli-captures.md)、[xinli 的建模](./xinli-model.md)、[评审：xinli 与内核的下一步](../review/2026-10-27-xinli-kernel-next.md) · 待裁决：[Capture 与流该不该在内核](../review/2026-10-27-capture-boundary-review.md)、[带图示的评估页面](../review/2026-10-27-persona-plan.html) · 内核术语见 [CONTEXT.md](../../CONTEXT.md)，Persona 与应用层术语分别见 [Persona 术语](../../persona/CONTEXT.md)、[xinli 术语](../../apps/xinli/CONTEXT.md)（§3.1）

心力球是以录音为入口的个人助手：记录、回忆、跟进事务、反思成长。本文回答三个问题：现在的 yanshi 能不能承载它；不能承载的部分补在哪一层；以后它演进成更完整的 persona agent、接入三方 Agent 时，扩展的范式是什么。

## 0. 结论

- **yanshi 的执行内核能承载 xinli，不需要推倒。** Session、Run、Event、Capture 与流、来源推导、写入闸门、Retention、委托、评测与模拟测试，正是一个长期运行的个人 Agent 需要的底座。
- **缺的是两块。** 一是内核的几处原语（§2.3），都是"第二条业务线也会需要、且业务侧做不到或做了不安全"的；二是更大的一块：生产的业务后端完全没有，目前只有 `persona/` 的内存实现与 `apps/xinli` 的开发进程。
- **分三层**（ADR-0043）：yanshi 内核只管 Agent 怎样可靠、安全地运行；Persona 层管关于一个人的结构化知识；xinli 应用层管产品（账号、设备、识别、渠道、推送、编排）。Persona 与应用层只经公开契约使用内核，内核不认识它们。
- **扩展范式**：新功能默认只改 Persona 与应用层（§9）；内核只在满足判据时改，并写 ADR。

## 1. 适配性评估

| 功能 | 现在的 yanshi | 落在哪里 |
|---|---|---|
| 全时记 | Capture 与 transcript 流、`captures` 按时段分页与读取位置、`xinli-day` 整理、变更集（mock） | 内核已有；识别在应用层（§4.1），交谈与重点在 Persona |
| 实况预览 | 只能开 Run，每次刷新都写日志 | 内核新增**单次调用**（Oneshot，ADR-0047） |
| 随口记 | 输入、`/transcriptions` | 内核已有；笔记在 Persona |
| 内容纠错与词库 | Event 只追加，原文不能改 | 内核新增**原文修订**（CaptureCorrection）；摘要与知识的纠错是 Persona 中用户的变更集；词库在 Persona，识别热词在应用层 |
| 个人记忆（人、组织、概念、事件） | Memory 是扁平陈述，每人每业务线至多 500 条，不建模"人"（ADR-0042） | Persona 的实体与时间线（§3） |
| 智能问答（引用、历史、重生成、收藏） | 助手 Session、Memory 召回、卡片 | 内核新增**重答**（Redo）；线程即 Session，收藏在应用层，引用来自 Persona 的依据（§3.4） |
| 项目跟踪、待办 | `xinli-project`、承诺账本（mock） | Persona |
| 定时提醒与执行 | 按钟点的定时由业务线以服务令牌提交，内容在投影里算"用户原话" | 内核新增**定时**（Schedule，ADR-0045） |
| 未来录音关注、心力优先、周期洞察 | 无 | Persona 的关注项与目标；由定时开启的 Run（§5） |
| 个性化理解 | Memory（本人） | 内核 Memory 只存本人；画像在 Persona（§3.3） |
| 账号、设备、权益、声纹 | 无 | 应用层；会员分档的配额由内核执行（**档位** Tier） |
| 多渠道 | 只有按 Session 的 SSE | 内核新增**出站流**（Outbox，ADR-0046）；渠道适配在应用层 |
| 外部 AI 授权 | 无 | Persona 对外的 MCP（[M7 Agent 生态](./m7-agent-ecosystem.md) §3） |
| 数据控制（部分记录删除） | 只能按整个 Session 删除，`captures` 的工具结果里还有原文副本 | 内核新增**擦除**（Erasure）与 Capture 正文存储（ADR-0044）；派生数据由 Persona 按引用级联删除 |
| 运营与版本分发 | 用量、指标（只含 ID） | 应用层 |

## 2. 三层

### 2.1 拓扑

```
 HostApp（手机，嵌入 yanshi SDK）── 领夹 ─ 蓝牙
   │ 音频分片                 │ 输入、审批、回答、事件流（Connection / HTTP，用户令牌）
   ▼                          ▼
┌──────────────── xinli 应用层（apps/xinli）────────────────────────────┐
│ 采集识别  账号·设备·声纹·权益  渠道适配(微信/飞书/TeamTalk)  推送  编排器  BFF  运营 │
└──┬─────────────┬──────────────────────┬───────────────┬──────────────┘
   │ 服务令牌：     │ 出站流（拉取）          │ 查询与用户改动   │ 单次调用
   │ Capture、输入、│                        ▼               │
   │ 定时、Session  │        ┌──── Persona 层（persona/）────┐ │
   │               │        │ 实体·时间线·交谈·承诺·项目·笔记 │ │
   │               │        │ 变更集日志 + 物化状态 + 混合检索 │ │
   │               │        │ MCP 工具（内核调用）· 查询 API   │ │
   │               │        │ 对外 MCP（OAuth，三方只读）     │ │
   │               │        └──────────▲────────────────────┘ │
   ▼               ▼                   │ MCP（X-Yanshi-End-User / Session，cite_captures）
┌──────────────────────── yanshi 内核 ─────────────────────────────────┐
│ Session·Run·Event  流·触发·定时  来源·写入闸门·内容安全  Memory·Grant      │
│ 能力路由(设备/沙箱/MCP/A2A/委托)  Capture 正文存储·擦除  出站流  单次调用  配额·档位 │
└──────────────────────────────────────────────────────────────────────┘
```

### 2.2 职责与边界

| 层 | 管什么 | 不管什么 | 数据 |
|---|---|---|---|
| yanshi 内核 | Agent 怎样运行：执行语义、来源与信任、内容安全、配额、删除、能力路由、评测 | 任何业务实体（交谈、承诺、项目、人）；一天从哪里开始 | 事件日志、Capture 正文、Memory、用量；按 Session 或 EndUser 分片 |
| Persona 层 | 关于一个人的结构化知识：实体、时间线、交谈、承诺、项目、笔记、关注项、目标、画像、词库；变更集的检查与回执 | 账号、设备、渠道、推送；Agent 的执行 | 独立的 PostgreSQL，按 (BusinessLine, EndUser) 分片；变更集日志是它的事实源 |
| xinli 应用层 | 产品：采集识别、账号设备声纹、权益、渠道、推送、跨 Session 编排、App 的 BFF、运营 | 知识的语义（交给 Persona）；Agent 的执行（交给内核） | 账号、设备、权益、渠道映射、收藏 |

依赖方向只有一个：应用层 → Persona → 内核的公开契约（`proto/`、`gen/`、`sdk/`、HTTP API、MCP）。`persona/` 与 `apps/xinli/` 是独立的 Go module，模块路径不以 `yanshi/` 开头（`persona`、`xinli`），Go 的 internal 规则才会拒绝它们 import `yanshi/internal/...`；`make check` 另做依赖检查；内核不 import 它们（ADR-0043）。

Persona 按业务线分租户，数据归业务线（ADR-0003 的延伸）：第二个 persona 类产品用同一套 Persona 服务，数据互不可见；跨业务线读取以后经 Grant 扩展（[M7 Agent 生态](./m7-agent-ecosystem.md) §4）。

### 2.3 内核补的原语

| 原语 | 为什么在内核（§9 的判据） | 设计 |
|---|---|---|
| Capture 正文存储、擦除、原文修订 | 部分删除要物理删除原文，事件日志只追加，业务侧碰不到日志 | [M7](./m7-kernel-primitives.md) §2–§4，ADR-0044 |
| 交付确认（Ack） | 读取位置随 Run 完成提交，交出失败时会越过没保存的时段；只有平台能决定位置 | [M7](./m7-kernel-primitives.md) §11，ADR-0049 |
| EndUser 删除屏障 | 注销期间按 EndUser 的写入可能复活数据 | [M7](./m7-kernel-primitives.md) §12 |
| 定时（Schedule） | 业务侧提交的输入在投影中是"用户原话"，会把模型写的或外部来的文字洗成可信内容 | [M7](./m7-kernel-primitives.md) §5，ADR-0045 |
| 出站流（Outbox） | 业务线服务端每个 Session 挂一条 SSE，几十万 Session 撑不住；擦除要可靠地通知 Persona | [M7](./m7-kernel-primitives.md) §6，ADR-0046 |
| 单次调用（Oneshot） | 业务侧直连模型会绕过计量、配额、内容安全与换模型的评测（章程 G9） | [M7](./m7-kernel-primitives.md) §7，ADR-0047 |
| 档位（Tier） | 配额由内核执行，触发与定时开启的 Run 没有用户令牌，档位只能存在内核 | [M7](./m7-kernel-primitives.md) §8 |
| 重答（Redo） | 只有平台能让 Transcript 略去被重做的 Run | [M7](./m7-kernel-primitives.md) §9 |
| 说话人引用（`speaker_ref`） | 契约字段，只能在 `proto/` 中加 | [M7](./m7-kernel-primitives.md) §10 |

加上已经设计、尚未实现的触发（ADR-0040 第 2 步）与三方 Agent 的 A2A 出站和入站（ADR-0048）。

## 3. Persona 层

### 3.1 术语

Persona 层术语见 [persona/CONTEXT.md](../../persona/CONTEXT.md)，内核的 `CONTEXT.md` 只保留内核术语。Exchange、Changeset、Revision、Commitment、Project、Awareness 沿用原有定义，已迁入 Persona 术语表；以下新增术语也在其中定义。Span 是平台的分页单位，留在内核。

- **Entity（实体）**：Persona 中有稳定 ID 的一个人、组织、概念或事件，带别名。承诺的对方、交谈的参与者、项目的相关方都引用实体。
- **Note（笔记）**：EndUser 经随口记口述的内容，是本人的话，整理后保存。
- **Watch（关注项）**：EndUser 事先指定的、之后的旁听中要特别留意的内容（"下周和供应商开会时留意交期"）。整理时随候选一起召回，命中后在交谈中标出。
- **Goal（目标）**：EndUser 明确说过的重要目标，心力优先据此对照近期投入。
- **Lexicon（词库）**：EndUser 的专有词汇（人名、项目名、术语），供识别热词与整理使用。
- **Lineage（依据）**：派生对象所依据的引用（`#seq.index`、交谈 ID、笔记 ID）。

### 3.2 数据模型与存储

- **事实源是变更集日志**（ADR-0042 的推广）：Agent 交出的变更集、用户在 App 中的改动、三方提议并经用户确认的改动，都记为带作者与依据的变更集；交谈、承诺、实体、项目的当前状态是按变更集叠加出的物化表。按来源键幂等，重交整体取代，`rev` 做乐观并发，这些规则不变。
- **存储**：独立的 PostgreSQL（与内核不共用库，故障域分开），按 (BusinessLine, EndUser) 分片，一个事务只涉及一个键（与 ADR-0028 同一约束）。
- **检索**：pgvector 加中文全文检索混合召回，供 `find_related`、问答的跨记录汇总使用；私有化部署只依赖 PG 扩展。嵌入经内核的单次调用（`embedding` 种类）取得，计入用量。
- **依据**：每个派生对象记录 Lineage，并维护反向索引（引用 → 对象）。两处用它：问答的来源引用直接给出依据；擦除的级联删除。
- **擦除的跨层契约**（[M7](./m7-kernel-primitives.md) §3）：Persona 收到出站流的 `capture_erased` 后，在同一事务中记下擦除墓碑、删除只依据被擦除片段的对象、把依据部分被擦除的对象标为不可读并移出检索向量（重新整理后恢复或删除）；之后每个变更集在提交事务内检查墓碑，引用落在其中的操作被拒绝，迟到的提交不会复活被删的内容。从擦除到 Persona 生效之间是出站流的消费延迟，BFF 在发起擦除后立即在 App 中隐藏相关对象。
- **交付回执**：`file_changes` 等交出整理结果的工具在保存成功时附结构化回执，内核据此推进读取位置（[M7](./m7-kernel-primitives.md) §11）；拒收不附回执。

### 3.3 与内核 Memory 的分工

| | 内核 Memory | Persona |
|---|---|---|
| 存什么 | 关于 EndUser **本人**的陈述：profile、preference、interest、plan、health | 关于 EndUser 的**世界**：他人、组织、事件、交谈、承诺、项目、笔记、画像 |
| 怎么进上下文 | 每个 Run 开始时自动召回（记为 Event） | Agent 按需调用工具拉取；结果是外部内容 |
| 跨业务线 | 经 Grant 按类别共享（ADR-0003） | 按业务线隔离；三方经 OAuth 范围授权 |
| 写入保护 | 写入闸门（ADR-0023）、健康许可（ADR-0022） | 变更集检查、用户优先、三方写入只进"待确认" |

xinli 的 AgentDef 不再把他人的信息写成 Memory 的 `relationship`：人在 Persona 中只记一处。这是接入 Persona 的业务线的约定，由 AgentDef 的指令与白名单落实；内核的 `relationship` 类别不变，没有 Persona 的业务线照常使用（北极星场景 A 用它识别"同事"）。Persona 的知识不自动注入，请求前缀保持稳定，信任边界也不变（工具结果按外部内容对待）。画像是推断，与觉察一样不写入 Memory。

### 3.4 接口

| 接口 | 谁调用 | 说明 |
|---|---|---|
| MCP 工具 | 内核（Agent 的调用） | 现有的 `find_related`、`file_changes`、`day_ledger`、`file_day`、`day_summary`、`set_status`、`update_commitments`、`list_commitments`、`commitment_history` 等，加上 `save_note`、`entity`、`search`、`watch`、`goals`；只读的在登记中声明只读，交出结果的以 `no_approval` 放宽（ADR-0017），登记开启 `cite_captures` |
| 查询与改动 API | 应用层的 BFF | App 的项目、待办、笔记、实体页面；用户的改动记为作者为用户的变更集 |
| 对外 MCP（OAuth） | 三方 AI | 只读加提议写入，不含旁听原文（[M7 Agent 生态](./m7-agent-ecosystem.md) §3） |
| 评测夹具契约 | `internal/eval` | HTTP 上的 Seed、State 与用户、业务线的操作（确认归属、改承诺、绑定 Session 等）；评测通过独立进程接入，同一套用例可指向内存实现或生产实现。评分逻辑留在 `internal/eval`，夹具不提供 Check：被测方不做自己的裁判 |

领域核心（变更集检查、期限换算、核对、回执文字）只有一份，生产与评测共用，只是存储不同。回执是给模型看的文本，属于被测行为（[架构评审](../review/2026-10-26-xinli-architecture.md) §3.1）。

## 4. xinli 应用层

### 4.1 采集与识别

- 领夹经蓝牙把音频交给 HostApp，HostApp 把音频分片上传到采集服务；采集服务做流式识别、说话人区分、声纹比对与个人词库热词，再以服务令牌调用 `SubmitCaptures`。内核仍只收文字（ADR-0036）。
- 声纹比对出已登记的说话人时，片段带上 `speaker_ref`（Persona 的实体 ID，[M7](./m7-kernel-primitives.md) §10）。来源仍只看佩戴者还是他人：认出来的他人仍是他人。
- **音频默认不保留**，只在识别完成前缓存。若以后保留，保留期与那天的旁听 Session 对齐、一起删除。
- 网络中断时 HostApp 本地缓冲，恢复后以同一 `capture_id` 重交（去重已有）；采集服务故障同样由 HostApp 缓冲。

### 4.2 账号、设备、声纹与权益

账号体系签发业务线令牌（[鉴权](./auth.md)）。会员档位变化时以服务令牌设置内核中 EndUser 的档位（[M7](./m7-kernel-primitives.md) §8），权益本身（活动、保修）留在应用层。声纹只在应用层保存，删除声纹不影响已提交的 Capture。设备状态提醒由推送服务直接发出。

### 4.3 渠道

微信、飞书、TeamTalk 的适配器是 `apps/` 下可复用的渠道模块：入站以服务令牌提交到 EndUser 的问答线程；出站消费出站流中的回复。渠道账号到 EndUser 的映射由 xinli 保存。卡片、提问降级为"文字 + 深链"；**审批一律回到 App 完成**，第三方渠道不能批准。

### 4.4 推送

两条路分开：
- **业务事实的提醒**（承诺到期、设备状态、权益）：推送服务读 Persona 或账号数据直接推，不经模型，不用内核的 Notification（与现在的承诺提醒相同）。
- **Agent 生成的主动内容**（心力优先、周报、定时执行的结果）：定时在长期的 `xinli-brief` Session 中开启 Run，结果经出站流交给推送服务，再分发到 App 或渠道。
- 内核的 Notification 仍只用于审批、提问与长 Run 结束。

### 4.5 编排器

Session 内"何时做"由内核负责（触发、定时）；跨 Session 的扇出由编排器消费出站流完成：

| 出站流中的事件 | 编排器做什么 |
|---|---|
| 当天整理的 Run 完成 | 为涉及的项目创建或复用 `xinli-project` Session，提交"整理 <日期>" |
| 每周回顾的 Run 完成 | 为本周有新交谈的项目提交"重写现状" |
| 擦除 | 通知 Persona 按依据级联删除 |
| 问答线程的回复、提问 | 交给渠道或推送 |

编排器只读出站流中的 ID，再按需读事件；按事件 ID 去重，至少一次投递下重复处理是幂等的（输入 ID 由事件派生）。内核不新增跨 Session 写入（ADR-0037 不变）。

## 5. 功能落位

| 功能 | AgentDef 与 Session | 怎么开启 | 用到的 |
|---|---|---|---|
| 全时记 | `xinli-day`，每人每天一个 | 白天：触发；当天整理：定时，带静默条件（[M7](./m7-kernel-primitives.md) §5） | `captures`、Persona 的 `file_changes`、`day_ledger`、`file_day`；关注项随 `find_related` 召回 |
| 用户标记重点 | 同上 | App 经 BFF 把标记（当地时刻、可选的一句话）写入 Persona | `find_related`、`day_ledger` 返回落在时段内的标记，整理时作为重点写入交谈；不新增流种类（不满足 §9 的判据） |
| 实况预览 | 无 Session | App 打开时，BFF 每分钟读最近的旁听，调用单次调用 `xinli-live-brief` | 单次调用、`GET /v1/sessions/{id}/captures` |
| 随口记 | `xinli-note`，每人一个、长期 | 用户输入 | Persona 的 `save_note`；话中有请求时同一 Run 处理（如创建定时） |
| 内容纠错 | 无 | App 中修改原文 → 原文修订；修改摘要或知识 → Persona 中用户的变更集；新词进词库 | 原文修订、Persona |
| 个人记忆 | `xinli-day` 整理时消解实体；`xinli-week` 合并重复实体（提议，用户确认） | 触发、定时 | Persona 的 `entity`、`find_related` |
| 智能问答 | `xinli-assistant`，**每个话题线程一个 Session** | 用户输入 | Persona 的查询工具、Memory 召回、重答、`show_card`；收藏在应用层 |
| 项目跟踪 | `xinli-project`，每个项目一个 | 编排器 | 现有设计 |
| 待办 | 承诺加用户手动添加的待办，都在 Persona 的账本中 | — | 现有设计；用户添加的记为作者为用户的变更集 |
| 定时提醒与执行 | 由创建它的 Session 承载（问答线程、随口记），或 `xinli-brief` | 定时 | 定时工具 `schedule` |
| 未来录音关注 | 随口记或问答中创建 | 用户输入 | Persona 的 `watch` |
| 心力优先 | `xinli-brief`，每人一个、长期 | 每个工作日的定时 | Persona 的 `goals`、`project_digest`、`list_commitments` |
| 个性化理解 | `xinli-week` 归纳画像 | 每周的定时 | 基础资料与明确的偏好进 Memory（本人）；画像在 Persona |
| 周期洞察与成长觉察 | `xinli-week`，每人每周一个 | 每周的定时 | 现有设计；练习建议作为觉察的一部分 |
| 数据控制 | — | App | 擦除；删除某天、某个项目、某个线程（删除 Session）；删除实体或知识来源（Persona 的变更集与级联）；画像与觉察开关（应用层配置，关闭时编排器不再提交，Persona 停止返回） |
| 外部 AI 授权 | — | 三方调用 | Persona 对外的 MCP |

问答线程按话题分 Session：历史列表就是 Session 列表（`GET /v1/sessions` 按 AgentDef 筛选），删除一个线程就是删除一个 Session，重答只在本线程中生效。连续性靠 Memory 召回与 Persona，不靠一个无限长的 Session。

## 6. 关键流程

### 6.1 一天

1. 当地一天的第一个 Capture 之前，应用层创建当天的 `xinli-day` Session，并以服务令牌创建当天整理的定时：窗口当地 18:00–次日 04:30，窗口内流上静默满 2 小时即执行，否则在 04:00–04:30 之间随机执行（[建模](./xinli-model.md) §12 的规则，现在由内核执行）。
2. 采集服务持续提交 Capture。说完一段话、静默 `quiet` 后，触发开启"整理交谈"，Run 逐时段交出变更集。
3. 定时开启"整理今天"：读完余下的时段、当天整理、`file_day`。
4. 出站流上出现这次 Run 完成，编排器归集到各项目。
5. 应用层关闭当天的 Session；7 天后原文随 Retention 删除，交谈、承诺与 Memory 保留。

### 6.2 删除一段谈话

1. 用户在 App 中选中 10:15–10:40 删除，BFF 调用内核的擦除（引用范围）。
2. 内核先写擦除事件、同时中断当天 Session 中进行中的整理，再物理清空片段正文与修订（[M7](./m7-kernel-primitives.md) §3）；之后任何读取、Transcript 的渲染都只见占位。读旁听的 AgentDef 都是 `scope: run`，之后的整理不会从旧回复或压缩摘要中看到被擦除的内容。BFF 立即在 App 中隐藏相关的交谈与承诺。
3. 出站流上出现擦除事件，编排器通知 Persona；Persona 记下墓碑、按依据删除只依据这段的交谈与承诺，依据不全的不再可读；之后引用这段的变更集被拒绝。
4. 这段谈话写入的 Memory：擦除的结果中，内核列出同一 Session 中依据落在擦除范围内的 Memory（用写入闸门的依据判定，[M7](./m7-kernel-primitives.md) §3），App 询问用户是否一并删除。内核不自动删除，因为同一条 Memory 可能还有其他依据。

### 6.3 定时执行

用户在问答中说"每周五下午五点给我一份本周项目周报"：Agent 调用 `schedule` 在本线程创建定时；每周五到期时，内核以平台来源开启 Run，输入是创建时写下的文字；Run 用 Persona 的查询工具生成周报；出站流交给推送服务。用户说"取消周报"时，Agent 列出并取消。

## 7. 容量与性能

按首年 10 万 DAU 设计，按 100 万验证可扩展（每人每天约 8 小时旁听、十几万字）：

| 项 | 10 万 | 100 万 | 处理 |
|---|---|---|---|
| Capture 正文 | 约 50 GB/天，7 天约 350 GB | 约 500 GB/天，约 3.5 TB | 移出事件日志，按天分区、整片删除（ADR-0044） |
| Capture 写入（峰值） | 全天平均约 550 批/秒，旁听集中在 8 小时，峰值约 1700 | 平均约 5500，峰值约 17000 | 按 Session 分片；一批一个事务写事件与至多 200 行片段；超过单分片容量前须实现分片路由（[M7](./m7-kernel-primitives.md) §13） |
| 内容安全 | 每批一次，整批被拦下时逐段复核 | 同写入 | 外部提供商的容量按峰值与复核放大采购 |
| 整理 Run | 每人每天几十次（触发）＋1 次当天整理 | 同比 | 后台工作类别；准入让交互优先；当天整理按各人作息分散 |
| 停放的定时 | 每人若干个长期定时 | 百万级 | 远期的到期放独立的到期表，认领只扫可认领项；P2 压测 |
| Persona 写入 | 每人每天约几十个变更集 | 同比 | 独立 PG，按 EndUser 分片 |
| 实况预览 | 只在 App 前台时每分钟一次单次调用 | 同比 | 单次调用有并发子限额与提供商预算上限，后台保有最低份额 |
| 出站流 | 每个 Run 一两条 | 同比 | 按分片的游标，只含 ID |

成本的大头是识别与模型调用，不是存储。`captures` 的工具结果只存引用之后，日志中不再有原文副本，存储再省约一半。以上是估算，单分片容量、片段写入与逐 Session 删除的 WAL 成本以 P1、P2 的压测为准；Capture 正文的分区方案是缓解，不达标时再评估移出 PostgreSQL。

## 8. 稳定性与故障域

| 故障 | 影响 | 兜底 |
|---|---|---|
| 采集服务或识别提供商不可用 | 新的旁听暂停进入 | HostApp 本地缓冲，恢复后以同一 `capture_id` 重交 |
| 内容安全不可用 | Capture 与输入被拒绝（fail closed） | 同上 |
| Persona 不可用或拒收 | 交出失败；模型可能仍正常结束 Run | 没有回执的时段不推进读取位置（交付确认），下一次触发或当天整理补上，原文 7 天内都在 |
| 编排器落后或崩溃 | 归集、渠道回复延迟 | 出站流的游标保存在编排器侧，恢复后接着拉；处理幂等 |
| 内核单元故障 | 该业务线所有 Session | 内核已有的租约接管、同步复制（[部署](./deployment.md)） |
| 模型提供商限流 | 后台整理排队 | 准入让交互优先；单次调用独立限流 |

故障域按层分开：Persona 与内核不共用数据库，应用层的任何组件故障都不影响内核写日志。

## 9. 扩展范式

新功能按这个清单做，默认不改内核：

1. **Persona**：新的实体类型或变更集操作，及其检查与回执；
2. **工具**：Persona 的 MCP 工具，声明是否只读；
3. **AgentDef**：新增或修改，写明能力白名单；
4. **编排**：由触发、定时，还是出站流驱动；
5. **评测**：用例，以及 Persona 夹具的一致性测试。

每一步的验收都要回答四个问题：新内容的**来源**是什么（本人、他人、外部、平台）；它存在哪里、怎样随 Session、EndUser 或擦除**删除**；三方能否读到、经哪项**授权**；重试与重交是否**幂等**。

**改内核的判据**：第二条业务线也会需要，**并且**业务侧做不到或做了不安全。满足时写 ADR，在 [M7](./m7-kernel-primitives.md) 这类设计中记录理由。本次新增的每一处都按这条判据记录（§2.3）。

persona agent 的演进按同一范式（ADR-0043）：

- **一个前台、几个专员。** 用户面对前台（问答线程），它直接使用 Persona 的工具；重的活（报告生成、项目深度回顾）经委托交给专员（ADR-0031），子 Run 的能力不超出前台的白名单。后台整理不归前台调度，仍由触发、定时与编排器开启。
- 演进就是给前台加工具、加专员，接入三方 Agent（[M7 Agent 生态](./m7-agent-ecosystem.md)），内核不变。

## 10. 代码布局

```
proto/ gen/ sdk/          内核契约（不变）
internal/                 内核
persona/                  Persona 层（独立 Go module，模块路径 persona）：领域核心、PG 与内存存储、MCP、查询 API、评测夹具
  CONTEXT.md              Persona 术语
apps/xinli/               xinli 应用层（独立 Go module，模块路径 xinli）：采集、账号、渠道、推送、编排器、BFF
  CONTEXT.md
agents/xinli-*.yaml       AgentDef（随应用层发布，仍由内核加载）
evals/                    评测用例；夹具经契约指向 Persona
```

- 原模拟业务线的领域逻辑已迁入 `persona/`，编排部分（归集、每周回顾的提交）迁入 `apps/xinli` 的编排器；内核不 import 二者，所以夹具是独立进程：`apps/xinli` 提供开发用的 `xinli dev`（Persona 的内存实现、MCP、夹具 HTTP、编排器，以及原先控制台的"旁听"视图），评测与本地开发服务启动它，按它给出的地址登记 MCP。原进程内业务开关与业务调试路由已删除，内核侧只剩通用的 MCP 登记与 HTTP API；控制台不再展示业务实体。
- Persona 对线上格式（`_meta` 的身份与 `cite_captures`、`#seq.index` 引用、交付回执）各自实现一份，不 import 内核的包；一致性由端到端的 `capture-*` 评测保证。第二条业务线也要写 MCP Server 时，再考虑把它们提升为公开包。
- `make check` 增加依赖方向检查：`persona/`、`apps/` 的 `go list -deps` 中不得出现 `yanshi/internal/`。模块路径不以 `yanshi/` 开头，否则 Go 的 internal 规则不起作用。
- 业务侧的设计文档在 `docs/design/` 下以 `persona-`、`xinli-` 为前缀；ADR 仍用统一编号。

## 11. 落地顺序

现在不对真实用户开放（[10-27 评审](../review/2026-10-27-xinli-kernel-next.md) §0），所以先打牢边界与合规，再做产品功能。

| 阶段 | 内容 | 验收 |
|---|---|---|
| P0 边界（实现已完成） | 模拟业务线迁成 `persona/`（内存存储）；评测夹具契约；依赖方向检查；术语拆分 | `make check` 全绿；`make eval` 的 `capture-*` 与基线一致 |
| P1 内核：数据、合规与可靠通知 | Capture 正文存储、`captures` 结果只存引用与统一展开器；擦除与原文修订；交付确认；出站流（含删除信封与对账）；EndUser 删除屏障；Persona（内存实现）的擦除墓碑与回执 | `make check`、`make sim`；删除不变量覆盖擦除与注销；擦除跨库竞态；消费者离线超过 72 小时后对账；`make eval` 与基线一致 |
| P2 内核：驱动 | 触发（ADR-0040 第 2 步）；定时与到期表 | 同上，加模拟测试的触发、定时不变量；百万停放压测 |
| P3 内核：补充 | 单次调用；档位；重答；`speaker_ref` | 同上；单次调用的评测；子限额下问答首字延迟不退化 |
| P4 生产服务 | Persona 的 PG 存储与检索；应用层的采集识别、账号设备、渠道、推送、编排器、BFF；内核的分片路由 | 夹具契约在内存与 PG 实现上结果一致；端到端一天；故障演练：断网补传、Persona 故障、出站流消费者落后、数据库切换 |
| P5 Persona 新能力 | 实体与时间线、笔记、关注项、目标与心力优先、画像、词库 | 每项新增评测用例 |
| P6 生态 | 先做授权上下文与出境规则，再接协议：A2A 出站能力 → A2A 入站与提议写入 → 托管（注册中心里程碑） | [M7 Agent 生态](./m7-agent-ecosystem.md) §7 |

真实内容安全接入随开放真实用户的时间（[上线清单](../launch-checklist.md)）。

## 12. 不在这一版

- **跨业务线共享 Persona**：先按业务线隔离；第二个 persona 类产品出现时，按 Grant 的方式扩展。
- **端侧识别**：先用云端识别；端侧按章程 §3 的"规则 → 经典 ML → 小模型"另行评估。
- **音频保留与回放**：默认不保留（§4.1）。
- **采集的非文字种类**（照片、位置）：每一种另立 ADR（ADR-0036）。

## 13. 决定

讨论记录（2026-10-27），各题的推荐均被采纳。

| # | 决定 | 选择 | 没有选的 |
|---|---|---|---|
| Q1 | 分层 | 内核 / Persona / 应用三层，Persona 是内核之外、按业务线分租户的领域服务 | Persona 并进 xinli 后端（第二个产品重写）；扩展内核 Memory 为图谱（业务语义进内核） |
| Q2 | 容量目标 | 首年 10 万 DAU，按 100 万验证 | — |
| Q3 | 识别 | 应用层的采集服务，内核只收文字；音频默认不保留 | 内核收音频（改写 ADR-0036） |
| Q4 | 部分删除 | 物理删除；内核新增擦除；纠错为只追加的修订 | 按小时切 Session（粒度仍粗，打乱整理） |
| Q5 | 定时 | 内核原语，到期输入的来源为平台 | 业务侧提交（来源洗白） |
| Q6 | 无会话的模型调用 | 内核的单次调用，计量、内容安全、评测 | 业务侧直连提供商 |
| Q7 | 业务线出站 | 内核的拉取式出站流，webhook 只作提示 | 每个 Session 一条 SSE |
| Q8 | 代码 | 同仓库、独立 Go module，夹具契约 | 独立仓库（评测与领域核心难共用） |
| Q9 | 实体与 Memory | 实体在 Persona，`speaker_ref`；Memory 只存本人 | 他人信息同时写 Memory 与 Persona |
| Q10 | 问答、重答、随口记 | 线程即 Session；内核的 `redo_of`；随口记是输入 | 一个无限长的助手 Session；"请重新回答"式的输入 |
| Q11 | 会员分档 | 内核执行档位配额，权益在应用层 | 业务侧自己限流（触发与定时的 Run 管不到） |
| Q12 | 渠道 | 应用层可复用模块，审批只在 App 内 | 渠道内审批 |
| Q13 | 外部 AI | Persona 对外 MCP，不含原文 | 开放原文 |
| Q14 | 跨 Session 编排 | 应用层编排器消费出站流 | 内核的跨 Session 写入原语 |
| Q15 | Capture 存储 | 正文移出日志、按天分区 | 留在日志中逐 Session 删除（表膨胀） |
| Q16 | Persona 存储 | 独立 PG、按 EndUser 分片、变更集日志 + 物化表、混合检索、依据 | 共用内核数据库；引入 ES |
| Q17 | persona agent 形态 | 一个前台、几个专员 | 一个 Agent 包揽全部工具；前台调度后台整理 |
| Q18 | 主动触达 | 业务事实直推；生成内容经 `xinli-brief` 与出站流 | 都经内核的 Notification |
| Q19 | 扩展范式 | 五步清单；改内核须满足判据并写 ADR | — |
| Q20 | 术语与命名 | 内核术语留在 `CONTEXT.md`，业务术语迁出；Erasure、CaptureCorrection、Schedule、Oneshot、Outbox、Tier、Redo | — |
| Q21 | 落地顺序 | P0–P6（§11） | 先做产品功能 |
| Q22–Q27 | 三方 Agent | 见 [M7 Agent 生态](./m7-agent-ecosystem.md) §8 | — |

P0 动工前的讨论（2026-10-27），各题的推荐均被采纳：

| # | 决定 | 选择 | 没有选的 |
|---|---|---|---|
| Q28 | 先做什么 | 先做 P0；P1 中取决于原文放在哪里的部分（正文存储、擦除、修订、出站流的擦除信封）等[边界评审](../review/2026-10-27-capture-boundary-review.md)的 A/C 裁决 | 直接按现行 M7 做 P1 |
| Q29 | 原文保留期能否由业务线担保 | 可以接受；P0 之后做边界评审 §5 的三步验证，再写 ADR 裁决 A 与 C | 不接受（直接定 A） |
| Q30 | 评测怎样拿到 Persona | 子进程（`xinli dev`），按地址登记 MCP | `go.work` 让 eval import persona（违反依赖方向）；把 eval 移出内核模块 |
| Q31 | 线上格式的小函数 | Persona 各自实现 | 提升为内核的公开 Go 包 |
| Q32 | 编排放在哪里 | P0 就拆进 `apps/xinli` 的编排器；出站流就位前由 Persona 的进程内通知驱动 | 留在 Persona 的夹具里到 P4 |
| Q33 | 夹具契约 | Seed、State 与操作；评分留在 eval | 夹具提供 Check |
| Q34 | 控制台的"旁听"视图 | 搬进 `xinli dev` 自带的页面 | 内核的通用调试反向代理；去掉 |

讨论后修正一处：Q11 原拟由令牌带 `tier` 声明，但触发与定时开启的 Run 没有用户令牌，准入时拿不到档位，改为由业务线以服务令牌设置、内核按 EndUser 保存（[M7](./m7-kernel-primitives.md) §8）。

Codex 两轮评审后的修订（[评审记录](../review/2026-10-27-persona-architecture-review.md)）：新增交付确认与 EndUser 删除屏障；出站流按事务可见性水位发布、删除信封不随 Session 删除；擦除中断进行中的 Run、Persona 以墓碑拒绝迟到的提交；统一展开器；分区只回收空间、删除仍逐 Session 精确执行；Q27 收窄为"旁听原文不开放、听过他人旁听的 Session 不能调用三方"；落地顺序把可靠通知与删除闭环提前到 P1。
