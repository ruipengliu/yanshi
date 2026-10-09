# yanshi

yanshi 是承载多条 C 端业务线的分布式 Agent 运行时基础设施。本文件是项目统一术语表：代码、文档、对话中使用同一套名词。

## 主体

**BusinessLine（业务线）**:
接入 yanshi 的一条 C 端业务，是租户隔离、配额与计费的单位。
_Avoid_: 租户（泛指时）、客户、应用

**EndUser（终端用户）**:
使用某条业务线产品的自然人；同一 EndUser 可同时使用多条业务线。
_Avoid_: 用户（有歧义时）、账号、客户

## 节点与能力

**Device（设备）**:
EndUser 持有的物理终端，例如手机或电脑。
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

**Session（会话）**:
EndUser 与某个 AgentDef 之间长期存在的交互容器，可被多个 Device 同时接入。
_Avoid_: 对话、聊天、线程

**Run（运行）**:
Session 内由一次输入触发的一段执行，可被插话与中断，终态为 completed、failed 或 interrupted。一个 Session 同一时刻至多一个未终态的 Run。
_Avoid_: 任务、Job、Turn

**Attempt（尝试）**:
某个 Worker 对一个 Run 的一次执行；故障接管即开启新的 Attempt。Attempt 号是写入日志的 fencing 令牌。
_Avoid_: 重试、执行实例

**Call（通话）**:
Session 内的一段全双工实时音视频交互；耗时工作由 Call 派生 Run 完成。
_Avoid_: 实时会话、语音模式、Live

**Event（事件）**:
Session 中只追加、不可修改的一条记录；Run 的全部状态都可由 Event 重建。
_Avoid_: 消息、日志条目

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

**Steer（插话）**:
EndUser 在 Run 进行中追加输入，以改变其后续方向而不终止它。
_Avoid_: 追加消息、打断

**Worker**:
无状态的执行进程，认领 Session 并以 Attempt 推进其中的 Run。
_Avoid_: Node（Node 提供 Capability，Worker 执行 Run）、执行器

## 个性化与治理

**Memory（记忆）**:
关于某个 EndUser、跨 Session 留存的个性化信息；默认归属产生它的 BusinessLine。
_Avoid_: 画像、上下文、知识库

**Recall（召回）**:
Run 开始时检索 EndUser 的 Memory（本业务线的，以及经 Grant 可读的其他业务线的）并记录为 Event，作为当前 Run 的上下文；同时也是 Memory 的访问记录。
_Avoid_: 注入、加载（泛指时）

**Grant（授权）**:
EndUser 显式同意某个 BusinessLine 读取其在另一 BusinessLine 下的 Memory。
_Avoid_: 共享、权限

**Approval（审批）**:
执行高风险 Capability 前，由 EndUser 在某台 Device 上做出的单次确认。
_Avoid_: 确认框、授权（与 Grant 区分）

**Token（令牌）**:
由 BusinessLine 签发、证明调用方身份的短期凭证。用户令牌代表一个 EndUser，服务令牌代表业务线服务端；yanshi 只验证，不签发。
_Avoid_: 会话（与 Session 区分）、票据、凭据（泛指时）

**Retention（保留策略）**:
BusinessLine 为其 Session 声明的保留期限：最后一次输入后多久自动关闭、关闭后多久自动删除。
_Avoid_: 过期策略、TTL（泛指时）

**Policy（策略）**:
约束 Run 能做什么的规则集合，涵盖 Capability 访问、Approval 要求、预算与内容安全。
_Avoid_: 规则、配置
