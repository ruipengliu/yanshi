---
status: accepted（2026-11-19 定稿）
---

# 委托在宽限期后转入后台

> **已定稿（2026-11-19），随本次提交实施。** 来源：ADR 评审第二轮（[ADR 评审](../review/2026-11-14-adr-review.md) §4.3 的保留问题）。修订 [ADR-0035](0035-background-calls-for-offline-devices.md) 第 7 条与 [ADR-0031](0031-delegation-as-routed-calls.md) 的"父 Run 等待"。

ADR-0055 之后，委托可以并行、可以交接工件，但父 Run 仍要等子 Run 结束：委托调用的截止时间取 `delegation.timeout`（默认 1 小时），期间父 Run 挂起、父模型不被调用，用户说什么都得不到回应。专员做的事（报告、深度回顾、长任务，章程 G3）多在几分钟到几十分钟，委托用得越多，对话越像"卡住"。

ADR-0035 为离线设备解决过同一个问题：宽限期后写占位结果、Run 照常继续，迟到的结果作为一条输入送达。它的第 7 条把委托排除在外，理由是"子 Run 结束后，迟到的结果回不到父 Run"——而那套机制恰好提供了这条路。代码中有两处排除：(a) 委托调用本身（目标以 `@` 开头）不转后台；(b) 委托派生的子 Session 中的设备调用不转后台（`internal/session/state.go` 的 `checkBackgrounded`）。

## 决定

1. **只让父 Session 的委托调用转后台，子 Session 内的设备调用维持不变。** 去掉排除 (a)，保留 (b)：子 Session 的委托 Run 是产出回复的那个 Run，它的设备调用若转后台，子 Run 会以"已排队"结束，迟到的结果开启的新子 Run 没有父调用在等。子 Session 等离线设备，现在就是后台委托在等，对父方只是一次更长的委托。
2. **按时间触发，默认 60 秒，按目标 AgentDef 配置。** 委托调用派发后，子 Run 超过目标 AgentDef 的 `delegation.background_after` 仍无结果时，Worker 写入带 `ToolResult.backgrounded` 的占位结果（"已交给 <agent>，结果稍后单独报告，不要重复委托"），父 Run 照常继续。评测中多数委托在 20–35 秒内完成，留在原 Run 中，模型不必分两轮回答；`0` 关闭（现行行为），负数非法。不让模型以参数决定：那是多一个模型判断，准确率要另做评测，也不随子 Run 的实际时长调整。
3. **结果作为输入送达，复用 `BackgroundResult`。** 子 Session 的委托 Run 结束时，`Delegator.Report` 看父 Session 的投影：调用仍在 Run 中等待，照旧直接写结果；已转后台、尚未了结，写一条唯一内容块是 `BackgroundResult` 的输入，并入活跃 Run 或开启新 Run（ADR-0035 第 3 条：不做准入、不在入口检查配额，Worker 调用模型前检查）。ADR-0055 第二期的交还工件走同一条路：先把副本复制进父 Session，`ArtifactCopied` 与这条输入在同一个决策中写入，文字改写为副本 ID，副本作为媒体块附上。来源推导与现行委托结果相同。
4. **截止、取消与上限。**
   - **截止不延长**：仍是 `delegation.timeout`，从派发起算；ADR-0035 的 24 小时后台时限不适用于委托。到期时中断子 Run，写 `reason = timeout` 的 `BackgroundResult`。
   - **取消**：用户的 `CancelBackground` 中断子 Run（级联），写 `BackgroundCancelled`。中断父 Run 不影响已转后台的委托（与 ADR-0035 相同）；关闭或删除父 Session 照旧级联。
   - **上限**：`delegation.max_fanout`（默认 4）改为按 **Session** 计数，包括已转后台、尚未了结的委托，防止模型跨轮堆积子任务。
5. **审批与提问照旧路由到用户。** 后台期间子 Session 中的审批与 `ask_user` 与现在相同，客户端照常展示；子 Run 的能力仍是与每个祖先白名单的交集。

## 必须验证

- 模拟测试：慢于宽限期的子 Run 走后台路径，并注入崩溃；不变量"每个后台委托至多了结一次（结果、到期、取消三者之一）""已了结的子 Session 不再回报""删除父 Session 级联到子 Session"；工件交接的不变量覆盖后台路径；覆盖统计 `BackgroundDelegations`、`BackgroundDelegationResults`。
- e2e：一个后台委托的端到端用例。
- 评测：`delegate-background-report`（专员较慢，用户在其间闲聊，之后的回复引用结果），基线随代码提交。

## Considered Options

- **由模型以参数 `background: true` 决定**：见第 2 条。
- **子 Session 内的设备调用也转后台**：见第 1 条，迟到的结果没有归属。
- **后台委托沿用 24 小时时限**：委托的截止由目标 AgentDef 声明，转后台只改变父 Run 是否等待，不改变子任务的预算。

## Consequences

- 同一件事可能分两轮完成：先是"已交给专员"，结果另起一轮报告；用户在其间说的话照常得到回应。
- `max_fanout` 的计数口径从"同一父 Run"改为"同一 Session"。
- ADR-0035 第 7 条只保留排除 (b)；ADR-0031 的"父 Run 挂起等待"改为"宽限期内挂起等待"。

## 重审条件

- 需要子 Session 中的设备调用也转后台（排除 (b)）；
- 需要模型或用户显式选择前台/后台；
- 后台委托的数量或时长在线上成为配额或成本的主要来源。
