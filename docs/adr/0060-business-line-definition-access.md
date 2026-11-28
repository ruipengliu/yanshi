---
status: accepted（2026-11-22 定稿）
---

# 业务线声明可用的定义

> **已定稿（2026-11-22），随本次提交实施。** 来源：[架构评估评审](../review/2026-11-22-architecture-evaluation-review.md) §5 A4；裁决：同一单元内的业务线按互不信任对待（2026-11-22）。修订 [M4 灰度发布](../design/m4-agent-rollout.md) §1 的"AgentDef 本身不分业务线"，统一 [部署](../design/deployment.md) §1 的表述，取消该节"MCP Server 名称在一个内核中唯一"的限制。
>
> 2026-11-28：[ADR-0067](0067-business-line-model.md)（方向，待定稿）拟以"目录即归属 + 规范名 `<业务线>/<名>`"逐步取代 `allow` 列表，入口检查的位置不变。ADR-0067 定稿并实现之前，本 ADR 全部有效。

单元是一组共享 EndUser 身份与 Memory 的业务线（[部署](../design/deployment.md) §1）。令牌、Session、配额、MCP 工具已按业务线隔离（`httpapi` 的 `claim`、`service.WithAccess`、`mcpcap.Server.BusinessLine`），但 AgentDef 与 Oneshot 按全局名称解析：业务线 A 可以用业务线 B 的指令、模型策略与能力声明建 Session、委托（`agent:*` 列出全库定义）或调用单次调用，Oneshot 声明名一层的并发槽跨业务线共享。MCP Server 名称全局唯一，两条业务线不能各自登记同名的 Server。

## 决定

1. **可用的定义由业务线配置声明，定义文件不变。** `businesslines/<业务线>.yaml` 增加 `agents.allow`，并在已有的 `oneshots` 下增加 `oneshots.allow`（与 `oneshots.max_concurrency` 并列），两者都是名称 glob 列表（`path.Match`），列出本业务线可以使用的 AgentDef 与 Oneshot；`["*"]` 表示全部。定义文件仍是一个文件一个 (name, version)，名称在一个内核中唯一，不写业务线：业务线名是部署配置，同一份定义在不同部署中可能属于不同的业务线（开发中的 `demo`、评测中的 `eval`）。共享即是在多条业务线的配置中列出同一个名称，因而总是显式的。
2. **在入口检查，已有 Session 不受影响。** 检查点：建 Session（默认版本与指定版本，HTTP 与 Connection 同经 `service.Create`）、委托（子 Session 经同一入口以父 Session 的业务线创建；委托目录只列出本业务线可用的 AgentDef，`agent:*` 随之收窄）、单次调用。未列出的定义与不存在的定义返回同样的错误，不暴露其他业务线有哪些定义。Worker 读取 `State.Agent`、祖先白名单求交、Call 启动与同名换版本（`AgentSwitched`）不再检查：Session 的定义在创建时已获准。从列表中删去一个名称只阻止新的 Session、委托与单次调用；要停用已有 Session 的版本，照旧在发布配置中撤回。
3. **jwt 模式下必须声明。** `-auth jwt` 时业务线配置缺少 `agents.allow` 即启动失败；`oneshots.allow` 缺省为空（不能调用任何单次调用）。`-auth none` 的开发模式信任自报身份，未声明的业务线不受限制。`yanshi keygen` 生成的开发配置写 `["*"]`。
4. **单次调用的声明名并发槽按业务线计。** `oneshots.max_concurrency`（业务线）与合计上限不变；定义声明的 `concurrency` 改为每条业务线各自的上限，共享同一单次调用的业务线不再争抢同一组名额。
5. **MCP Server 以 (业务线, 名称) 为键。** 登记的唯一性检查、连接与工具缓存都改为按 (业务线, 名称)；工具名 `mcp_<server>__<tool>` 不变（一个 Session 只属于一条业务线，名称在其中唯一）。两条业务线可以在同一单元各自登记同名的 Server（如 `xinli`）。
6. **发布配置仍按名称。** `releases.yaml` 是运维维护的单元级文件，名称唯一，私有定义的发布只影响列出它的业务线；共享定义的发布同时作用于所有列出它的业务线，这是共享的含义。按业务线分别发布同一定义仍不做（[M4 灰度发布](../design/m4-agent-rollout.md) §1）。

## Considered Options

- **定义文件带 `business_line` 字段、名称在业务线内唯一（复合键）**：能让两条业务线使用同名的 AgentDef，但业务线名写进随代码发布的定义文件；发布配置、灰度散列、保留策略的按 Agent 覆盖、索引中的 Agent 名、评测派生版本都要改为复合键；已有 Session 的 `AgentRef` 没有业务线，需要按 `SessionCreated.business_line` 补全。换来的同名能力目前没有需求，以名称前缀约定即可。
- **维持现状，同名定义须分属不同单元**：不满足"同一单元内互不信任"的裁决。
- **在 Worker 每一步都检查**：已有 Session 的定义在创建时已获准，逐步检查只会让配置变更中途打断进行中的 Run；停用由撤回版本完成。

## Consequences

- 每条业务线的配置多了两项必须维护的列表，上线检查清单增加一项。
- 委托的 `agent:*` 在多业务线的单元中只看到本业务线的 Agent；共享的专员须在两边配置中都列出。
- 单次调用声明的 `concurrency` 语义从"全部业务线合计"改为"每条业务线"。

## 重审条件

- 两条业务线需要同名而内容不同的 AgentDef 或 Oneshot（改为复合键）；
- 业务线需要自助发布或各自发布同一共享定义（与 ADR-0020 的"需业务方自助发布"一并重审）；
- 需要撤销已有 Session 对定义的使用权，而撤回版本不够用。
