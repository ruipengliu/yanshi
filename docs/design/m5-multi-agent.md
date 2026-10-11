# M5 多 Agent 委托（草案）

> 状态：已实现（2026-10-11，ADR-0031），与草案的差别见 §9 · 来源：[延展性评审](../review/2026-10-11-scalability-review.md)讨论、[调研：Agent harness](../research/2026-10-agent-harness-and-system1.md) §1 · 依赖：[M1 设备即节点](./m1-device-nodes.md)、[M2 Session 生命周期](./m2-session-lifecycle.md)、[M4 AgentDef 灰度](./m4-agent-rollout.md)、注册中心（M5，未做）· 术语以 [CONTEXT.md](../../CONTEXT.md) 为准


## 1. 要解决的问题

章程 G7 要求跨节点多 Agent 协作，M5 列有"跨节点多 Agent 编排"。现状：一个 Session 对应一个 AgentDef，模型没有任何调用另一个 Agent 的工具。唯一的 Agent 到 Agent 路径是 Call 的 `run_task`（语音模型把任务交给同一 AgentDef 的后台 Run），是固定的一对一派生。

"准确调用"要同时满足两件事：模型能选对要委托的 Agent；选定之后平台保证一定送达、只执行一次、结果一定回来、父子生命周期一致。后者与多端路由（M1）是同一个问题，所以本设计把 Agent 当作一种 Capability，复用全部路由机制，不另造一套编排。

## 2. 原则

- **Agent 即 Capability。** 委托在模型眼里是一次工具调用，在平台眼里是一次路由调用。
- **委托即派生 Run。** 子 Agent 以子 Session 中的 Run 运行，天然继承租约、fencing、挂起、中断、删除与计量语义（调研 §1 的结论：以"Run 派生 Run"为原语，不在一个 Run 内开线程）。
- **不跨 BusinessLine，不跨 EndUser。** 子 Session 属于同一业务线、同一 EndUser（ADR-0002）。跨业务线的协作留给 A2A，不在本设计内。
- **安全默认不变。** 子 Run 的 Approval 与 `ask_user` 照常路由到用户本人；子 Agent 的权限是自身 AgentDef 的白名单与父 Run 白名单的交集。

## 3. 面向模型的形态

- AgentDef 白名单新增条目 `agent:<名称>`（可用 glob，如 `agent:research-*`）。
- 工具名 `agent__<名称>`，描述取自目标 AgentDef 的 `description`（新增字段，一两句话说明它擅长什么、不做什么）。版本由发布配置按 EndUser 分流（ADR-0020），父 Run 不指定版本。
- 输入契约固定：`task`（必填，文字说明）、`context`（可选，父 Run 中的 `artifact://` 引用与文本片段）。输出是子 Run 最后一条 `AssistantMessage` 的内容加子 Run 产生的工件引用。
- 工具列表中 Agent 工具与设备工具、沙箱工具一起按名称排序（请求前缀稳定）。

## 4. 平台侧的生命周期

```
父 Run: AssistantMessage.tool_calls{agent__x}
  └─ ToolCallStarted{node_id: "@agent/<子 Session ID>", deadline}     父 Run 挂起
       子 Session: SessionCreated{agent: x, parent: {session, run, call_id}}
       子 Session: RunRequested{input: task + context}
       ...（子 Run 正常执行：审批、设备、沙箱、再委托）
       子 Session: RunCompleted | RunFailed | RunInterrupted
  └─ ToolResult(attempt = 0)                                            外部结果，唤醒父 Run
```

