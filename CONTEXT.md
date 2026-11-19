# yanshi

yanshi 是承载多条 C 端业务线的分布式 Agent 运行时基础设施。本文件是内核术语表：内核的代码、注释、文档使用同一套名词。

业务线的领域术语见 [Persona 术语](persona/CONTEXT.md) 与 [xinli 应用层术语](apps/xinli/CONTEXT.md)；内核不定义业务实体（ADR-0043）。

## 主体

**BusinessLine（业务线）**:
接入 yanshi 的一条 C 端业务，是租户隔离、配额与计费的单位。
_Avoid_: 租户（泛指时）、客户、应用

**EndUser（终端用户）**:
使用某条业务线产品的自然人；同一 EndUser 可同时使用多条业务线。
_Avoid_: 用户（有歧义时）、账号、客户

## 节点与能力

**Device（设备）**:
EndUser 持有的物理终端，例如手机或电脑。只经某个 HostApp 接入的外设（如领夹录音器）不是 Device：yanshi 只看到那个 HostApp。
_Avoid_: 端、终端、客户端

**HostApp（宿主应用）**:
嵌入了 yanshi 端侧 SDK 的业务线 App；一台 Device 上可以有多个 HostApp。
_Avoid_: 客户端、插件

**Node（节点）**:
向 yanshi 提供 Capability 的执行端点。Device 上的每个 HostApp 实例、每个 Sandbox、每个云端服务连接器都是一个 Node。
_Avoid_: Worker、Agent、端

**Sandbox（沙箱）**:
由 yanshi 在云端托管、隔离的代码执行环境，每个 Session 一个，是一种 Node；其中只运行不可信代码，由沙箱外的受信控制器驱动。
_Avoid_: 容器、VM、执行器

**Workspace（工作区）**:
Sandbox 中跨 Run 保留的文件系统（`/workspace`）；Sandbox 空闲回收时计算资源释放而工作区保留。
_Avoid_: 磁盘、卷、目录

**Connection（连接）**:
HostApp 实例与网关之间的一条长连接（WebSocket）。它可以作为 Node 声明 Capability，也可以作为会话客户端订阅 Session、提交输入与审批，或两者兼任（ADR-0024）。
_Avoid_: 通道、长链接、Socket

**Presence（在场）**:
哪些 Device 此刻订阅着某个 Session，以及它是否显示在前台、用户是否正在输入。易失、带有效期，不进 Event 日志。
_Avoid_: 在线状态（指 Node 是否连接时）、已读

**Question（提问）**:
Agent 经 `ask_user` 向 EndUser 提出的、可点选或填写的问题；Run 挂起等待，回答作为该调用的结果回到 Run。
_Avoid_: 表单、弹窗、确认框（与 Approval 区分）

**UIContext（界面上下文）**:
EndUser 提交输入时所在界面的内容（界面、对象、选中文字、可见内容），由 HostApp 附在输入上，帮助模型理解"这个""这里"指什么；按不可信数据对待。
_Avoid_: 屏幕内容、页面状态

**Notification（提醒）**:
需要 EndUser 注意（待审批、待回答的提问、长 Run 结束）而他没有在看时，推送到他最近使用的一台 Device 的消息；只含种类与 ID。
_Avoid_: 通知（泛指时）、推送（指通道时）、消息

**Inbox（收件箱）**:
某个 Node 待投递的 Capability 调用集合；Node 上线后由网关投递，结果回写日志后移出。
_Avoid_: 队列、任务列表

**Capability（能力）**:
Node 声明的一项可被 Agent 调用的动作，带有输入输出契约和风险等级。
_Avoid_: 工具（指声明时）、插件、函数

## 执行

**AgentDef（Agent 定义）**:
一份版本化的声明，描述一个 Agent 的指令、技能、可用 Capability、模型策略与预算。
_Avoid_: Bot、智能体配置、Prompt

**Release（发布配置）**:
某个 AgentDef 名下各版本的发布状态：稳定版本、灰度版本（Canary）与比例、撤回的版本；新 Session 据此决定使用哪个版本。
_Avoid_: 上线配置、版本策略

**Canary（灰度版本）**:
按比例（按 EndUser 稳定分流）或白名单分给一部分 EndUser 的 AgentDef 新版本，用于在全量之前比较效果。
_Avoid_: 实验组、Beta

**Withdraw（撤回）**:
回滚一个 AgentDef 版本：新 Session 不再使用它，已有 Session 在下一个 Run 开始时切换到稳定版本。
_Avoid_: 下线、删除版本

