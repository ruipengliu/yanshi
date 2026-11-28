---
status: accepted（2026-11-22 定稿）
---

# MCP 写工具按来源条件审批（`taint_gated`）

> **已定稿（2026-11-22），随本次提交实施。** 来源：[架构评估评审](../review/2026-11-22-architecture-evaluation-review.md) §5 A7、§3 第 19 条。补充 ADR-0017（未声明只读的 MCP 工具默认需要审批）与 ADR-0023（写入闸门只收紧不放宽）；修订 [MCP 适配](../design/m5-mcp.md) §2、§3。

远程 MCP 工具未声明只读时默认需要审批（ADR-0017），业务线可用 `tools.no_approval` 逐个放宽。放宽是全有或全无：一个工单、日程或笔记类写工具，要么每次都打扰用户，要么在上下文含外部内容（网页、他人的旁听、其他工具的结果）时也直接执行，后者正是提示注入借写工具外溢的路径。内核已有的来源判断（`runtime.tainted`：上下文中有不是用户原话的内容，或经过压缩、或是委托的子 Session）只保护 `memory_save` 与实现了 `capability.ApprovalSummarizer` 的本地能力（目前是 `schedule`），MCP 工具用不上。

## 决定

1. **登记增加 `tools.taint_gated`**：名称 glob 列表，语法与 `allow`、`no_approval` 相同。匹配且已被 `no_approval` 放宽的写工具，在上下文含非用户原话时请求审批，在干净的上下文中照常免审批。
2. **只收紧不放宽。** `taint_gated` 不单独放宽任何工具：未列入 `no_approval`、或明确声明 `destructiveHint` 的工具照旧总是审批；只读工具不受影响。
3. **复用现有判断，不新增机制。** 远程 MCP 工具实现 `capability.ApprovalSummarizer`：受约束的工具返回非空的动作摘要，其余返回空（Worker 把空摘要视为不审批）。判定时机、已请求审批的调用一律走完、调用开始后不再回头请求审批，都与 `schedule` 相同（`internal/runtime/worker_calls.go` 的审批段）。
4. **工具结果算外部内容。** 来源推导把 MCP 与设备工具的结果计为外部内容（`session.State.Pieces`），所以同一 Session 里先调过这类工具，之后的 `taint_gated` 调用都要审批。业务线若需要"先查后写"也免审批，应把查询做成不经工具结果的形式，或接受审批；内核不按工具区分结果的可信度。

## Considered Options

- **在 `capability.Spec` 上加字段，由 Worker 判断**：效果相同，但多一条与 `ApprovalSummarizer` 平行的路径，`schedule` 与 MCP 的闸门可能分别演化。
- **按 Server 一个开关（`taint_gated: true`）**：同一 Server 常有只读、低风险与高风险的写工具混在一起，按工具列出与 `no_approval` 的粒度一致。
- **把只读 MCP 工具的结果视为可信**：只读说明它不改外部状态，不说明它返回的内容可信（CRM 的客户备注、网页摘录都可能含注入），与来源推导"三方给的一切是外部内容"相反。

## Consequences

- 业务线多一个可选项；不写时行为不变。
- 列入 `taint_gated` 的工具在多数多步流程（先查询再写入）中仍会审批，免审批只发生在用户直接下指令的干净上下文中。
- 评测增加两个用例（`mcp-crm-ticket-direct`、`mcp-crm-ticket-tainted`，名称以实现为准），覆盖干净上下文免审批与查询后须审批。

## 重审条件

- 业务线反馈"先查后写"的审批过多，且能给出把特定工具结果视为可信的判据；
- 注入判断（ADR-0030）退出影子模式、可以按内容而不只是来源决定审批。
