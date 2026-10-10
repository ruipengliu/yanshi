# M3 多端双工通道

> 状态：§2–§6 已实现，§7–§8 规划中 · 依赖：[M1 设备即节点](./m1-device-nodes.md)、[跨进程实时增量](./m2-live-deltas.md) · ADR-0024 · 契约：[`node.proto`](../../proto/yanshi/v1/node.proto)

## 1. 问题

章程 G1 要求同一 Session 可以多端同时在线、跨端接续。原来客户端用 HTTP 提交输入、用 SSE 接收事件，两个方向分别是两条半双工通道。移动端还要另外保持一条 Node 的 WebSocket，于是一台手机要维持两条长连接。实时 UI 交互需要的是：

- 任一端的操作（输入、插话、中断、审批、点选）立即生效，其他端立即看到结果；
- 在生成中途打开的设备，也能从头看到正在生成的文字；
- 知道哪些设备在线、正在看这个 Session（§6）；
- Agent 能给出可点选的界面，用户的选择直接回到 Run（§7）；
- 用户的输入可以带上"我正在看什么"（§8）。

语音通话（Call）暂缓，见章程 M3；本文只处理文本与界面交互。

## 2. 一条连接，两种角色（ADR-0024）

HostApp 实例与网关之间只保持一条 **Connection**（WebSocket，路径 `/v1/connect`；`/v1/nodes/connect` 保留兼容）。它可以兼任两种角色：

| 角色 | 消息 | 处理方 |
|---|---|---|
| Node | `Hello.capabilities`、`Invoke` / `InvokeResult` / `Cancel` / `ResultAck` | `node.Hub`（M1） |
| 会话客户端 | `Subscribe` / `Unsubscribe`、`ClientRequest` / `ClientResponse`、`Event`、`LiveDelta`、`SubscriptionEnded` | `channel` 包 |

- **Node 角色可选。** `Hello.client_only = true` 时只认证、不登记为 Node，用于网页等不提供 Capability 的端；此时不能声明 Capability。
- **身份来自令牌。** 和 Node 一样，以 `Hello.token` 中的用户令牌认证（[鉴权](./auth.md)）。会话客户端只能访问令牌中 EndUser 在该业务线下的 Session，其余一律按不存在处理（`not_found`），与 HTTP API 一致。令牌到期时整条连接断开，SDK 取新令牌重连。
- **编码跟随 Hello。** 二进制帧为 protobuf，文本帧为 protojson。网页可以不引入 protobuf 运行时，直接用 JSON 接入。
- **HTTP 与 SSE 保留。** 二者与 Connection 走同一套代码（`service`、`feed`），语义一致。服务端对服务端的集成、调试仍可用 HTTP。

## 3. 订阅

`Subscribe{session_id, after_seq}` 先补发 seq 大于 `after_seq` 的已提交事件，之后实时推送新事件与增量。实现与 SSE 共用 `feed.Source.Stream`：

- 先订阅增量，再读日志，二者之间的增量不会丢；
- 只在日志前进时读日志，不再每收到一个增量就读一次；
- 对外的事件清除 `AttemptStarted.live_endpoint`（ADR-0013）；
- Session 被删除时以 `SubscriptionEnded{reason: "deleted"}` 结束。

一条连接最多同时订阅 32 个 Session（`limit`）。重复订阅同一 Session 时，以新的 `after_seq` 重新开始。

**续传。** SDK 为每个订阅记下最后收到的 seq。重连后自动带上它重新订阅，并丢弃 seq 不大于它的事件，所以跨重连的事件不漏、不重。

## 4. 晚加入者补齐增量

此前（[跨进程实时增量](./m2-live-deltas.md) §4），订阅时正在生成的消息，开头部分收不到。现在：

- 增量带上 `after_seq`：这次生成接在日志的哪个位置之后。`(run_id, attempt, after_seq)` 相同的增量属于同一条消息。
- `live.MemBus` 为每个 Session 缓存正在生成的文本。新订阅者先收到一条 `snapshot = true` 的增量（到目前为止的全文），然后才是之后的增量。缓存与扇出在同一把锁下，所以 Snapshot 与其后的增量之间不漏也不重。
- 生成结束（不论提交还是失败）时，Worker 发布 `end = true`，缓存随之清除。客户端收到它时丢弃尚未被提交事件覆盖的草稿。
- 跨进程时，执行进程的内部接口同样先发 Snapshot，订阅方进程原样转发。
- 草稿超过 256 KiB 时不再补发，晚加入者等提交的事件即可。

客户端的规则：Snapshot 替换草稿；普通增量追加到草稿；`end` 或 seq 大于 `after_seq` 的 `AssistantMessage` 到达时，以提交的内容为准。

增量仍是检查之前的草稿（ADR-0021），Snapshot 也一样。

## 5. 请求

`ClientRequest` 可以是创建 Session、提交输入（新 Run 或插话）、中断、审批或关闭，每个请求恰好对应一个 `ClientResponse`（以 `request_id` 关联）。

