---
status: accepted（2026-11-28 定稿）
---

# 在服务端扫描日志查询输入状态

> **已定稿（2026-11-28），首版已实现。** 回应[内核架构评估评审](../review/2026-11-22-kernel-architecture-evaluation-review.md) §1.2 中的异步提交对账成本。按 [ADR-0043](0043-kernel-persona-app-layers.md) 归入内核既有执行职责的完善。首版只在 xinli 验证。

异步提交丢失响应后，调用方需要把输入接受的事实与业务完成分开。现有对账路径见 [xinli 的 Events 回放](../../apps/xinli/kernel/inputs.go)，输入去重窗口见 [Session 投影](../../internal/session/state_inputs.go)。

## 决定

1. **公开按 `input_id` 查询。** `GET /v1/sessions/{id}/inputs?input_id=…` 沿用 Events 的授权范围。命中返回接受位置、Run 状态、Attempt 和调用证据。未命中对应一次读取的日志上界。相同公开 ID 被多次接受时返回 `409 conflict`，原因是 `input_accepted_more_than_once`。定时、委托、卡片动作和 Call 派生的输入按现有约定排除，完整字段见 [HTTP 契约](../design/auth.md#9-输入状态)。
2. **复用授权时的投影与 Event 日志。** `Service.Load` 校验归属与删除记录，其 `Seq` 固定查询上界，Run 状态取该投影。服务端按页读取日志，查询归约只读控制字段，并在返回之前复查删除记录。存储仍沿用现有 Event 解码。查询无专用存储、迁移或投影版本变化。
3. **扫描预算为 4096 条 Event，每页至多 64 条。** 按满页计算为 64 次日志读取，为超过 64 条输入去重窗口且含多个 Attempt、调用的 Session 留出余量。取值是首版容量假设，待线上数据验证。授权仍沿用 `Service.Load` 的快照与日志加载，冷加载的读取在专用扫描预算之外。Session 存续期间日志只会增长，超限返回不可重试的 `422 scan_budget_exceeded`，省略 `Retry-After`。调用方改用 Events 接口分页读取并自行归约。
4. **由业务线解释调用证据。** 整 Run 的统计覆盖所有 Attempt，插话附上从接受位置往后的证据，供 xinli 保留既有判定。回答沿用提交回执的 `Steered` 语义，xinli 保守标记为 `Unsafe`。收到 `scan_budget_exceeded`、旧路由的 404 或仅允许 POST 的 405 时，xinli 回退到 Events。部署支持的最旧内核包含查询路由后删除旧路由的兼容分支。

## Considered Options

- **在投影保留接受位置和调用证据。** 历史冲突要求保留同键的多次接受位置。全量保留会扩大快照，变更快照版本还会触发升级后的冷回放。有界保留仍需为窗口外查询回放日志。
- **持久输入索引。** 按 `(session_id, input_id, accepted_seq)` 定位接受及 Run 起点，可缩小补读范围。索引要与 Event 追加和唤醒原子提交，须管理迁移、回填、分片及删除交错。
- **保留调用方分页对账。** 可以延后内核变更，调用方仍承担多次 HTTP 往返，并各自处理读取上界、接受类型与删除交错。

## Consequences

- 公开查询返回同一日志上界的事实。是否退役 Session、重做任务或确认业务完成由业务线决定。提交去重窗口与判定入口沿用[现有实现](../../internal/service/service.go)。
- 扫描成本随 Session 的日志长度增长。以线上查询端到端 p99 大于 **250 ms** 或一次专用扫描读量 p99 大于 **2048 条 Event** 为门槛，复核索引方案。读量用日志读取条数评估，普通完成查询可由 `as_of_seq` 得到。
- 线上出现 `scan_budget_exceeded` 即重审预算或索引，评估调用方回退到 Events 回放的成本。
- 第二条业务线有同类需求、调用方需要按 `accepted_seq` 查询指定接受，或需要长期提交去重时，同样重审。重审时单独评估只读索引与提交去重的一致性要求。
