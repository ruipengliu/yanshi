---
status: accepted
---

> 2026-10-28 按 P3 的讨论修订（`docs/design/persona-architecture.md` §13 的 Q77–Q82）：输入只收文字（ADR-0050 后内核没有原文，删除旁听引用范围）；`embedding` 推迟到 Persona 的向量检索。

# 无会话的单次调用

实况预览的简述、标题、纠错建议、Persona 的嵌入都是一次输入、一次输出的"模型函数"。用 Run 做，每次刷新都要建 Session、写十几条事件；业务侧直连模型提供商，又绕过了计量、配额、内容安全与换模型的评测（章程 G9、原则 10）。

## 决定

（设计见 `docs/design/m7-kernel-primitives.md` §7）

- `oneshots/*.yaml` 声明提示词、模型、可选的输出 JSON Schema 与工作类别，与 AgentDef 一样版本化、经发布配置灰度；种类为 `generate` 或 `embedding`。
- `POST /v1/oneshots/{name}`：输入为文字，业务线自己按说话人格式化（修订前还可以是一段旁听的引用范围）。
- **不写日志、不存结果**，因此没有需要删除的数据；日志与指标只记 ID 与 token 数。
- 照常计量与配额、输入输出都过内容安全、与 Run 共用提供商的准入；改动须经评测。

## Considered Options

- **业务侧直连提供商**：最快，但治理全部失效，换模型要改业务代码。
- **用短命的 Session 加 Run**：语义统一，但每次刷新十几条事件与一次删除，实况预览每分钟一次，量不可接受。

## Consequences

- 单次调用没有工具、没有状态、不调用 Agent 能力：需要这些的仍用 Run。
- 评测增加单次调用的用例形态。