**Session（会话）**:
EndUser 与某个 AgentDef 之间长期存在的交互容器，可被多个 Device 同时接入。
_Avoid_: 对话、聊天、线程

**Run（运行）**:
Session 内由一次输入触发的一段执行，可被插话与中断，终态为 completed、failed 或 interrupted。一个 Session 同一时刻至多一个未终态的 Run。
_Avoid_: 任务、Job、Turn

**Trigger（触发）**（已取消，ADR-0050，现 ADR-0052）:
原先设计为内核按流静默开启 Run；现由业务线判断旁听静默并提交固定整理输入。按钟点的定时见 Schedule。
_Avoid_: 定时任务、cron、事件订阅

**Schedule（定时）**（已实现，[设计](docs/design/m7-kernel-primitives.md) §5、ADR-0045）:
属于一个 Session、按钟点开启 Run 的声明：一次性或周期，时区由创建方给出（默认取 Session 的时区），没有静默窗口。经 HTTP 或 Agent 的 `schedule` 能力创建；到期时平台写入输入，来源是平台，不是用户原话。只在本 Session 内开启 Run。
_Avoid_: cron、定时任务、提醒（指业务线按承诺期限推送时）

**Oneshot（单次调用）**（已实现，[设计](docs/design/m7-kernel-primitives.md) §7、ADR-0047）:
没有 Session 的一次模型调用：声明式的提示词与输出结构，一次输入、一次输出，没有工具与状态；照常计量、配额与内容安全，不写日志、不存结果。输入只收文字；取嵌入向量的种类推迟到 Persona 的向量检索。
_Avoid_: 函数调用、补全接口、Task

**Outbox（出站流）**（B3/B4 已实现，[设计](docs/design/m7-kernel-primitives.md) §6、ADR-0046）:
业务线服务端按分片游标拉取的选定运行事件信封序列；至少一次，内容按 ID 读事件。用于没有在线客户端时可靠发现回复、提问与审批、维护跨 Session 收件箱、渠道转发及停机后续消费。普通信封与事件原子提交，删除信封与删除记录原子提交；不承担业务事实接受或实时增量。P4 的范围与恢复协议见设计 §6。
_Avoid_: webhook、消息队列、事件订阅

**Outbox envelope（出站流信封）**（已实现，ADR-0046）:
出站流的一条稳定 ID 记录，只含 ID 与运行元数据；普通信封随 Session 删除。session_deleted 表示逻辑删除，不含 EndUser 或 Event seq，不随 Session 删除，按出站保留地板淘汰；消费方按信封 ID 去重。
_Avoid_: Event（删除信封不是事件）、通知正文

**Loop（循环）**（[设计](docs/design/m5-inputs-triggers-loops.md)、ADR-0041）:
决定 Run 每一步做什么的部分：内置的循环调用模型，外部循环请求业务线登记的服务。Loop 只给出回复与调用请求，动作都由平台执行。
_Avoid_: 策略（那是 Policy）、插件、编排

**Attempt（尝试）**:
某个 Worker 对一个 Run 的一次执行；故障接管即开启新的 Attempt。Attempt 号是写入日志的 fencing 令牌。
_Avoid_: 重试、执行实例

**Call（通话）**:
Session 内的一段全双工实时音视频交互；耗时工作由 Call 派生 Run 完成（run_task，ADR-0027）。音频不进日志，双方每句话的转写（CallTranscript）进日志。代码中"调用"（call_id、ToolCall、`session.Call`）指一次 Capability 调用，与 Call 无关：表示通话的标识符一律带 Call 前缀（`CallStarted`、`CallEvent`、`State.ActiveCall`、`from_call`）。
_Avoid_: 实时会话、语音模式、Live

**Event（事件）**:
Session 中只追加、不可修改的一条记录；Run 的全部状态都可由 Event 重建。
_Avoid_: 消息、日志条目

**Capture（旁听）**:
EndUser 与他人的日常对话转写，带结构化说话人标签，不是对 Agent 说的话。原文由业务线保存，内核只接收可信结构化页并在写日志前审核；佩戴者的话可作用户原话，他人的话按外部内容处理。原文副本随工作 Session 删除。旧的内核 CaptureAppended 事件已删除，旁听片段只出现在 attested 结果中（ADR-0052）。
_Avoid_: 录音、输入（指它时）、CallTranscript（那是 Call 的转写）

