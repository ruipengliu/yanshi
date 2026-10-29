# Persona 术语

这是 Persona 层的术语表，内核术语见根 [CONTEXT.md](../CONTEXT.md)。Persona 的代码与文档可使用这两处术语；内核只使用根 CONTEXT.md 的术语。分层依据见 [Persona 架构](../docs/design/persona-architecture.md) §3.1、ADR-0043。

**Project（项目）**:
Persona 中围绕一个主题、跨天归集交谈摘要的对象（[设计](../docs/design/xinli-captures.md)）：有现状、按天的时间线与它的承诺。在内核中对应一个长期的 `xinli-project` Session，由编排器创建并提交整理输入，Persona 记下两者的绑定。旁听原文只短期保留，Project 中只有归入它的交谈的摘要（谈了什么、定了什么、风险）；EndUser 亲口说过的稳定信息（偏好、人际关系）不进 Project，进 Memory。
_Avoid_: 主题、文件夹、知识库

**Exchange（交谈）**（[设计](../docs/design/xinli-model.md)、ADR-0042）:
一段谈话：由整理的模型在一个时段内划定范围 `[from, to]` 交给业务线，ID 取自 `from`（如 `x57.0`）。是摘要、时间线与"今日"页的单位，可以关联多个项目，当天整理时可以合并被停顿切开的几段；承诺不以它为身份，只以它为出处。
_Avoid_: 对话（指 Session 时）、会话、片段（指单句时）、时段（指平台按停顿切出的分页单位时）

**Changeset（变更集）**（ADR-0042）:
一次整理交给业务线的一组操作：新建交谈与承诺、修订或关闭承诺、合并。每个操作引用依据，按来源去重，重交时整体取代并保留原叠加位置；有效变更集按稳定顺序叠加，用户在 App 中的改动也记为变更集。项目、日总览、周回顾等另存直接事实，不是全面不可变事件溯源（[设计](../docs/design/persona-architecture.md) §13 Q92）。
_Avoid_: 提交、批次、补丁

**Revision（修订）**（ADR-0042）:
承诺的一次改动（期限、内容、对方、状态），带依据、说话时间与记录时间；不删除旧值，当前状态由修订依次叠加得出。修订带序号 `rev`，写入方据此做乐观并发检查。
_Avoid_: 版本（指 AgentDef 时）、编辑

**Commitment（承诺）**:
旁听中一方答应另一方要做的事：佩戴者答应别人的（mine），或别人答应佩戴者的（theirs）。随交谈交给业务线，引用原话的片段，由平台给出被引用片段的说话人供业务线核对；业务线保存在承诺账本中，按 ID 增量更新，按期限提醒。
_Avoid_: 待办（指它时）、todo、行动项

**Awareness（觉察）**:
每日整理从 EndUser 本人的话里得出的、对其沟通方式的观察与建议（如"答应别人时会给出明确期限"），随交谈与当天的整理结果交给业务线；每周回顾归纳至少两天出现过的习惯。它是推断，不是 EndUser 说过的话，所以不写入 Memory；情绪与身心状态不属于觉察，只以 health 类别写入 Memory。
_Avoid_: 洞察、insights、画像

**Entity（实体）**:
Persona 中有稳定 ID 的一个人、组织、概念或事件，带别名。承诺的对方、交谈的参与者、项目的相关方都引用实体。

**Note（笔记）**:
EndUser 经随口记口述的内容，是本人的话，整理后保存。

**Watch（关注项）**:
EndUser 事先指定的、之后的旁听中要特别留意的内容（"下周和供应商开会时留意交期"）。整理时随候选一起召回，命中后在交谈中标出。

**Goal（目标）**:
EndUser 明确说过的重要目标，心力优先据此对照近期投入。

**Lexicon（词库）**:
EndUser 的专有词汇（人名、项目名、术语），供识别热词与整理使用。

**Lineage（依据）**:
派生对象所依据的引用（`#批次.段号`、交谈 ID、笔记 ID）。

**Business day（业务日）**（ADR-0050）:
EndUser 当地的一天（`YYYY-MM-DD`），是 Persona 保存旁听原文、编排引用与推进处理进度的单位。每个业务日记下当前与历史工作 Session（`xinli-day`）；擦除后可重建、Session ID 会变，业务日不变。P4 首次接收固定该日时区，期限为次日当地 00:00 加七个当地日历日，保存 UTC；补传、账号改时区与重建不延长（Q95）。
_Avoid_: 当天 Session（指业务日时）、日期（泛指时）