- **与 HTTP 走同一条路径。** 请求直接调用 `service.Service`，内容安全、配额、发布配置、删除记录检查都与 HTTP 相同。错误码与 HTTP 状态码一一对应：`invalid` 400、`not_found` 404、`conflict` 409、`rejected` 422、`quota_exceeded` 429（带 `retry_at`）、`unavailable` 503、`internal` 500。
- **不阻塞 Node 角色。** 每个请求在独立的 goroutine 中执行（内容安全检查可能较慢），每条连接最多 8 个并发请求，超出时在读循环中等待，以此对客户端施加背压。
- **结果未知。** 响应到达之前连接断开时，SDK 返回 `ErrDisconnected`。提交输入不是幂等的：客户端应先从订阅中确认输入是否已出现，再决定是否重试。以客户端生成的输入 ID 去重，留待需要时再做。

SDK（`sdk/nodesdk`）：`Executor` 为 nil 时连接只作会话客户端。主要方法有 `Subscribe(sessionID, after, handler)`、`CreateSession`、`Submit` / `SubmitText`、`Interrupt`、`Decide`、`CloseSession`。回调在接收 goroutine 中按到达顺序串行调用，不应阻塞。

## 6. 在场

订阅一个 Session 的设备就算"在看"它。设备还可以用 `Activity{session_id, focused, typing}` 报告两项状态：该 Session 是否正显示在前台，以及是否正在输入。每个订阅者先收到一次 `Presence{session_id, viewers}`，之后每当列表变化时收到全量（列表包含自己）。同一设备的多条连接合并为一项。

同一 EndUser 的手机和电脑同时打开一个 Session 时，界面可以据此显示"也在 MacBook 上打开"；服务端今后也可以据此决定：用户正在某台设备上看着时，不必再推送提醒。

**存储与跨进程（`presence.Store`，mem / PostgreSQL）**

- 每个 (Session, 连接) 一条记录，带有效期（90 秒），由连接每 30 秒续期一次（`Touch`，按连接一条语句）。连接正常结束时撤下记录；进程崩溃时，记录最迟 90 秒后过期。
- "正在输入"也带有效期（10 秒）：客户端忘了置回 false 也不会一直显示。持续输入时，客户端每隔几秒重发一次。
- 显示内容变化时，每个 Session 的版本号递增，并发出 `NOTIFY yanshi_presence`。每个订阅中的推送协程按版本号等待：通知是主要途径，兜底轮询间隔为 15 秒（`Notifier.WaitForEvery`），不用默认的 2 秒，因为等待者与订阅一样多。记录或输入状态过期不会产生通知，推送协程在最早的过期时刻自行重读。
- 在场变化按用户操作计（打开、切走、开始或停止输入），频率远低于 token 增量，所以可以用 `NOTIFY`。ADR-0013 拒绝的是逐 token 的 `NOTIFY`。续期不发通知。

**删除。** 记录含有 Session ID，属于 Session 数据（ADR-0015）：

- `presence.Service.Put` 写入后复查删除记录，Session 已删除就撤回；
- Janitor 清扫时按 Session 删除，`Sweep` 顺带清理过期记录；
- 模拟测试会向已删除的 Session 写入在场记录（对应"授权检查通过之后、写入之前 Session 被删除"的竞争），检查它被撤回，并在删除不变量中检查没有残留。差分测试比较内存与 PostgreSQL 实现的在场记录。

**顺序。** 一条连接重复订阅同一 Session 时，新订阅等旧订阅撤下记录之后才写入，因为二者用的是同一个 (Session, 连接) 键。设备在授权检查完成之前发来的 Activity，先记在订阅上，通过检查后再随加入一并写入。

SDK：`SetActivity(sessionID, focused, typing)`；处理器实现可选接口 `PresenceHandler.OnPresence` 即可收到列表。重连后，SDK 随订阅重发前台状态，"正在输入"是瞬时的，不重发。

## 7. Agent 生成的交互式界面（规划中）

## 8. 界面上下文（规划中）

## 9. 测试

- `live`：晚加入者收到 Snapshot；结束标记清除草稿；新的生成重新累积；草稿溢出时不补发；并发发布与订阅时，Snapshot 加上其后的增量恰好是全文。
- `live/peer`：远程晚加入者从执行进程收到 Snapshot。
- e2e（`channel_test.go`）：
  - 两台设备订阅同一 Session，生成中途加入的设备拼出的草稿与完整输出一致，事件从 seq 1 连续；
  - 同一条连接既是 Node 又是客户端：看到审批、经同一连接批准、执行；
  - 访问他人的 Session 一律 `not_found`；
  - 文本帧按 protojson 收发，且 `client_only` 的连接不出现在 Node 列表中；
  - 令牌到期重连后续订，事件连续；
  - 两台设备连在共享 PostgreSQL 的两个实例上，互相看到对方加入、聚焦、正在输入、停止输入、离开。
- `presencetest` 一致性套件（mem、PostgreSQL）：只有显示内容变化才递增版本，过期记录不列出，删除与隔离，等待被变化唤醒，按 Session 删除。
- 模拟测试：在场写入与删除竞争时被撤回（`PresenceWrites`、`PresenceWithdrawn`），删除不变量，差分指纹包含在场记录。
