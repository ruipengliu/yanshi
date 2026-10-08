---
status: accepted
---

# 设备即节点：端侧能力经嵌入 HostApp 的 SDK 暴露

手机和电脑上的本地能力，由嵌入业务线 HostApp 的 yanshi SDK 以 Node 身份主动连接云端并声明 Capability 来提供（电脑端同样以 App 形态存在），而不是由一个统一的 yanshi 独立客户端提供。对 Agent 而言，Device 上的 Capability 与云端 Sandbox、云服务的 Capability 是同一种东西，由能力路由统一处理在线状态、Approval、超时与幂等。

## Consequences

- 一台 Device 上多个 HostApp 是多个 Node；Capability 的可见性受 BusinessLine 边界约束。
- Node 必然会频繁离线（移动端后台限制、电脑休眠），离线是一等语义：调用 Device Capability 的 Run 必须能挂起等待，并可经推送唤醒。
