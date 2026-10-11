# M3 端侧 SDK

> 状态：已实现（Android aar 未实测） · 依赖：[多端双工通道](./m3-duplex-channel.md)、[M1 设备即节点](./m1-device-nodes.md) · ADR-0024、ADR-0026 · 契约：[`node.proto`](../../proto/yanshi/v1/node.proto)

## 1. 三个 SDK

| SDK | 端 | 角色 | 实现 |
|---|---|---|---|
| `sdk/nodesdk`（Go） | 电脑、服务端集成 | Node + 会话客户端 | 参考实现 |
| `sdk/mobile`（gomobile） | iOS、Android | Node + 会话客户端 | 绑定 `nodesdk`，字节进、字节出 |
| `sdk/web`（TypeScript，`@yanshi/client`） | 网页、小程序、H5 | 会话客户端（`client_only`） | 按协议实现，类型由 `proto/` 生成 |

三者的语义相同（ADR-0026）：

- 断线以指数退避重连（0.5 秒起，至多 30 秒，带抖动），每次连接重新取令牌。网关同时进行的握手有上限（`wsgateway.Gateway.MaxHandshakes`，默认 128）：网关滚动重启后，成批的连接会同时重连，排不上的以 503 拒绝，并给出带抖动的 Retry-After（2～10 秒）。Go SDK 按 Retry-After 等待；浏览器看不到握手失败的状态码，只按退避重试。
- 重连后按各订阅最后收到的 seq 续订，并丢弃不比它新的事件：事件不漏、不重。
- 提交输入时生成输入 ID。连接在响应之前断开时，重连后以同一 ID 重试，直到得到结果：输入恰好生效一次。
- 其余请求在断线时报告"结果未知"，由调用方决定是否重试，因为重复执行不会产生重复效果。创建 Session 例外，重试会再创建一个。
- 重连后随订阅重发前台状态；"正在输入"是瞬时的，不重发。
- 回调按到达顺序串行调用。
- 请求在发出之前已被取消（AbortSignal、ctx）时不发出：发出之后的取消只是不再等待，请求（例如批准）仍会生效。
- 写入有期限：写入卡住（网络黑洞、对端不读）时，请求在自己的期限到时返回，连接随之关闭、重连；会话状态锁内不做网络写入（Go）。

## 2. 网页（`sdk/web`）

```ts
import { Client, Draft, PendingQuestions } from "@yanshi/client";

const client = new Client({
  url: "wss://example.com/v1/connect",
  deviceId: persistentDeviceId(),            // 存在 localStorage，跨刷新不变
  tokenSource: () => fetchTokenFromBusinessLine(),
  label: "Chrome", kind: "browser",
});
client.start();

const draft = new Draft(), questions = new PendingQuestions();
client.subscribe(sessionId, lastSeq, {
  onEvent: (e) => { draft.onEvent(e); questions.onEvent(e); render(e); },
  onDelta: (d) => { draft.onDelta(d); renderDraft(draft.text); },
  onPresence: (p) => renderViewers(p.viewers),
});
client.setActivity(sessionId, document.visibilityState === "visible");
await client.submitText(sessionId, "这个什么时候到？", { uiContext: { screen: "订单详情", ref: "order://A1029" } });
await client.answer(sessionId, questions.questions[0].callId, { selected: ["b"] });
```