**Processing position（处理进度）**（ADR-0050）:
业务日中已整理完的位置：`captures` 的 `after: "new"` 从这里读起。只在接受范围变更集的同一事务中推进，并且只越过被完整覆盖的时段；擦除时退回到最早受影响的时段之前。
_Avoid_: 读取位置（内核的 `capture_read`）、回执、ack

**Deletion task（删除待办）**（ADR-0050）:
擦除或删除一天时，与删除原文在同一事务中记下的待办：要删除的工作 Session 与当天用过的全部工作 Session。由编排器幂等执行（中断、删除、等清理完成、查出受影响的 Memory），完成之前 App 只显示"删除中"。
_Avoid_: 删除任务队列、job

**Erasure tombstone（擦除墓碑）**（ADR-0050）:
被擦除的引用编号：不再复用，之后引用它们的承诺、修订与交谈端点一律拒收。
_Avoid_: 删除记录（指内核的 Tombstone 时）

**Natural expiration（自然到期）**（P4 设计，Q95、Q96）:
业务日达到固定原文期限后立即拒绝原文访问、补传与旧工作 Session 的整理写入；后台清理原文和读页状态，保留长期账本、历史绑定及不含正文的不可复用标记。不同于主动删一天，不级联删除长期账本。
_Avoid_: 擦除、删一天、Retention（那是内核 Session 的策略）

**Capture revision（原文版本）**（P4 设计，capture_revision，Q98）:
业务日原文的变化版本，追加、擦除、自然到期时改变。应用以它拒绝已过时的在途简述；整理进度与工作 Session 换绑不代替原文版本。P4 不支持原位原文修订。
_Avoid_: 处理进度、读取位置、Revision（那是承诺修订）

**Domain change（领域变化）**（P4 设计，Q99、Q115）:
Persona 接受日总览、周回顾等领域事实时，在同一 owner 事务记录的稳定事实 ID 与版本。供应用可靠发现后续工作，应用持久接收后才确认；只表达事实，不保存调用哪个 Agent 的策略。空周回顾也是接受，同命令重复交付去重，合法重交形成新版本。事实 ID 在 owner 内唯一，完整去重键含 owner、事实 ID 与版本；确认历史可清理，版本计数保留至注销（[A7 实现规则](../docs/design/xinli-captures.md#接受事实与领域变化p4-a7)）。
_Avoid_: Event（那是内核日志）、编排任务、Run 终态

**Owner deletion barrier（owner 注销屏障）**（P4，Q104）:
按 (BusinessLine, EndUser) 永久保存的注销记录。建立后拒绝该 owner 的业务读写，逐批清除 Persona 数据；完成状态只指 Persona 清理完成。重新注册使用新 EndUser ID，旧屏障仅保存键、状态与时间，不能由夹具重置。应用负责封禁登录、内核及逐库注销。
_Avoid_: 删除待办（那是擦除后的工作 Session 清理）、内核 EndUser 宽限期

**Work Session binding（工作 Session 绑定）**:
业务日、项目或周回顾与其当前内部工作 Session 的对应关系。生产绑定按期望旧值、目标世代与编排任务世代比较后更新；只有当前绑定可以执行已登记的整理命令。
_Avoid_: 内核 Session 的所有权、Persona 领域事实

**Work Session retirement（工作 Session 退役）**:
无法排除旧远端写入时，停止一个工作 Session 在 Persona 的读写资格，并把它保留为历史绑定。后续命令使用新绑定；重建继承原删除期限，历史绑定仍参与审计、擦除和注销。
_Avoid_: Run 中断、Session 物理删除

**Persona MCP service credential（Persona MCP 服务凭据）**:
可信内核调用 Persona MCP 时携带的秘密，绑定唯一业务线与到期时间。Persona 每请求校验，工具访问存储前要求平台 _meta 的业务线一致；同业务线可同时登记多把用于轮换。它授权可信平台声明 EndUser，不是用户登录态。
_Avoid_: 内核服务 JWT、应用登录态、三方 OAuth
