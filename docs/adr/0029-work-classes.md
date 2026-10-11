---
status: accepted
---

# 工作队列按类别认领：业务线与优先级

工作队列原来是全局先进先出：`Claim` 没有"认领谁的工作"的参数。多条业务线接入后，无法给某条业务线专用 Worker；小时级的长 Run 与需要低首字延迟的问答（章程 G3、G6）也只能排在同一条队伍里。等所有队列实现都写定再加这个参数，就要同时改动每个实现与模拟测试。所以现在把类别放进接口（[延展性评审](../review/2026-10-11-scalability-review.md) §3.2）。

## 决定

- **入队带类别。** 类别是 `workqueue.Class{BusinessLine, Priority}`，`Enqueue(ctx, sessionID, class)` 设置它；已在队列中的项改用新类别，因为最近一次入队最了解当前情况。持有者还可以在 `Park`、`Release` 之前改写 `lease.Class`，归还时一并更新类别。
- **两级优先级，由 Run 的进度决定**（`session.State.WorkClass`）：
  - `Interactive`：没有活跃 Run，或活跃 Run 调用模型的次数少于 `LongRunTurns`（6）。新输入、问答、刚开始的任务都在这一级。
  - `Background`：活跃 Run 已调用模型达到 6 次，即长任务。
  - 不按经过的时间判定。否则在积压时，排队超过阈值的新 Run 会被一律降级，交互 Worker 反而空闲。按次数判定只看已经做了多少工作，并且是确定的，可在模拟测试中重放。
- **认领按池。** `Claim(ctx, holder, ttl, pool)`：`Pool` 可限定业务线与最低优先级，零值认领任意工作。同一队列中先认领优先级高的，同一优先级内按入队顺序。
- **交互 Worker 移交长任务。** `serve -interactive-workers` 指定一部分 Worker 只认领 `Interactive`（默认为 Worker 数的四分之一），使长任务占满其余 Worker 时，问答仍有 Worker 可用。交互 Worker 持有的 Run 成为长任务后，它在两步之间挂起这个 Run（`RunSuspended{reason: "handoff"}`），并以 `Background` 类别立即归还，由其他 Worker 恢复。恢复不算接管，不计入无进展接管的上限。
- **优雅停机用同一个移交。** 进程收到 SIGTERM 后不再认领新工作。持有 Run 的 Worker 先完成当前这一步，最多等 `-drain-timeout`，然后以 `handoff` 挂起并归还租约，其他进程立即接手，不必等租约过期，也不必重做整次模型调用。

## Considered Options

- **按经过的时间降级**：与"超过 2 分钟的 Run 结束时发提醒"的口径一致。但积压时新 Run 会被成批降级，而且从挂起恢复时（如用户刚做出审批）同样被视为长任务。
- **由 AgentDef 声明优先级**：显式，但没有配置的 AgentDef 得不到保护；同一个 Agent 既回答简单问题也做长任务，声明的优先级无法区分。
- **业务线之间的加权公平认领**：要在认领时统计各业务线的在租数量，在 PostgreSQL 实现中开销大。目前用池（专用 Worker）做隔离，公平份额留待分片层（ADR-0028）一起设计。

## Consequences

- 交互 Worker 移交长任务时会多写一对事件（`RunSuspended` 与之后的 `AttemptStarted`）。每个长任务至多移交一次：之后的唤醒以 `Background` 类别入队，只有通用 Worker 认领。
- 客户端看到短暂的挂起。`handoff` 不是在等审批、设备或配额，等待视图不显示它。
- 沙箱队列沿用 Session 的类别（Worker 派发时带上），Janitor 队列一律为 `Background`。
- Submit 准入（按队列深度或等待时长拒绝）尚未实现。准入阈值要依据队列深度与等待时长的指标来定。