**Erasure（擦除）**（内核原语已实现，业务侧流程另行实施，ADR-0052）:
EndUser 删除一天中的部分谈话：业务线删除原文、按依据级联派生对象，再删除当天的工作 Session，删除原因为 Erasure（`erasure`）：保留该 Session 的既有 Memory，撤回迟到的新写入，清理后列出依据可能落在被删范围内的有效 Memory 由用户决定。内核不按范围清除日志中的文字（ADR-0044 已取代）。
_Avoid_: 删除（指整个 Session 时）、屏蔽、隐藏

**Ack（交付确认）**（已退役，ADR-0050，现 ADR-0052）:
原先用于按业务线回执推进内核读取位置；现由业务线在保存整理结果的同一事务中推进处理进度，内核不再登记或消费回执。
_Avoid_: 回执（指给模型看的结果文字时）、ACK（指传输层时）、提交

**CaptureCorrection（原文修订）**（不实施，ADR-0050 取代 ADR-0044，现 ADR-0052）:
EndUser 对一段旁听文字的纠正。原文存放在业务线，纠正是业务库中原文的修订，内核没有对应的 Event；纠正前的文字在日志中仍可作为依据，要撤回只能删除工作 Session。
_Avoid_: 编辑、覆盖、Revision（见 [Persona 术语](persona/CONTEXT.md)）

**Attested result（可信结构化片段）**（已实现，ADR-0052）:
`attests_speakers: true` 登记的同业务线 MCP Server 返回的结构化旁听页（片段的引用、说话人、时间、文字与页的元数据）。由平台用旁听渲染器渲染并在 ToolResult 上记下 attested 标记，按结构化说话人推导来源；写日志前经内容安全检查。
_Avoid_: 业务线返回的旁听文字、可信结果（不指明结构化时）

**Stream（流）**（已退役，ADR-0050，现 ADR-0052）:
原先按种类区分内核采集序列；AgentDef 的流声明、transcript 流和内核读取位置均已删除。
_Avoid_: 频道、Topic、队列

**Span（时段）**（[设计](docs/design/xinli-model.md)、Persona ADR-0001）:
相邻两段说话时间相隔不超过间隔的一段连续旁听，由业务线按停顿确定性地切出，是 `captures` 分页与判断"是否已结束"的单位，不承担语义。
_Avoid_: 交谈（指它时）、窗口

**ContentBlock（内容块）**:
Event 中承载一段多模态内容（文本、图片、音频、视频、文件引用）的单元。
_Avoid_: 消息体、Part

**Compaction（上下文压缩）**:
把 Session 中较早的历史替换为一段摘要，记录为 Event；之后的模型上下文由"最新摘要 + 其后的历史"组成。原始 Event 不删除。
_Avoid_: 截断、记忆（与 Memory 区分：Compaction 只作用于本 Session 的模型上下文）

**Artifact（工件）**:
属于某个 Session、不可变、可被独立引用和下载的文件（录音、图表、表格等）；Event 中只以 `artifact://<id>` 引用，不内嵌内容。
_Avoid_: 附件、输出、文件（泛指时）

**Interrupt（中断）**:
EndUser 让进行中的 Run（或 Call 中的发言）立即停止；已产生的输出保留，Session 可继续接收输入。
_Avoid_: 取消、停止

**Suspend（挂起）**:
Run 等待外部条件（审批、设备结果）时，当前 Attempt 正常结束、不占用任何 Worker；条件满足或超时后由任意 Worker 恢复。
_Avoid_: 暂停、阻塞、睡眠

**Background Call（后台调用）**（ADR-0035）:
路由到设备的调用在设备离线超过宽限期（默认 20 秒）后，Run 不再等它（委托超过目标 AgentDef 的 `delegation.background_after`，默认 60 秒，同样转入后台，至多到委托的截止时间，ADR-0058）：模型得到"已排队"的结果，Run 照常继续，调用在 Session 级等待设备上线，至多到后台时限（默认 24 小时）。设备的结果或到期作为一条输入送达（有进行中的 Run 时并入，否则开启新 Run），用户也可以单独取消它。每个后台调用至多了结一次。
_Avoid_: 异步任务、离线队列、延迟调用

**Steer（插话）**:
EndUser 在 Run 进行中追加输入，以改变其后续方向而不终止它。
_Avoid_: 追加消息、打断

**Redo（重答）**（已实现，[设计](docs/design/m7-kernel-primitives.md) §9）:
EndUser 让 Session 中最后一个符合资格的 completed Run 以同一输入重新回答；调用须有只读证据或为 show_card，其他资格见设计。接受即替代，被替代 Run 不再进入模型上下文、其中卡片动作按 call_id 失效；日志与安全来源保留全部。
_Avoid_: 重试（那是故障后的接管）、重新生成（产品文案可用）

