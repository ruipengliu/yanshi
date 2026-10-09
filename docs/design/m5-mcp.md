# M5 MCP 适配

> 状态：设计中 · 依赖：[M1 设备即节点](./m1-device-nodes.md)、[鉴权](./auth.md)、ADR-0008 · ADR-0017

## 1. 定位

MCP（Model Context Protocol）Server 是现成的工具生态。yanshi 把它们接成 Capability 的提供者，有两种方式：

| 方式 | 谁运行 MCP Server | 谁调用 | 数据在哪里 |
|---|---|---|---|
| **远程 MCP**（§2–§4） | 业务线或第三方，以 Streamable HTTP 对外提供 | Worker 进程内的受信 MCP 客户端 | 业务线的服务 |
| **设备端桥接**（§5） | EndUser 的电脑，例如文件、日历、本地应用 | HostApp 中的端侧 SDK，经 Inbox 路由 | 留在用户本机 |

云端不运行 stdio 型 MCP Server：那属于不可信代码，按 ADR-0008 只能放进沙箱，而沙箱默认没有网络，这需要另行设计安全边界。

第一阶段只接入 MCP 的 **tools**；resources、prompts、sampling、elicitation 留到以后。

## 2. 远程 MCP 的登记

业务线在 `mcp/*.yaml` 中登记 MCP Server，与 AgentDef 一样是版本化的声明：

```yaml
name: crm                      # 工具名前缀，[a-z0-9-]
business_line: demo            # 只有该业务线的 Session 可以使用
url: https://crm.example.com/mcp
headers:                       # 业务线级凭证；${VAR} 从环境变量注入，不写明文
  Authorization: "Bearer ${CRM_MCP_TOKEN}"
timeout: 30s
tools:
  allow: ["search_*", "get_*", "create_ticket"]   # 可选：只开放这些工具
  no_approval: ["create_ticket"]                  # 可选：业务线确认无需审批的工具
```

- **模型看到的工具名**是 `mcp_<server>__<tool>`，例如 `mcp_crm__search_customer`。设备标签只含 `[a-z0-9-]`、不含下划线，所以不会与 `<label>__<capability>` 冲突。超过 64 字符的名字会截断，并附加短哈希。
- **AgentDef 白名单**使用 `mcp:<server>/<glob>`，例如 `mcp:crm/*`，与 `device:`、`sandbox:` 的写法一致。
- **工具发现**：启动时调用 `tools/list`，结果缓存 5 分钟。调用时发现工具不存在，会立即刷新一次缓存。
- **出网白名单**：yanshi 只连接登记过的 URL，模型无法指定任意地址。私有化部署中，这些 URL 就是需要放通的外联项（ADR-0001）。

## 3. 调用语义

远程 MCP 工具按**进程内 Capability** 执行（M0 设计 §4）：

1. 执行前先落盘 `ToolCallStarted`；
2. Worker 在 `during` 中调用 MCP Server，被中断或接管时取消 HTTP 请求，并向 Server 发送 `notifications/cancelled`；
3. 结果写入 `ToolResult`。

如果 Worker 在执行中途崩溃，接管者按幂等性处理：幂等的工具重新执行，否则返回"结果未知"，不会重复执行有副作用的调用。

**工具注解如何映射为风险与幂等性**：

| MCP 注解 | yanshi |
|---|---|
| `readOnlyHint: true` | 低风险，无需审批；幂等 |
| `idempotentHint: true` | 幂等（崩溃后可以重新执行） |
| 未声明只读 | **需要审批**（安全默认）；业务线可在登记中用 `no_approval` 逐个放宽 |
| 明确声明 `destructiveHint: true` | 需要审批，不能用 `no_approval` 放宽（规范中未声明时默认也是 true，但那种情况允许业务线放宽，否则没有注解的工具永远无法免审批） |

第三方 Server 的注解只当作提示：放宽审批必须经过业务线的明确登记。

**身份传递**：每次调用都带上 `X-Yanshi-Business-Line`、`X-Yanshi-End-User`、`X-Yanshi-Session` 请求头，以及 MCP 请求的 `_meta`（`yanshi/end_user` 等）。业务线的 Server 依靠这些信息按用户处理数据。它可以信任这些值，因为调用来自持有业务线凭证的 yanshi。

## 4. 结果映射

| MCP 内容 | yanshi ContentBlock |
|---|---|
| `text` | Text |
| `image`、`audio`（base64） | 保存为当前 Session 的 Artifact，以 Media 引用（字节不进日志） |
| `resource_link` | 一行描述文本（URI、名称、类型） |
| 内嵌 `resource` | 文本内容直接作为 Text；二进制保存为 Artifact |
| `structuredContent` | JSON 文本（有 `content` 时作为补充） |
| `isError: true` | 错误结果 |

单个结果中的文本超过 64KB 时截断并注明（上下文中还会再按 `max_tool_result` 截断）。工具结果一律视为**不可信数据**，不能提升权限（章程原则 6）。

## 5. 设备端桥接

电脑上的 HostApp 用端侧 SDK 的 `nodesdk.MCPBridge` 连接本机的 MCP Server：

- stdio：由 HostApp 启动的子进程，例如 `npx @modelcontextprotocol/server-filesystem ~/Documents`；
- 本机 HTTP。

桥接会把这些 Server 的工具声明为该 Node 的 Capability，名称是 `<server>_<tool>`，模型看到的工具名是 `<label>__<server>_<tool>`。风险与幂等性的映射同 §3：只读的工具无需审批，其余需要审批。

调用沿用设备节点的全部机制：Inbox 投递、离线挂起、以 `call_id` 去重、审批、超时。SDK 收到调用后转发给本机 MCP Server。

这种方式下，用户数据与 MCP Server 的凭证都留在本机，yanshi 只看到工具声明和调用结果。`yanshi node` 新增 `-mcp name=command` 参数，用来演示。

## 6. 不做

- **用户级 OAuth**，例如连接用户自己的邮箱账号：需要令牌保管与授权流程，留待后续。届时登记中增加 `auth: oauth`，令牌按 (EndUser, Server) 存放，并纳入 ADR-0015 的删除流程。
- **把 yanshi 自身暴露为 MCP Server**，以及 A2A。
- **云端运行 stdio 型 Server**（§1）。

## 7. 测试

- 用 Go MCP SDK 在测试进程内启动 MCP Server，测试以下内容：工具发现与白名单；注解与审批的映射；结果映射（含图片保存为 Artifact）；超时与中断取消；Server 不可用时返回错误结果。
- 崩溃后的幂等语义复用进程内 Capability 的路径，模拟测试已经覆盖。另外加一个单元测试，确认 MCP 工具的 `Idempotent` 取自注解。
- e2e：远程 MCP 需要审批的工具，从审批到执行的完整流程；设备端桥接中，SDK 把本机（内存传输的）MCP Server 的工具暴露为设备能力，调用后得到结果。
- 授权：其他业务线的 Session 看不到不属于自己的 MCP Server。
