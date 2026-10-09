# M0 核心原语：Session / Run / Event

> 状态：已实现（M0）· 契约：[`proto/yanshi/v1/event.proto`](../../proto/yanshi/v1/event.proto) · 术语：[CONTEXT.md](../../CONTEXT.md)

## 1. Event 日志

每个 Session 拥有一条独立的、只追加的 Event 日志。日志是 Session 与其中所有 Run 状态的**唯一事实源**（ADR-0004）。

- `seq` 由日志分配，从 1 开始、在 Session 内连续递增。
- 追加使用乐观并发：`Append(session, expectedSeq, events...)`，仅当日志当前末尾 `seq == expectedSeq` 时成功，否则返回 `ErrConflict`。多个事件一次追加是原子的。
- 状态由纯函数 `session.Reduce(events) → State` 投影得到。任何组件都不得持有日志之外的"权威状态"。

## 2. Run 状态机

```
            RunRequested
                 │
                 ▼
            ┌─────────┐  AttemptStarted   ┌─────────┐
            │ queued  │ ────────────────▶ │ running │ ◀─┐ AttemptStarted(n+1)
            └─────────┘                   └─────────┘ ──┘ （接管/故障恢复）
                 │                          │   │   │
   RunInterrupted│            RunCompleted  │   │   │ RunInterrupted
                 ▼                          ▼   │   ▼
          interrupted               completed   │  interrupted
                                                ▼ RunFailed
                                              failed
```

- 终态：`completed`、`failed`、`interrupted`。进入终态后该 Run 不得再有任何事件。
- M0 约束：**一个 Session 同时至多一个非终态 Run**。用户输入在有活跃 Run 时成为 `Steered`，否则成为新的 `RunRequested`（由 API 层基于当前投影判定，以乐观并发保证无竞态）。
- `Steered` 不改变状态，只把输入加入上下文，Worker 在下一次模型调用时可见。
- `RunInterrupted` 可由用户（经 API）或 Worker 追加；Interrupt 保留已产生的输出，Session 可继续接收新输入。

## 3. Worker、Attempt 与 fencing

Worker 无状态。一次 Worker 对某个 Run 的执行称为一个 **Attempt**，从 `AttemptStarted{attempt}` 开始。

- **正确性来自日志，而不是锁。** Worker 认领 Run 时追加 `AttemptStarted{attempt = 当前+1}`；此后它追加的每一批事件都携带自己的 attempt 号，并以乐观并发写入。若追加冲突，Worker 重读投影：
  - Run 已终态，或 `current_attempt` 已不是自己 → 放弃（被中断或被接管）；
  - 否则在新末尾上重试追加（冲突来自 `Steered` 等无害事件）。
- **工作队列只负责活性。** 工作队列（WorkQueue）按 Session 发放带 TTL 的租约，避免多个 Worker 同时争抢；租约过期后其他 Worker 可接管。即使租约机制出错导致两个 Worker 同时执行，日志 fencing 也保证只有一个 Attempt 的事件能被提交。
- 无进展的连续接管（在 `running` 状态下开启新 Attempt，且其间上下文没有前进）超过上限 → `RunFailed`。从挂起恢复不计入，见 [M1 设计](./m1-device-nodes.md) §2 与 [长任务设计](./m2-long-runs.md) §6。

## 4. Agent 循环（一步 = 一次 Step）

Worker 以 `Step()` 为单位推进，每个 Step 同步完成一件事，内部不启动 goroutine：

1. 认领：从 WorkQueue 取租约，读日志、投影，若有 `queued` 或需接管的 Run，追加 `AttemptStarted`；
2. 若存在未完成的 Capability 调用 → 处理一个（见下），追加 `ToolCallStarted` / `ToolResult`；
3. 否则组装上下文，调用模型，追加 `AssistantMessage`（`tool_calls` 即调用请求）；
4. 模型未请求任何 Capability → 与 `AssistantMessage` 同批追加 `RunCompleted`，释放租约。

这样的拆分使确定性模拟测试可以在任意两个 Step 之间注入故障。

### Capability 调用的幂等

一次调用经历三个事件：`AssistantMessage.tool_calls`（请求）→ `ToolCallStarted`（执行**之前**落盘）→ `ToolResult`。故障恢复时：

- 有请求、无 `ToolCallStarted` → 从未执行，可安全执行；
- 有 `ToolCallStarted`、无结果 → 可能已产生副作用：
  - Capability 声明为幂等 → 重新执行；
  - 否则 → 追加错误结果 `outcome unknown`，交由模型决定后续，**绝不重复执行有副作用的调用**。

## 5. 流式输出

模型的 token 增量**不进入日志**，只经由 Session 级的 LiveBus 实时扇出，并标注 `run_id` 与 `attempt`。日志只记录完整的 `AssistantMessage`。

客户端订阅 = 从某个 `seq` 起的已提交事件 + 实时增量。断线重连时只需带上最后见到的 `seq`，已提交事件可完整补齐；丢失的增量会被随后提交的完整消息覆盖。

## 6. 确定性模拟测试

`internal/sim` 用固定随机种子驱动多个 Worker 与模拟客户端，所有组件使用虚拟时钟与脚本化模型：

- 随机交错各 Worker 的 Step 与客户端动作（输入、插话、中断）；
- 随机注入故障：Worker 崩溃（丢弃其内存状态）、租约过期、模型报错；
- 每步后检查不变量：终态后无事件、Attempt 单调、每个调用至多一个结果、非幂等调用不重复执行、至多一个活跃 Run；
- 停止注入故障后，所有 Run 必须在有限步内进入终态。

失败时打印种子，同一种子可完全复现。

## 7. M0 不包含

Device Node 与能力路由、Run 挂起等待（均见 [M1 设计](./m1-device-nodes.md)）、持久化日志存储（ADR-0007）、鉴权、上下文压缩（见 [长任务设计](./m2-long-runs.md)）、Call。
