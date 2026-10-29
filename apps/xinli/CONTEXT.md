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

**Change receipt（变化接收记录）**:
应用已持久接收一个 owner 的某个事实版本及其任务集合的证明，空集合也算接收。Persona 的确认只在这份记录与任务一起保存成功之后推进。
_Avoid_: 领域变化、输入提交凭据

**Input receipt（输入提交凭据）**:
内核已接受某条编排输入的证明，关联输入 ID 与 Run；从事件对账取得时也记录接受位置。它不代表 Persona 已接受领域事实，也不代表 Run 已结束。
_Avoid_: 交付确认、领域事实、处理进度

**Command slot（命令占用）**:
一个后台 Session 当前尚未确认结束的编排命令的占用关系；租约过期、调用超时与本地重启都不会自动释放它。无法排除旧远端写入时，须通过工作 Session 退役后才能切换命令。
_Avoid_: Worker 租约、内核工作项

**Session creation intent（Session 创建意图）**:
编排器调用内核创建工作 Session 前保存的恢复凭据，说明所属任务、目标和删除期限。创建成功不代表绑定成功；未绑定的工作 Session 不接受内容，也没有读取原文的资格。回包丢失时，意图仍保留发现空孤儿的依据。
_Avoid_: 幂等创建、Input receipt

**Empty orphan（空孤儿）**:
内核已创建、但尚未绑定到 Persona 目标且没有内容的工作 Session。它可能来自进程退出、创建回包丢失或竞争绑定失败，由编排器确认资格后清理。
_Avoid_: 退役工作 Session（它可能已经处理过内容）

**Automatic review（自动整理）**:
应用依据业务日固定时区、最后批次的服务端接收时间与处理进度生成的编排任务。白天与日终按原文版本、周回顾按七天起点去重；输入仍是 AgentDef 已支持的固定指令。业务完成须有 Persona 接受事实，日整理还须处理完原文；Run 终态只决定命令能否安全结束。重做次数、下次检查时间与失败类别保存在编排任务中，达到上限或到期也不延长原文期限。
_Avoid_: 内核定时、Run 完成（指业务完成时）

**Kernel TokenSource（内核令牌来源）**:
应用按业务线取得短期内核服务令牌的并发安全接口；可重读静态文件/环境值，或读取业务线私钥签发并在到期前刷新。身份从持久任务 owner 传递，不在共享 Client 上保存可变的当前业务线。
_Avoid_: 应用登录态、Persona MCP 服务凭据

**Account（账号）**:
应用拥有的业务线与 EndUser 身份，状态为 active、disabled、deleting、deleted；包含时区、偏好和带单调版本的权益。禁用撤销全部登录态，注销终态永久保存旧 ID 屏障；同身份重新注册生成新的 EndUser ID。
_Avoid_: 内核 Session、Persona owner 的知识账本

**Installation（安装实例）**:
应用账号绑定的设备 ID、平台、绑定时间和撤销状态。登录态属于安装实例，单设备登出只撤销该实例；领夹不建模为内核 Device 或 Node。
_Avoid_: Node、Connection

**Application session（应用登录态）**:
应用签发的不透明随机访问与刷新凭据；服务端只存 SHA-256。刷新同时轮换两种凭据，撤销与过期逐次检查；BFF 以自己的内核服务令牌代表已认证用户访问内核。
_Avoid_: 内核 Session、内核 JWT

**Account deletion（账号注销）**:
应用持久驱动的跨库清理流程：封禁与撤销、Persona 屏障与清理、内核注销与完成查询、应用清理。仅最后一步完成后为 deleted；独立只读注销查询凭据只存哈希，登录态失效后仍可查询进度。
_Avoid_: 单个 Session 删除、Persona 清理完成

**Account gate（账号操作互斥）**:
同 owner 的应用请求、权益调用与禁用/注销操作共享的串行边界。内存使用互斥通道，PG 使用连接级咨询锁；短事务负责落库，远程调用不持有 SQL 事务。它不等于内核的远端版本条件写入，连接丢失后的迟到请求仍须内核契约提供 fencing。
_Avoid_: Worker 租约、Persona 注销屏障
