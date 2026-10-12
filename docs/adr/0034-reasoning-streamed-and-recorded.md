---
status: accepted
---

# 思考过程流式推送并随回复写入日志

火山方舟的 `doubao-seed-2-1` 等模型先思考再回答，思考可能持续数秒到数十秒。原来思考过程（`reasoning_content`）在模型接入层被丢弃：这段时间里客户端什么也看不到，事后也无从查看模型是怎么想的、为什么调用了某个工具。

## 决定

1. **流式推送**：`model.Delta` 与 `live.Delta` 新增 `Reasoning`，`LiveDelta` 新增 `reasoning` 字段，与正文分开累积。晚加入的订阅者在 snapshot 中同时补齐二者。
2. **随回复写入日志**：`AssistantMessage` 新增 `reasoning`，事后可以展开查看。写入时有两条限制：
   - 超过 32 KB 截断；
   - 与回复文本一起在写日志之前做内容安全检查（ADR-0021；以写入日志、即截断后的形式），被拦截时思考过程同样不写入。
3. **不发回给模型**：Transcript 只取回复内容与调用。思考过程不占上下文、不改变请求前缀，不影响前缀缓存。
4. **记录调用信息**：`AssistantMessage` 新增 `duration_ms`、`first_token_ms`、`finish_reason`，`Usage` 新增 `reasoning_tokens`。
   - 耗时由 Worker 按 `clock.Clock` 计时，模拟测试中结果确定；
   - 这些字段只用于观察，不参与任何决策。

## Considered Options

- **只推送、不写日志**：日志不变大，但回复提交后思考过程就消失，晚加入的设备与刷新后都看不到，失去了调试价值。
- **写入单独的事件**：与回复分开校验与删除并没有好处；作为同一条 `AssistantMessage` 的字段，提交、内容安全检查与删除都是原子的。

## Consequences

- 日志变大：思考常比回复长数倍，所以有 32 KB 的上限。
- 思考过程属于个人数据，与回复一样只存放在可被删除的地方（ADR-0015），随 Session 删除。
- 内容安全提供商要检查的文本变长；提供商按字数计费时成本随之上升。
