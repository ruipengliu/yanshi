---
status: accepted
---

# 以每日旁听 Session 实现分级保留

他人的原话只应短期保留，而项目的来龙去脉要长期可查。Event 只追加（ADR-0004），删除只按整个 Session 进行（ADR-0015），因此不在一个 Session 内删除部分 Event，而是按保留期把数据分进不同的 Session：每个 EndUser 在当地每天一个旁听 Session，当天的 Capture 都写在这里，原文随它删除；每晚整理成摘要后，按主题归集到长期存在的 Project（每个项目一个 Session）；关于 EndUser 本人的稳定结论（偏好、沟通习惯）写入 Memory。

为此 Retention 可以按 AgentDef 名称覆盖：同一业务线中，旁听 Session 整理后关闭、关闭 7 天后删除，Project 长期保留。未覆盖的 AgentDef 沿用业务线的 Retention。

摘要由 Project 的 Run 经业务线的远程 MCP Server 拉取（如 `mcp_xinli__day_summary`，ADR-0017），而不是由服务端作为输入推入：服务令牌提交的输入在投影中就是用户原话，推入会把含有他人发言的摘要洗成可信内容；作为 MCP 的调用结果，它天然是外部内容，注入判断与 Memory 写入闸门照常生效，不需要新增标记。第一版的跨 Session 编排（创建 Project、分发、定时提醒）都放在业务线服务端，yanshi 不新增跨 Session 写入的原语。

## Considered Options

- **允许按说话人删除 Session 中的部分 Event**：保留期可以精确到每段话，但要新增一条删除路径，改写 ADR-0004、ADR-0015 与模拟测试的删除不变量。
- **拆成两条业务线**：Retention 不必改，但 Memory 归属业务线（ADR-0003），用户要通过 Grant 同意"自己读自己"。
- **由业务线服务端按时删除旁听 Session**：yanshi 不必改，但保留期的合规取决于服务端没有缺陷，平台无从保证。
- **给输入新增"派生自不可信内容"的标记**：推入也安全，但要改契约、投影与闸门；MCP 调用结果已有同样的信任边界。
- **业务线服务端作为 Node 接入**：Node 总是代表某个 EndUser，服务令牌不能接入（[认证](../design/auth.md) §3）；业务线的服务本来就以远程 MCP Server 提供，调用带有 EndUser 与 Session 的身份。

## Consequences

- 佩戴者本人的原话同样只保留 7 天：超过 7 天的问题只能依据摘要回答，产品须写明"原话保留 7 天"。
- 删除不级联到其他 Session：删除某天的原文不影响已经归集到 Project 的摘要；产品提供删除某天、删除某个项目、删除全部旁听数据三种粒度。
- 按 Retention 到期删除 Session 时，保留由它写入的 Memory，否则每晚整理写下的偏好与沟通习惯只能存活 7 天。EndUser 主动删除（删除某天、删除对话、注销账号）时照旧一并删除。这修改了 [Memory 设计](../design/m4-memory-grant.md) §9 第 3 条：它的本意是尊重用户的删除意图，到期清理不是用户的意图。
- 跨午夜的对话分在两天的旁听 Session 中；项目归集本来就跨天。
- 整理与 Project 更新的 Run 计入 EndUser 与业务线的 Quota；超额时由业务线服务端次日重试，原文删除前仍未整理的提醒用户，不为此放宽 Quota。