- **派发**：Worker 先提交 `ToolCallStarted`，再创建子 Session 并提交 `RunRequested`，二者以 `call_id` 幂等（子 Session ID 由 `call_id` 推导，重复派发只会命中已存在的 Session）。创建子 Session 失败时下一步重试，与 Inbox 写入失败同样处理。
- **结果回写**：子 Run 进入终态时，由子 Run 所在的 Worker 以外部结果（`attempt = 0`）向父 Session 追加 `ToolResult`，然后 `Enqueue` 父 Session。回写失败则子 Run 保持"终态但未回写"，Janitor 与 Worker 的下一步都可重试：回写以 `call_id` 幂等，父投影只接受针对已派发、未完成调用的结果（与设备结果相同）。
- **超时**：`deadline` 取目标 AgentDef 声明的 `delegation.timeout`（默认 1 小时）。到期父 Run 追加超时错误结果，并中断子 Run。
- **中断**：父 Run 被中断时，Service 中断其所有未完成的子 Run（级联，尽力而为，与撤回 Inbox 条目同一处）。子 Run 被用户单独中断时，父 Run 收到错误结果。
- **插话**：父 Session 的 Steer 不自动转发给子 Run；模型若要改变子任务，应中断后重新委托。保持语义简单，后续按评测需要再放开。
- **深度与扇出**：`SessionCreated.parent` 记录深度（顶层 Session 为第 1 层，子 Session 的深度是祖先数加一）；委托出去的子 Session 的深度超过委托方 AgentDef 的 `delegation.max_depth`（默认 2，即顶层可以委托、子 Session 不再委托）或同一父 Run 同时未完成的委托超过 `delegation.max_fanout`（默认 4）时，工具调用返回错误结果而不派发。
- **配额与计量**：子 Run 的用量按同一 EndUser、同一业务线计量（ADR-0019），`agent` 标签为子 AgentDef；父子共用 EndUser 的日配额。
- **删除**：子 Session 是独立的 Session，索引中带 `parent_session`；删除父 Session 时 Janitor 级联删除所有子 Session（ADR-0015，写入入口照旧"先写、后查"）。按 EndUser 删除本来就覆盖全部。
- **可见性**：子 Session 对 EndUser 可见、可订阅，客户端按 `parent` 字段折叠展示；子 Run 的 Approval 与提醒照常推送。

## 5. 与现有机制的对应

| 机制 | 设备路由（M1） | Agent 委托（本设计） |
|---|---|---|
| 目录 | Node 目录 `(BusinessLine, EndUser)` | 注册中心的 AgentDef 列表 + 发布配置 |
| 工具名 | `<标签>__<能力>` | `agent__<名称>` |
| 派发落盘 | `ToolCallStarted{node_id}` + Inbox | `ToolCallStarted{node_id: "@agent/…"}` + 子 Session |
| 幂等键 | `call_id`（SDK 账本） | `call_id`（子 Session ID 推导） |
| 结果 | 网关追加 `ToolResult(attempt=0)` | 子 Worker 追加 `ToolResult(attempt=0)` |
| 超时与中断 | deadline、Cancel | deadline、级联中断 |

`@agent/` 是新的保留 Node ID 前缀（`node.Reserved`），设备不能以它接入。

## 6. 模拟测试新增不变量

- 无环：子 Session 的祖先链中不出现自身的 AgentDef 与 Session（AgentDef 可以重复出现，Session 不能）。
- 子 Run 终态后，父调用在有限步内有且只有一个结果。
- 父 Run 终态（含中断）后，其所有子 Run 在有限步内终态。
- 父 Session 删除后，子 Session 在有限步内删除；删除不变量对子 Session 同样成立。
- 深度与扇出上限在任何交错下不被突破。
- 故障注入：创建子 Session 失败、回写失败、子 Worker 崩溃于回写前后。

## 7. 评测用例（新增，按 ADR-0018 固定）

- `delegate-when-specialist-exists`：有擅长的子 Agent 时委托，而不是自己硬做。
- `delegate-not-for-trivial`：简单问题不委托。
- `delegate-pick-right-agent`：两个描述相近的 Agent 中选对的那个；选错最多 1 分。
- `delegate-approval-routes-to-user`：子 Run 的高风险调用在用户设备上审批，父 Run 等待。
- `delegate-interrupt-cascades`：用户中断父 Run，子 Run 停止，无后续副作用。
- `delegate-result-cited`：父 Run 转述子 Run 结果时引用其工件，不编造。

