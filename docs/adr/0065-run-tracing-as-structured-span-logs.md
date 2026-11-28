---
status: accepted（2026-11-27 定稿）
---

# 按 Run ID 关联的结构化追踪记录

> **已定稿（2026-11-27），第 1、2 阶段已实现（2026-11-28）。** 来源为[上线检查清单](../launch-checklist.md)和[部署设计](../design/deployment.md) §4 的可观测性要求。日志字段遵循 [ADR-0015](0015-session-deletion-by-physical-removal.md) 的内容边界。

## 决定

1. **追踪记录写入进程日志。** `serve` 默认用文本日志且关闭追踪。用 `-log-format json -trace` 启用 stderr JSON 行与 Run 主链追踪。每个 span 结束时以固定消息 `span` 写一行，包含名称、开始时间（RFC3339Nano）、耗时（毫秒）、结果、span ID 及可选的父 span ID。部署沿用自己的日志采集器，以 `run=<run_id>` 查询记录。每次认领在确认 Session 后记录排队等待以及认领和初次加载的耗时，按 Session 与同一时间段关联。
2. **Run ID 决定 trace ID。** 对字节串 `yanshi/trace/v1\x00` 拼接 Run ID 求 SHA-256，取前 16 字节。span ID 由加密随机源生成，取 8 字节。两者均以小写十六进制表示，全零时将最低位置一，以兼容 W3C Trace Context 的 ID 格式。进程使用已有的 Run ID 重建 trace ID，无需更改队列、Inbox 或 peer 契约。未知 Run 时省略 trace ID。同步环节可带 `parent_span_id`，预留 `link_trace_id` 关联另一 Run。
3. **追踪只观察执行。** `internal/tracing` 只接受固定的名称与结果枚举，以及对应字段的 ID、计数与耗时。失败使用枚举，错误原文和用户、模型、调用内容留在原有路径。span ID 与耗时使用观测侧的随机源与单调时间，保持 Event 的 ID 和时间来源。Run 总时长用投影中的 `RequestedAt` 与已提交的终态 Event 时间计算。追踪记录是进程日志的一部分，沿用其保留策略，无须登记 `app.SessionData` 或执行删除复查。
4. **追踪覆盖启用追踪的内核进程，不抽样。**
   - 第 1 阶段记录提交与原子唤醒、认领、Attempt、内置循环的模型调用、提供商准入和 Run 终态。模型调用记录 `first_token_ms`，Guard 记录实际进入下一轮准入的重试次数。
   - 第 2 阶段记录进程内和 MCP 工具的执行，以及设备、沙箱、委托和向用户提问的派发。审批与提问等待沿用已有 Event。Worker 实际重派发时也记录，挂起期间重投的并行委托按 Attempt 号关联。网关记录每次设备投递，沙箱控制器记录认领至执行完成及其中的准备、执行。设备、沙箱与委托的结果写回各有记录，后台结果用 `backgrounded` 标识。设备结果可从投影仍保留的首次派发时间计时，缺少该时间时计量写回环节。子 Run 有独立 trace，子提交与父结果写回以 `link_trace_id` 双向关联。
   - 外部循环的 Worker 步与内核模型端点调用各有记录，端点从已校验的步令牌取 ID。网关逐订阅、按 Run 和 Attempt 汇总增量，记录首个增量到达耗时与转发条数。外部循环服务与 Node SDK 在内核进程边界之外，沿用已有协议，不传递 `traceparent`。
   - 未覆盖的环节包括 Janitor（按 Session 与保留期限清理）、Oneshot（没有 Run ID）、提醒推送（独立的尽力投递）与出站流（独立游标，可跨多个 Run）。

## Considered Options

- **OpenTelemetry SDK 与 OTLP。** 接入已有 tracing 后端时可复用生态，但首发需新增依赖及采集部署，当前的自托管日志查询可覆盖主链。ID 格式为后续加 OTLP 输出保留兼容性。
- **仅按现有日志字段关联。** 可以按 ID 查看部分记录，缺少首 token、准入、Attempt 结束等环节的独立耗时与父子关系。
- **为追踪新增可按 Session 删除的存储。** 会新增写路径、保留策略与删除协调。当前记录字段沿用进程日志的内容边界，日志采集已经提供所需的查询能力。

## Consequences

- Run 主链的记录可按 trace ID 聚合，父子关系可用于定位慢的模型调用和准入。记录在结束时输出，子 span 可能早于父 span 出现在日志中。
- 记录尽力而为。硬崩溃或提交结果未知时可能缺失记录，Event 日志仍是执行状态的唯一事实源。
- `app.NewCore` 传递可选 Tracer，关闭时不创建 span。以后可增设 OTLP 输出，沿用调用点与属性约束。

## 重审条件

- 部署需要将追踪接入已运行的 OTLP 后端。
- 单进程日志量超出采集预算，需要抽样。
- 排障要求向外部循环服务或 SDK 传播追踪上下文。
- 合规要求按 Session 删除追踪记录。