**Worker**:
无状态的执行进程，认领 Session 并以 Attempt 推进其中的 Run。
_Avoid_: Node（Node 提供 Capability，Worker 执行 Run）、执行器

**Card（卡片）**（[设计](docs/design/cards.md)、ADR-0032）:
Agent 经内置工具 `show_card` 展示给用户的结构化内容：标题、Markdown、图片、键值、列表、待填写的字段与按钮。随调用结果写入日志；同一 card_id 再次展示即替换。卡片不阻塞 Run。
_Avoid_: 富文本消息、组件、Widget

**Card Action（卡片动作）**:
用户在卡片上点按钮或点选列表项，连同填写的字段作为一条输入提交；同一版卡片上的同一动作只生效一次。Run 等回答时它就是对提问的回答，动作记在回答的结果中。
_Avoid_: 回调、按钮事件、Postback

**Fault Rule（故障规则）**（ADR-0033）:
调试部署（`serve -faults`）中向本进程注入一种服务端故障的规则：类型（模型错误、限流、流中断、延迟、Worker 崩溃、内容安全不可用、设备目录不可用）、可选的 Session 与次数。
_Avoid_: 混沌实验、Mock 故障

**Delegation（委托）**（[设计](docs/design/m5-multi-agent.md)、ADR-0031）:
一个 Run 把任务交给另一个 AgentDef 执行：在同一 BusinessLine、同一 EndUser 下派生子 Session 与 Run，父 Run 挂起等待其终态作为调用结果。Agent 在模型眼里是一种 Capability（`agent__<名称>`），子 Run 的能力不超出任何祖先的白名单。
_Avoid_: 编排、子 Agent（指 AgentDef 时）、多 Agent（泛指时）

**Artifact Handoff（工件交接）**（ADR-0055）:
委托的父子 Session 之间经平台复制传递 Artifact：父 Run 在委托参数中点名的工件复制进子 Session，子 Run 最后一条回复提到的子 Session 工件复制回父 Session。副本是目标 Session 的新工件，日志记 `ArtifactCopied`；工件本身从不跨 Session 共享。
_Avoid_: 共享工件、移交（Handoff 指 Worker 交出 Run）

**Decision（判断）**（[设计](docs/design/m6-decisions.md)、ADR-0030）:
对"给定状态、有限合法选项"的问题由规则、小模型或 LLM 作出的选择，只缩小候选、追加约束或触发默认路径，从不放宽安全要求；影响后续执行的 Decision 记为 Event。
_Avoid_: 分类、路由（指模型路由时）、System 1（指模型类型时可用）

**Handoff（移交）**:
Worker 主动结束自己的 Attempt，把 Run 交给其他 Worker：以 `handoff` 挂起并立即归还 Session，由任意 Worker 恢复。用于优雅停机，以及只认领交互工作的 Worker 交出已成为长任务的 Run（ADR-0029）。与接管不同，它不是故障，不计入接管次数。
_Avoid_: 迁移、转交、让出

**长任务**:
活跃 Run 已经做了很多工作：模型回合与上下文压缩合计达到 6 次，或累计输出达到 16000 token（ADR-0029）。按已做的工作判定，不按经过的时间；它的工作以 Background 优先级排队，只由通用 Worker 认领。
_Avoid_: 慢任务、后台任务

**恢复名额**:
用户对长任务做出审批或回答后，它在后台池中先被认领的一次机会，由下一个 Attempt 消费，不可叠加（ADR-0029）。它不让长任务回到交互池。
_Avoid_: 插队、加急

## 存储归属

**Shard（分片）**:
承载一部分 Session 或 EndUser 数据的独立存储单元；同一类键的同一个值，其数据归属同一分片。
_Avoid_: 槽位（槽位是映射单位）、业务线（业务线不是内核分片键）

**Slot（槽位）**:
由所属键稳定决定的逻辑归属单位，再由部署配置映射到分片；扩容时改变槽位的归属，不改变键到槽位的规则。
_Avoid_: 分片号、库号

**Control shard（控制库）**:
分片号为 0 的库，保存不含 EndUser 的单元控制数据（删除请求）；槽位表必须登记它。旧格式 ID 的兼容库含义已于 2026-11-15 删除（ADR-0051）。
_Avoid_: 兼容库、槽位 0（它不等于控制库）

## 个性化与治理

