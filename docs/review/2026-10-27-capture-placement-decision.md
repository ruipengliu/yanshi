# 裁决记录：旁听原文放在内核还是业务线

> 状态：已裁决（2026-10-27），结论见 ADR-0050 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准 · 承接：[Capture 与流该不该在内核](./2026-10-27-capture-boundary-review.md)（提出方案 A、B、C 与 §5 的三步验证）、[xinli 与 persona agent 的整体架构](../design/persona-architecture.md) §13 Q28–Q29
>
> 方式：P0（xinlimock 迁成 `persona/` 与 `apps/xinli`）完成之后，Claude 先请 Codex（gpt-6-astra，xhigh）独立评估，再请它逐条反驳 Claude 的结论，最后由 Claude 核实其事实陈述后取舍。下文的【事实】可在所引位置核对，【推断】由事实推出。

## 1. 边界评审 §5 的三步验证怎样完成

| 步骤 | 原计划 | 实际 |
|---|---|---|
| 1. 可信结构化片段进入来源推导 | 先做验证再裁决 | 改为 ADR-0050 迁移的第一步，带 `memgate`、`provenance` 与 `internal/sim` 的测试；在它通过之前不切换任何 AgentDef |
| 2. 业务侧输出与 `captures` 相同的文字，跑 `capture-*` | 先做验证再裁决 | 由设计保证：业务线只返回结构化的页，内核用现有的渲染器生成文字（§3）。剩下的模型可见差异只有工具名，在切换 xinli-day 时以 `make eval` 的 `capture-*` 把关 |
| 3. 删除语义对照 | 一页对照 | §2 |

第 2 步原本要先把 `internal/capture` 的分页迁进业务侧的 mock 才能跑，等于先实现一半方案 C；改由平台渲染之后，"模型看到的文字是否逐字相同"不再是要靠评测发现的风险，评测只需要在切换时确认工具名的变化没有影响。

## 2. 四种删除操作

L＝内核日志（含工具结果中的原文副本），K＝内核的 Capture 正文存储（只在 A 中有），B＝业务库中的原文（只在 C 中有），P＝Persona 的派生对象，M＝Memory。

| 操作 | A（现行 M7） | C（ADR-0050） |
|---|---|---|
| 删一段 | K 中该范围清空；L 中模型回复、`file_changes` 参数、压缩摘要里的派生副本留到当天 Session 删除；出站流 `capture_erased` → 编排器 → Persona 墓碑与级联（至少一次投递、72 小时对账）；M 列出、用户决定 | B 删除、写墓碑、级联 P，同一本地事务并记下删除待办；编排器中断并删除当天工作 Session（Erasure 原因），L 中的原文与派生副本一并删除；M 保留、列出、用户决定；重建的工作 Session 继承原来的删除期限 |
| 删一天 | 删除当天 Session：L、K 删除；P 中已归集的摘要不级联（ADR-0037），产品另给"删除项目"；M 随用户主动删除一并删除 | B 删除当天原文；删除当天工作 Session（用户主动删除，M 一并删除）；P 同 A |
| 注销 | EndUser 屏障；L、K、M 全删；P 由应用层调用 Persona 删除 | EndUser 屏障；L、M 全删；B、P 由应用层删除（同一个库） |
| 到期 | Retention 删除当天 Session（L、K），保留 M；P 保留 | 业务线删除 B（业务线担保）；Retention 删除工作 Session（L），保留 M；P 保留 |
| 担保 | 平台担保 L、K、M；业务线担保 P | 平台担保 L、M；业务线担保 B、P |

两者共同的残留：删除先不可见、后清理（ADR-0015）；从发起删除到 Session 删除之间，已经读到原文的 Run 仍可能写 Memory 或发出调用，所以删除完成前产品只承诺"删除中"。

## 3. 讨论中采纳与未采纳的意见

Codex 第一轮独立结论是 C，同时指出 C 会把原文页的副本带回内核日志（Q15 想避免的表膨胀）、审核不能信业务线自报。第二轮对 Claude 的结论逐条反驳，采纳如下：

| 意见 | 核实 | 处理 |
|---|---|---|
| 业务线返回的页头在来源推导中算外部内容，纯佩戴者的页也要过依据比对，比内置 `captures` 更严；外部文字与他人的话共用一个 `call_id`，注入判断会重复写判断或跨批漏判 | 【事实】`internal/runtime/memgate.go` 的依据比对与 `internal/session/provenance.go` 的 `resultPieces`：MCP 结果整体算 FromExternal | **改设计**：业务线只返回结构化的页，由平台渲染，结果中没有业务线写的文字 |
| attested 标记须由平台写入、随日志保存，Apply 再校验 | 与 ADR-0039 一致 | 采纳 |
| 审核要覆盖错误回显；组合违规不能因各段合格而放行 | 【事实】`internal/service/captures.go` 的整批送检、违规再逐段复核 | 采纳：整页送检在前 |
| 列出受影响的 Memory 时，`SourceSession` 只证明写入者、不证明依据 | 【事实】`internal/memory/memory.go` 的 `SourceSession` | 采纳：措辞改为"依据可能落在被删范围内"，判定沿用写入闸门的依据 |
| `scope: run` 只裁剪模型上下文，写入闸门看整个 Session；纠错后旧文字仍可作依据 | 【事实】`internal/runtime/transcript.go` 只按 Run 组装上下文，`State.Pieces` 覆盖整个 History | 采纳，写进 ADR-0050 的后果 |
| "关闭后 7 天删除"不能保证重建后不续期 | 【推断】擦除后重建的 Session 有新的关闭时间 | **新增内核原语**：创建 Session 时的绝对删除期限 |
| 只靠 BFF 重试删除，崩溃会丢任务 | 【推断】 | 采纳：业务库事务中记下删除待办、停用旧 Session，编排器幂等执行 |
| 分页依赖时钟、间隔与结果预算，MCP 没有传这些 | 【事实】`internal/capture/capture.go` 的分页与预算 | 采纳：平台参数经 `_meta` 交给业务线；分页迁入 Persona 时以固定输入逐页差分 |
| 处理进度在同一事务里推进，不能证明没有漏页 | 【推断】 | 采纳：只沿连续、完整交付的范围推进 |
| 退役条件写进 ADR-0050 即可，不必另立 ADR | — | 采纳 |
| 修订清单漏了 ADR-0039、0015、0021 | — | 采纳 |

未采纳：无。第一轮建议的"审核缓存绑定租户、完整文本组合与策略版本"是对的，但第一版不做缓存（ADR-0050 第 2 条），留到测出成本之后。

## 4. 已过时的陈述

- 【事实】[边界评审](./2026-10-27-capture-boundary-review.md) §3.3、§3.4 说 C 的擦除是"业务库本地事务""覆盖日志中的派生副本"：前者只对业务库与 Persona 成立，内核 Session 的删除仍是跨库的一步（§2）；后者要以删除工作 Session 为前提，并且重建的 Session 要继承删除期限。
- 【事实】[M7](../design/m7-kernel-primitives.md) 文件头"未实现"已过时：交付确认（`internal/session/ack.go`）与 EndUser 删除屏障已经实现；§2–§4、§11 由 ADR-0050 取代。