"何时委托、委托给谁"的判断起步用 LLM；小模型分类器在这一点上没有证据（[判断设计](./m6-decisions.md) §3），积累评测数据后再考虑。

## 8. 未决

- 注册中心的形态（本地目录还是服务）与 `description` 字段的评测方式。
- 子 Session 的 Memory 写入归属：按 EndUser 归属不变，但写入闸门（ADR-0023）是否把父 Run 的上下文视为外部内容。
- A2A 协议对接：跨业务线、跨部署的委托，本设计不覆盖。

## 9. 实现（2026-10-11）

代码在 `internal/delegation`（派发、撤回、写回、可委托 Agent 的目录），接入 `runtime`（工具解析、深度与扇出检查、写回）、`service`（指定 ID 与来处创建子 Session、`SubmitDelegated`）、`janitor`（级联删除）与 HTTP API（等待视图 `kind: "agent"`、Session 的 `parent`）。与 §3～§7 的差别：

- **输出只有文字。** 工件只对其所属 Session 可见（m2-artifacts），子 Run 产生的工件引用父 Run 打不开；`context` 同样只传文字。父子共享工件须先定义授权，留待需要时做。
- **扇出实际至多为 1。** 路由调用按顺序推进：父 Run 等一个调用的结果之后才开始下一个。`max_fanout` 照常检查，为将来的并行派发预留。
- **委托不经 Submit 准入。** 子 Run 是进行中的父 Run 的一部分；配额照常检查，用尽时父调用得到错误结果。
- **任务被拒绝时直接告诉父调用。** 子 Session 已删除或关闭、配额用尽、任务被内容安全拒绝时，派发写回错误结果而不是等到超时；内容安全或存储不可用时父 Worker 下一步重试。
- **级联只沿删除与中断。** 关闭父 Session 中断进行中的子 Run，子 Session 本身保留。
- **模拟测试**：sim Agent 可以委托给自己（长 Run 模式下不委托），子 Session 由父日志中的委托目标发现，审批、中断、收敛、删除检查都覆盖它们。不变量：来处与父日志一致、深度不超过上限、父 Session 删除完成后子 Session 也删除完成；写回丢失会表现为父 Run 不能收敛（把写回去掉，模拟测试即报活性失败）。覆盖统计 `Delegations`、`DelegationResults`。
- **评测**：§7 中除 `delegate-interrupt-cascades` 之外的五个用例在 `evals/delegate-*.yaml`，用例可以定义只给自己用的被委托 Agent（`setup.agents`）；中断的级联不依赖模型，由 e2e 测试 `TestDelegationCascades` 覆盖。
- **评审修复**（[评审](../review/2026-10-11-scalability-review.md) §11，2026-10-11）：
  - 子 Session 一律视为受外部内容影响，委托的任务不计入用户原话（Memory 写入闸门，ADR-0023）；
  - 派发在创建子 Session 之后对子 Session 本身先写后查：它恰在此前被删除并清理完毕时，撤回刚重建的索引与日志并告诉父调用（模拟测试在检查与创建之间注入删除，覆盖统计 `ChildDeletionRaces`）；
  - 委托 Run 记在投影的固定位置（`State.Delegated`，投影版本 10），写回、撤回与重复派发都用它，不受去重窗口淘汰；
  - 深度按"委托出去的子 Session 的深度"判定，与 `delegation.max_depth` 的定义一致；
  - 中断已终态的 Run 时仍撤回未完成的派发，重试中断或关闭即可补上失败的撤回；子 Worker 每次调用模型之前确认父调用仍在等待，不在等了就以 `parent_done` 中断委托 Run（`Delegator.Reconcile`，e2e `TestDelegationOrphanStops`；模拟测试注入撤回失败，覆盖统计 `CascadeFaults`）；
  - 祖先 AgentDef 的版本取不到时是配置错误（Run 失败），不再悄悄失去全部工具。
