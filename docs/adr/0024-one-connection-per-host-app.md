---
status: accepted
---

# 每个 HostApp 实例一条 Connection，兼任 Node 与会话客户端

HostApp 与网关之间只保持一条 WebSocket（Connection）。它既可以作为 Node 声明 Capability、执行调用，也可以作为会话客户端订阅 Session、提交输入与审批。会话客户端角色复用 HTTP API 背后的 `service` 与事件流（`feed`），HTTP 与 SSE 保留。

我们没有为会话客户端另开一条连接，原因有两点：

- 手机上每多一条长连接，就多一份保活耗电和一次唤醒后的重连。推送唤醒之后，一条连接恢复，Node 与界面就同时恢复。
- 身份、令牌到期断开、重连退避这些逻辑只需要实现一次。

## Consequences

- Node 角色变成可选：`Hello.client_only` 的连接不登记为 Node，网页等端也可以接入。
- 会话客户端的慢操作（内容安全检查）不能阻塞同一连接上的设备结果，所以请求并发执行，并按连接限流。
- 协议向后兼容：只新增消息与字段，旧 SDK 不受影响。
- 帧编码跟随 Hello（protobuf 或 protojson），网关要同时支持两种编码。