**Memory（记忆）**:
关于某个 EndUser、跨 Session 留存的个性化信息；默认归属产生它的 BusinessLine。
_Avoid_: 画像、上下文、知识库

**Persona（个人知识层）**（设计中，[设计](docs/design/persona-architecture.md)、ADR-0043）:
内核之外、关于一个 EndUser 的世界的结构化知识服务：他人与组织、交谈、承诺、项目、笔记，以有效变更集与直接事实为事实源，按业务线分租户。内核不认识它，只经 MCP 调用它的工具；它的术语在 `persona/CONTEXT.md`。
_Avoid_: Memory（那是内核中关于本人的陈述）、画像、知识库、用户模型

**Recall（召回）**:
Run 开始时检索 EndUser 的 Memory（本业务线的，以及经 Grant 可读的其他业务线的）并记录为 Event，作为当前 Run 的上下文；同时也是 Memory 的访问记录。
_Avoid_: 注入、加载（泛指时）

**Grant（授权）**:
EndUser 显式同意某个 BusinessLine 读取其在另一 BusinessLine 下的 Memory。
_Avoid_: 共享、权限

**Consent（单独同意）**（已实现，ADR-0022 修订）:
EndUser 对某类敏感个人信息（目前只有健康信息）的单独同意：由 BusinessLine 在自己的产品中取得，再以服务令牌登记，内核按 EndUser 保存同意时间；撤回即删除登记。业务线开启该类信息只表示有取得同意的流程，每个 EndUser 仍须单独登记。
_Avoid_: 授权（那是 Grant）、审批（那是 Approval）、opt-in、开关（指业务线配置时）

**Approval（审批）**:
执行高风险 Capability 前，由 EndUser 在某台 Device 上做出的单次确认。
_Avoid_: 确认框、授权（与 Grant 区分）

**Token（令牌）**:
由 BusinessLine 签发、证明调用方身份的短期凭证。用户令牌代表一个 EndUser，服务令牌代表业务线服务端；yanshi 只验证，不签发。
_Avoid_: 会话（与 Session 区分）、票据、凭据（泛指时）

**Retention（保留策略）**:
BusinessLine 为其 Session 声明的保留期限：最后一次输入后多久自动关闭、关闭后多久自动删除。可按 AgentDef 名称覆盖，使同一业务线中短期保留的 Session（如旁听 Session）与长期保留的 Session（如业务线按主题长期归集摘要的 Session）并存。
_Avoid_: 过期策略、TTL（泛指时）

**Usage（用量）**:
一次模型调用或沙箱执行消耗的资源（模型 token、执行时长）及按价格表折算的金额；独立于 Event 日志长期保存，不含 Session 与内容。这里的 token 指模型计量单位，与 Token（令牌）无关。
_Avoid_: 账单、消费记录

**Quota（配额）**:
在一个周期内允许的 Usage 金额上限：BusinessLine 每月一份，EndUser 每日一份。超出时拒绝新 Run、挂起进行中的 Run。
_Avoid_: 额度（指剩余量时可用）、预算（与 Policy 中的预算区分）、限流

**Tier（档位）**（已实现，[设计](docs/design/m7-kernel-primitives.md) §8）:
BusinessLine 为 EndUser 设置的配额档次（如会员等级），决定其每日 Quota；由业务线以服务令牌设置、内核按 EndUser 保存。权益本身由业务线管理。
_Avoid_: 会员、套餐、等级（指权益时）

**PriceList（价格表）**:
部署配置中每个模型的 token 单价与沙箱的时长单价，用于把 Usage 折算为金额。
_Avoid_: 费率、定价策略

**Moderation（内容安全）**:
对 EndUser 输入与模型输出的合规检查，在写日志之前进行；违规输入被拒绝、违规输出被替换为拒答文本。由可替换的提供商完成。
_Avoid_: 审核（泛指时）、过滤、风控

**Provenance（来源）**（[设计](docs/design/m5-inputs-triggers-loops.md)、ADR-0052）:
一段内容由谁产生、是否对 Agent 说：本人、他人、外部、平台（含平台定时）或模型。平台定时不是用户原话，也不是外部内容。由平台从 Event 推导，不由写入方填写；Memory 写入闸门与注入判断都依据它。
_Avoid_: 出处、标签、说话人（那只是旁听中区分本人与他人的字段）

**Policy（策略）**:
约束 Run 能做什么的规则集合，涵盖 Capability 访问、Approval 要求、预算与内容安全。
_Avoid_: 规则、配置
