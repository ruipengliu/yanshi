# xinli 应用层术语

这是 xinli 应用层的术语表；内核术语见根 [CONTEXT.md](../../CONTEXT.md)，知识领域术语见 [Persona 术语](../../persona/CONTEXT.md)。职责依据见 [Persona 架构](../../docs/design/persona-architecture.md) §2.2、§4。

**Orchestrator（编排器）**:
读取 Persona 持久领域变化与删除待办、执行编排任务的应用层服务：归集项目、推进每日整理与每周回顾、删除与重建工作 Session。内核出站只用于跟踪运行事件、把回复与提问等交给收件箱或渠道；不从 Run 完成推导业务接受。应用按业务日规则调度，内核负责 Session 内的通用定时（[设计](../../docs/design/persona-architecture.md) §4.5、§13 Q99–Q102、Q113–Q116）。
_Avoid_: Worker、调度器（指内核调度时）

**BFF（前端专用后端）**:
面向 xinli App 的应用层后端，组织产品页面所需的查询与用户操作，调用 Persona 的查询与改动 API 及内核公开 API；知识语义由 Persona 负责，Agent 执行由内核负责。P4 产品数据访问只经 BFF，使用可撤销应用登录态，客户端不持有可绕行的内核凭据；BFF 按用途限制对内部工作 Session 的操作（设计 §4.2、Q103）。
_Avoid_: 内核网关、Persona

**Capture Service（采集服务）**:
接收采集批次并把有序文字交给 Persona 的应用层服务（ADR-0050），内核只渲染、审核 Persona 返回的可信页。P4 实现文字采集、持久补传与识别适配契约，识别只有确定性 fake；真实接入后才接收 HostApp 音频并做 ASR、说话人区分、声纹比对和热词处理。音频默认只在识别完成前缓存（设计 §4.1、Q97、Q108、Q114）。
_Avoid_: Capture（那是可信页中的旁听片段）、Node（那是能力执行端点）

**Capture session（采集会话）**（P4 设计，Q97）:
一条持久序号确定顺序的采集与补传序列。P4 只支持一条有序采集会话，服务端暂存缺口，正式按序追加后才确认并返回引用，不重排已有引用；多设备合流后置。它不是内核 Session。
_Avoid_: 工作 Session、Run、Connection

**Capture generation（采集世代）**（P4 设计，Q114）:
采集请求与缺口缓冲的有效世代，和业务日生命周期一起持久化。擦除或删一天推进世代、清除旧暂存并拒收旧请求；到期和注销清全部缓冲正文。客户端收到重新同步状态，不能自动给旧请求换新世代。Persona 暂存区与擦除在同 owner 事务管理正文及世代。
_Avoid_: 原文版本、租约代数

**Orchestration task（编排任务）**（P4 设计，Q99–Q101）:
应用持久保存的跨服务动作，按（来源事实版本、操作、目标）去重，带状态、租约代数、重试时间和提交凭据（receipt）。短事务认领、事务外调用，结果按租约代数与业务对象世代条件提交；启动与周期扫描恢复。结果未知时补读完整所需日志对账，不把内核最近 64 条输入去重当永久幂等。
_Avoid_: Run、Work（内核工作项）、删除待办（Persona 的删除事实）
