---
status: accepted
---

# 内核、Persona 与应用分三层，内核不认识业务实体

xinli 要从评测中的模拟业务线（原内核内存实现，现已迁至 `persona/`）走到生产，并演进成更完整的 persona agent。产品功能（全时记、个人记忆、问答、待办、定时、多渠道、账号权益）远超内核的范围；同时以后还会有其他 persona 类产品与三方 Agent。

## 决定

分三层（设计见 `docs/design/persona-architecture.md`）：

- **yanshi 内核**：Agent 怎样可靠、安全地运行。不认识交谈、承诺、项目、人等任何业务实体。
- **Persona 层**（`persona/`）：关于一个人的结构化知识（实体、时间线、交谈、承诺、项目、笔记），以变更集日志为事实源（ADR-0042 的推广），独立的 PostgreSQL，按 (BusinessLine, EndUser) 分片，数据归业务线。经 MCP 向内核提供工具，向应用层提供查询 API。
- **应用层**（`apps/xinli/`）：采集识别、账号设备、权益、渠道、推送、跨 Session 编排、BFF。

依赖方向只有一个：应用层 → Persona → 内核的公开契约。`persona/` 与 `apps/` 是独立的 Go module，模块路径不以 `yanshi/` 开头，不能 import `yanshi/internal/`（`make check` 另做依赖检查）；内核不 import 它们。评测经 Persona 的夹具契约（Seed、State 与用户操作）准备与读取数据；评分留在内核的评测运行器，夹具不提供 Check（2026-11-27 更正：原文列出的 Check 与 [Persona 架构](../design/persona-architecture.md) §3.4 及实现不符；重审条件见[解耦调研](../review/2026-11-27-eval-xinli-decoupling-study.md)）。

接入 Persona 的业务线中，内核的 Memory 只存 EndUser **本人**的陈述；关于他人与外部世界的知识只在 Persona，由 Agent 按需拉取，不自动注入。Memory 的类别不变，没有 Persona 的业务线照常使用 `relationship`。

**改内核的判据**：第二条业务线也会需要，并且业务侧做不到或做了不安全；满足时写 ADR。

（2026-11-14 修订）这条判据只约束**业务能力进入内核**，不约束内核既有职责的完善：

> 业务实体与产品规则默认留在业务层。内核既有职责的完善不以第二条业务线实际落地为前提，但必须说明跨业务适用性、现有接口不足，以及业务侧实现的正确性或安全成本。

第二条业务线接入之前，"它也会需要"只是待验证的假设：据此泛化内核时，ADR 中要写明依据，以及假设不成立时怎样撤回。业务实体不进内核、依赖方向只有一个、Persona 与内核故障域分开，这三条不变。为通用而设计、需求却全部来自 xinli 的机制（ADR-0045、ADR-0046、ADR-0047）标为"只在一条业务线上验证过"（[ADR 评审](../review/2026-11-14-adr-review.md) §2.9）。

## Considered Options

- **Persona 并进 xinli 的业务后端**：少一个服务，但第二个 persona 类产品要重写知识层。
- **把内核的 Memory 扩成知识图谱**：只有一处存储，但业务语义进入内核，与 ADR-0037、ADR-0042 的边界相反，内核的召回也会随业务实体膨胀。
- **独立仓库**：边界更硬，但领域核心与评测难以共用一份（回执是被测行为，见架构评审 2026-10-26 §3.1）。独立 Go module（模块路径不以 `yanshi/` 开头，Go 的 internal 规则才生效）加上 `make check` 的依赖检查，已能守住依赖方向。

## Consequences

- 原模拟业务线的领域逻辑已迁入 `persona/`；原进程内开关与业务调试路由已删除，评测的 Seed/State 改经外部业务线夹具 HTTP。
- `CONTEXT.md` 只保留内核术语，业务术语迁到 `persona/CONTEXT.md` 与 `apps/xinli/CONTEXT.md`。
- xinli 的 AgentDef 不再把他人的信息写成 Memory 的 `relationship`。
- Persona 与内核不共用数据库，故障域分开；Persona 不可用时整理失败、下一次补上，不丢原文。

## 重审条件

（2026-11-14 补充）任一出现即重新评估：

- 第二条业务线接入：逐条复核内核与业务层的边界，以及标为"只在一条业务线上验证过"的机制；
- 业务侧连续两次绕开内核实现同类能力（说明判据把应当进入内核的东西挡在了外面）。
