---
status: accepted（2026-11-14 拆自内核 ADR-0037 的业务部分，按现行实现改写）
---

# xinli ADR-0001：按业务日分工作 Session，摘要归集到长期的 Project

> 拆自内核 [ADR-0037](../../../../docs/adr/0037-tiered-retention-by-daily-capture-sessions.md) 的业务部分（[ADR 评审](../../../../docs/review/2026-11-14-adr-review.md) §2.2、§6.1）。0037 写于原文还在内核的时候；本文按 ADR-0050 之后的现行实现改写，内核机制只引用 [ADR-0052](../../../../docs/adr/0052-attested-results-and-provenance.md)，不在这里重复定义。流程细节见 [旁听业务线 xinli](../../../../docs/design/xinli-captures.md) §1–§2。

他人的原话只应短期保留，而项目的来龙去脉要长期可查。内核只按整个 Session 删除（ADR-0015），日志中又会留下 Run 读过的原文页副本（ADR-0052 第 2 条），所以 xinli 按保留期把工作分进不同的 Session，而不是在一个 Session 内删除部分内容。

## 决定

1. **按业务日分工作 Session。** 原文按 EndUser 的当地日期存进 Persona 的业务日；每个业务日一个 `xinli-day` 工作 Session，以 `bind_day` 与业务日绑定，整理前必须完成绑定。工作 Session 由编排器独占写入。跨午夜的谈话分在两个业务日；已绑定的 Session 不按后续片段的日期重新拆分。
2. **原文期限由业务线担保，内核副本用同一期限。** 首次接收时固定业务日的时区，期限为次日当地 00:00 再加七个当地日历日，保存为 UTC。工作 Session 以同一个 `delete_at` 创建（ADR-0052 第 7 条），补传、改时区、重建都不延长。`xinli-day` 与 `xinli-week` 另按 AgentDef 覆盖 Retention（`close_after_idle: 36h`、`delete_after_close: 168h`），作为兜底。
3. **摘要归集到长期的 Project。** 当天整理之后，编排器为涉及的项目创建或复用长期保留的 `xinli-project` Session，提交"整理 <日期>"，输入中不含摘要。Project 的 Run 经业务线的 MCP 工具（`day_summary` 等）取回摘要。
   - **不由服务端把摘要推入输入。** 服务令牌提交的输入在投影中是用户原话（内核的来源推导，ADR-0052 第 4 条），推入会把含有他人发言的摘要洗成可信内容；作为 MCP 的调用结果，它是外部内容，注入判断与 Memory 写入闸门照常适用，不需要新增标记。
4. **关于 EndUser 本人的稳定结论写入 Memory。** 偏好、沟通习惯等由 `xinli-day` 写入 Memory；到期删除工作 Session 时保留这些 Memory（ADR-0052 第 7 条）。项目现状是业务线的数据，不写入 Memory。
5. **跨 Session 的编排都在业务线。** 创建与绑定工作 Session、提交整理、归集、退役与重建、删除待办，都由 xinli 的编排器完成；内核不提供跨 Session 写入的原语，只提供 Session 内的定时（ADR-0045）。

## Considered Options

- **按说话人删除 Session 中的部分 Event**：保留期可以精确到每段话，但要新增一条删除路径，改写内核 ADR-0004、ADR-0015 与模拟测试的删除不变量。
- **拆成两条业务线**：Retention 不必改，但 Memory 归属业务线（ADR-0003），用户要通过 Grant 同意"自己读自己"。
- **只靠业务线服务端按时删除工作 Session**：内核不必改，但内核日志中原文副本的保留期就取决于服务端没有缺陷；因此用内核的 `delete_at` 兜底（ADR-0052 第 7 条）。
- **给输入新增"派生自不可信内容"的标记**：推入也安全，但要改内核的契约、投影与闸门；MCP 调用结果已有同样的信任边界。

## Consequences

- 佩戴者本人的原话也只保留到业务日期限：超过期限的问题只能依据摘要回答，产品须写明原话的保留期。
- 删除一天不级联到其他 Session：已经归集到 Project 的摘要、周回顾与项目现状不随之删除（[Persona 架构](../../../../docs/design/persona-architecture.md) §6 的擦除流程）。
- 整理与归集的 Run 计入 EndUser 与业务线的配额；超额时 Run 由 Worker 挂起，业务线次日重试，不为此放宽配额。工作 Session 删除前仍未整理的，提醒用户"这几天的内容没能整理"。
- 擦除、删除一天与注销的流程见 [旁听业务线 xinli](../../../../docs/design/xinli-captures.md) §13。

## 重审条件

任一出现即重新评估：

- 产品需要按说话人或按段的保留期（而不是按业务日）；
- 内核支持在一个 Session 内部分删除（ADR-0004、ADR-0015 被重审）；
- 第二条业务线复用这套按日分 Session 的做法：考虑把它沉到 Persona 层。
