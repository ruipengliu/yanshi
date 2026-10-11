---
status: accepted
---

# 委托是路由到派生子 Session 的调用

章程 G7 要求多 Agent 协作。一个 Agent 把任务交给另一个 Agent 时，平台要保证送达、只执行一次、结果一定回来、父子生命周期一致——这与设备路由（M1）是同一个问题（[委托设计](../design/m5-multi-agent.md)）。

## 决定

1. **Agent 是一种 Capability。** 白名单条目 `agent:<glob>`，工具名 `agent__<名称>`，说明取自目标 AgentDef 的 `description`，版本按发布配置对 EndUser 分流（ADR-0020）。
2. **委托是一次路由调用，目标是派生的子 Session。** 父 Run 提交 `ToolCallStarted{node_id: "@agent/<子 Session ID>"}` 后挂起；子 Session ID 由父 Session 与调用 ID 推导，任务以 `delegate_<调用 ID>` 为输入 ID 提交，重复派发因此幂等。子 Run 继承 Run 的全部语义：租约、fencing、挂起、审批、提问、计量、删除。
3. **结果由子 Session 的 Worker 写回。** 子 Session 没有活跃 Run 时，Worker 把委托 Run 的终态作为父调用的外部结果写入，并在同一事务中唤醒父 Session（`CommitAndWake`），然后才归还子 Session；写回幂等，父调用已有结果时作废。结束子 Run 的中断与关闭同样唤醒子 Session。
4. **委托不扩大权限。** 子 Session 记录祖先 AgentDef（`SessionCreated.parent.ancestors`）；子 Run 的工具是自身白名单与每个祖先白名单的交集。子 Session 属于同一业务线、同一 EndUser，审批与提问照常路由到用户本人。
5. **超时、中断与删除沿用已有路径。** 父调用的时限取目标 AgentDef 的 `delegation.timeout`；超时与父 Run 的中断经撤回路由调用的同一处（`Canceler`）中断子 Run；Janitor 删除父 Session 时，从父日志中的委托目标找到子 Session 一并删除；子 Session 已删除时重新派发向父调用报告，不会复活它。

## Considered Options

- **在一个 Run 内开子线程**：子任务的崩溃恢复、审批与计量都要另做一套，模拟测试无从覆盖。
- **父 Worker 轮询子 Session 的结果**：父 Run 要么占着 Worker 等，要么定期唤醒查询；由子侧写回只在终态时发生一次，父 Run 挂起期间不占任何资源。
- **专用的编排服务**：新增组件与契约，而路由调用的机制已经提供了送达、去重与超时。

## Consequences

- `@agent/` 是保留的 Node ID 前缀，`agent` 是保留的设备标签。
- 路由调用目前按顺序执行：同一个父 Run 同时未完成的委托至多一个，`delegation.max_fanout` 为将来的并行派发预留。
- 工件只对其所属 Session 可见（m2-artifacts），父子之间只传文字：任务的 `context` 与子 Run 最后一条回复。
- 删除父 Session 级联删除子 Session；关闭父 Session 只经中断结束进行中的子 Run，子 Session 本身保留。
- （2026-10-11 补充，[评审](../review/2026-10-11-scalability-review.md) §11）子 Session 的任务由父 Run 的模型写成，Memory 写入闸门把子 Session 一律视为受外部内容影响，任务不算用户原话。委托 Run 记在子 Session 投影的固定位置（`State.Delegated`），不依赖输入去重窗口。撤回委托是尽力而为的，子 Worker 每次调用模型之前确认父调用仍在等待，作为兜底。
