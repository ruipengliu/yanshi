---
status: accepted
---

# MCP Server 作为 Capability 提供者：远程由 Worker 调用，本机经设备桥接

远程 MCP Server（Streamable HTTP）由业务线登记，Worker 进程内的受信客户端以业务线级凭证调用，语义与进程内 Capability 相同。用户电脑上的 MCP Server 由端侧 SDK 桥接成设备 Capability，经 Inbox 路由，数据与凭证留在本机。未声明只读的工具默认需要审批，只能由业务线逐个放宽。

我们没有选择在云端运行 stdio 型 MCP Server：它们是不可信代码，只能放进沙箱（ADR-0008），而沙箱默认无网络，多数 Server 却需要联网，安全边界需要另行设计。也没有在第一阶段做用户级 OAuth：令牌保管、刷新与删除（ADR-0015）的工作量和风险都显著更大，业务线级凭证已经能覆盖"业务线把自有服务开放给 Agent"这一主要场景。

## Consequences

- MCP 工具的幂等性与审批来自工具注解，并受业务线登记约束；第三方注解不能单独放宽审批。
- 远程 MCP 的 URL 就是私有化部署中需要放通的外联项，由登记文件显式列出（ADR-0001）。
- 设备端桥接复用 M1 的全部机制（离线挂起、去重、审批），不需要新的协议。