- **编码。** 消息类型由 protobuf-es 从 `proto/` 生成（`sdk/web/src/gen`，随 `make gen` 更新并提交），线上是二进制帧。运行时依赖只有 `@bufbuild/protobuf`。
- **环境。** 默认使用全局 `WebSocket`（浏览器、Node.js 22+）。小程序等环境通过 `webSocket` 工厂适配，只需提供 `send`、`close` 与 `on*` 回调。
- **seq 是 `bigint`**（proto 的 uint64），持久保存续传位置时转成字符串。
- **渲染辅助。**
  - `Draft` 按 [§4 的规则](./m3-duplex-channel.md#4-晚加入者补齐增量)维护正在生成的草稿：Snapshot 替换，增量追加；`end`，或 seq 大于草稿 `after_seq` 的 AssistantMessage 到达时清除；已提交的生成迟到的增量忽略。
  - `PendingQuestions` 跟踪等待回答的 ask_user 提问，在该调用出现 ToolResult 或 Run 结束时收起。
  - `questionOf` 从工具调用中解析问题。
- **错误。**
  - `RequestError.code` 与 HTTP 状态码对应，`quota_exceeded` 带 `retryAt`。
  - `DisconnectedError` 表示结果未知，`StoppedError` 表示客户端已停止。
  - 请求接受 `AbortSignal`。
  - 回调抛出的异常交给 `onHandlerError`，不中断连接。

**通话**（[Call](./m3-call.md) §8）：`await client.startCall(sessionId, {onAudio, onSpeech, onText, onTask, onEnded})` 在接通后返回 `Call`：
- `sendAudio(Int16Array)` 发送 16 kHz 的麦克风音频；
- `mute`、`interrupt`、`hangup`；
- `onAudio` 收到 24 kHz 的语音；
- 连接断开时 `onEnded("disconnected")`。

麦克风采集、重采样与播放见网页通话页（`internal/webui/static/call.js`）。Go 与移动端的通话接口尚未提供。

## 3. 移动端（`sdk/mobile`）

gomobile 只能跨语言传基本类型、`[]byte`、结构体指针与接口，所以：

| 方向 | 形式 |
|---|---|
| 请求 | `Request(ClientRequest 字节, timeoutMillis) → ClientResponse 字节`。网关的拒绝放在响应的 `error` 中；返回 error 只表示没有得到响应（超时、停止、断线后结果未知） |
| 订阅 | `Subscribe(sessionID, afterSeq, SessionHandler)`，回调参数为 `Event` / `LiveDelta` / `Presence` 字节 |
| 原生能力 | `AddCapability(CapabilitySpec 字节, CapabilityHandler)`。`Execute(Invoke 字节) → InvokeResult 字节`，返回 error 时作为错误结果交给 Agent |
| 令牌 | `TokenSource.Token() string`，返回空串表示暂时取不到，稍后重试。不用 error 返回值，因为 gomobile 对返回字符串的方法不生成 Swift 的 `throws` |
| 推送 | `Config.PushPlatform` + `PushTokenSource` |
| 工件 | `UploadArtifact(sessionID, name, mime, data) → ContentBlock 字节`，通常在能力中上传照片、文件 |

Swift 中（ObjC 前缀 `Yanshi`，协议名加 `Protocol` 后缀）：

```swift
let cfg = YanshiConfig()
cfg.url = "wss://example.com/v1/connect"; cfg.nodeID = deviceID; cfg.ledgerDir = appSupportDir
let client = YanshiClient(cfg)!
client.setTokenSource(tokens)                       // YanshiTokenSourceProtocol
try client.addCapability(spec.serializedData(), handler: TakePhoto())
try client.start()
let resp = try Yanshi_V1_ClientResponse(serializedBytes: client.submitText(sid, text: "你好", timeoutMillis: 10_000))
```

- **线程。** 回调在 Go 的线程中调用，更新界面前切回主线程。`Request`、`UploadArtifact` 会阻塞，应在后台线程调用。
- **账本。** 声明了 Capability 时必须给出 `LedgerDir`（应用私有目录）。账本保证 App 被杀后重启时，有副作用的调用不会重复执行。清除某 Session 的记录时，仍在执行的该 Session 的调用被取消，之后才写下的记录随即删除（`nodesdk.Executor` 在进程内记住清除标记）。
- **取消。** 原生能力无法从 Go 中止：调用被取消时 SDK 不再等待结果，原生代码自行结束。
- **消息类型。** 原生侧用 swift-protobuf / protobuf-javalite 从 `proto/` 生成，绑定层不附带。

## 4. 测试

- `sdk/web` 单元测试（`make check`，用假 WebSocket）：
  - Hello 声明 `client_only`，请求等到 Welcome 之后才发出；
  - 错误码与配额重置时间；
  - 重连后按 seq 续订，丢弃重放的事件，重发前台状态；
  - 响应丢失的提交以同一 ID 重试；
  - 其他请求在断线时报告结果未知；
  - stop 之后请求失败且不再重连；
  - AbortSignal 取消请求，已取消的请求与 Call 不发出；
  - 订阅结束、取消订阅、在场；
  - 回调抛出异常不影响连接；
  - 每次连接重新取令牌；
  - `Draft` 与 `PendingQuestions` 的规则。
- 网页互通（`make check`，`internal/e2e` 的 `TestWebSDK` 启动网关，以 Node.js 运行 `sdk/web/test/interop.ts`）：
  - 两台网页设备，生成中途订阅的一台先收到 Snapshot，两台拼出的草稿都与全文一致，提交后草稿清除；
  - 在场；
  - ask_user 由另一台设备回答，不合法的回答为 `invalid`，重复回答为 `conflict`，两端的问题都收起；
  - 同一输入 ID 再次提交得到 `duplicate`；
  - 他人的 Session 为 `not_found`。
- 移动端绑定（`make check`，`TestMobileBinding`）：
  - 以字节收发请求与事件；
  - 原生能力上传工件、返回错误；
  - 拒绝放在响应中；
  - 在场；
  - 启动后不能再声明能力；
  - 停止后请求立即失败。
- `make mobile`（需要 Xcode 与 JDK，不在 `make check` 中，改动 `sdk/nodesdk`、`sdk/mobile` 或 `node.proto` 后运行）：
  - gomobile 生成 iOS、iOS 模拟器与 macOS 的 `Yanshi.xcframework`；
  - Swift 冒烟程序（`sdk/mobile/smoke`，用 swift-protobuf）先在本机、再在 iOS 模拟器中对网关跑一遍：声明原生能力、创建 Session、订阅、提交、看到能力结果、请求被拒绝；
  - Android 只编译 gobind 生成的 Java 绑定。本机没有 NDK，aar 的构建与真机验证列入[上线检查清单](../launch-checklist.md)。
