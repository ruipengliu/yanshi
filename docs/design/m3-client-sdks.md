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
- 提交旁听转写（[旁听](./captures.md) §3）时，批次 ID 由调用方给出：HostApp 随本地缓冲持久保存它，重启后补交同一批时用原来的 ID。断线时同样以同一 ID 重试，直到得到结果；没有 ID 的提交在发出之前就被拒绝。
- 其余请求在断线时报告"结果未知"，由调用方决定是否重试，因为重复执行不会产生重复效果。创建 Session 例外，重试会再创建一个。
- 重连后随订阅重发前台状态；"正在输入"是瞬时的，不重发。
- 回调按到达顺序串行调用。
- 请求在发出之前已被取消（AbortSignal、ctx）时不发出：发出之后的取消只是不再等待，请求（例如批准）仍会生效。
- 写入有期限：写入卡住（网络黑洞、对端不读）时，请求在自己的期限到时返回，连接随之关闭、重连；会话状态锁内不做网络写入（Go）。
- 应用对一个 Session 至多一个订阅，重复订阅时新的取代旧的。`PendingTracker` 对子 Session 的订阅与它并存、各自取消，应用另外打开子 Session 时互不挤掉：
  - 网关上一个 Session 仍只有一个订阅，从各订阅者中最小的 seq 续传，每个订阅者各自丢弃已收到的事件；
  - 应用取消订阅而跟踪器还在时，撤下前台状态，否则网关以为用户仍在看这个 Session，不推送它的提醒。

## 2. 网页（`sdk/web`）

```ts
import { Client, Draft, PendingQuestions, PendingTracker } from "@yanshi/client";

const client = new Client({
  url: "wss://example.com/v1/connect",
  deviceId: persistentDeviceId(),            // 存在 localStorage，跨刷新不变
  tokenSource: () => fetchTokenFromBusinessLine(),
  label: "Chrome", kind: "browser",
});
client.start();

const draft = new Draft(), questions = new PendingQuestions();
// 进行中的委托里子 Agent 的提问与审批：辅助类自己订阅子 Session，变化时回调。
const pending = new PendingTracker(client, sessionId, { onChange: (items) => renderPending(items) });
client.subscribe(sessionId, lastSeq, {
  onEvent: (e) => { draft.onEvent(e); questions.onEvent(e); pending.onEvent(e); render(e); },
  onDelta: (d) => { draft.onDelta(d); renderDraft(draft.text); },
  onPresence: (p) => renderViewers(p.viewers),
});
client.setActivity(sessionId, document.visibilityState === "visible");
await client.submitText(sessionId, "这个什么时候到？", { uiContext: { screen: "订单详情", ref: "order://A1029" } });
await client.answer(sessionId, questions.questions[0].callId, { selected: ["b"] });

// 输入框的文字：恰好一个子 Agent 的提问在等回答时，发往子 Session 作为它的回答。
const q = pending.typedAnswerTarget();
if (q) await client.answer(q.sessionId, q.callId, { text: typed });  // conflict 时保留文字、提示，不改投
else await client.submitText(sessionId, typed);
```

- **编码。** 消息类型由 protobuf-es 从 `proto/` 生成（`sdk/web/src/gen`，随 `make gen` 更新并提交），线上是二进制帧。运行时依赖只有 `@bufbuild/protobuf`。
- **环境。** 默认使用全局 `WebSocket`（浏览器、Node.js 22+）。小程序等环境通过 `webSocket` 工厂适配，只需提供 `send`、`close` 与 `on*` 回调。
- **seq 是 `bigint`**（proto 的 uint64），持久保存续传位置时转成字符串。
- **渲染辅助。**
  - `Draft` 按 [§4 的规则](./m3-duplex-channel.md#4-晚加入者补齐增量)维护正在生成的草稿：Snapshot 替换，增量追加；`end`，或 seq 大于草稿 `after_seq` 的 AssistantMessage 到达时清除；已提交的生成迟到的增量忽略。
    - 草稿所属的 Run 结束（completed、failed、interrupted）时也清除，之后该 Run 迟到的增量不再建立草稿。断线期间生成被丢弃、Run 随后结束时，客户端收不到 `end`，重连后补发的只有终态事件，没有这条规则草稿会一直留着。
  - `PendingQuestions` 跟踪等待回答的 ask_user 提问：
    - 该调用派发给用户（`ToolCallStarted{node_id: "@user"}`）之后才算待回答，只出现在 AssistantMessage 中还不算。Worker 按顺序一次推进一个调用，模型同一轮返回"委托 + ask_user"时，提问要等委托结束才派发；在那之前服务端不接受对它的回答；
    - 在该调用出现 ToolResult 或 Run 结束时收起。
  - `questionOf` 从工具调用中解析问题。
  - `Cards` 跟踪展示过的卡片（同一 `card_id` 只留最新版）与已执行的动作（`taken(callId, action)`）。`cardActionOf` 从事件中读出已执行的卡片动作：输入（`RunRequested`、`Steered`）中的，或 Run 等回答时作为回答、记在 `ToolResult.card_action` 中的（[卡片](./cards.md) §5）。Go SDK 中是 `nodesdk.CardOf`、`nodesdk.CardActionOf`。
  - `BackgroundCalls` 跟踪未了结的后台调用（ADR-0035，[M1 设计](./m1-device-nodes.md) §3.1）：带 `backgrounded` 的 `ToolResult` 使调用转入后台；输入中的 `backgroundResult`（设备结果或到期）与 `BackgroundCancelled` 了结它。界面据此列出它们，`client.cancelBackground(sessionId, callId)` 取消（已了结时为 `conflict`）。`backgroundResultOf` 从 `RunRequested` 或 `Steered` 中读出后台调用的了结：它是平台送达的说明，不要渲染成用户的气泡。Go SDK 中是 `nodesdk.BackgroundCalls`、`nodesdk.BackgroundResultOf` 与 `Client.CancelBackground`。
  - `PendingTracker` 跟踪顶层 Session 及其进行中的委托（递归）里等待用户的提问与审批（[委托](./m5-multi-agent.md) §4）。
    - 顶层 Session 的事件由应用喂给它。被跟踪的 Session 出现委托时，它经 Client 从头订阅子 Session；委托调用出现结果、或所在 Run 结束时，退订并收起其下的待处理项。
    - Worker 先提交 `ToolCallStarted`、再创建子 Session，发现委托立即订阅时子 Session 可能还不存在。子 Session 的订阅以 `not_found` 或 `error` 结束、而委托仍在进行时，退避后重订（200ms 起翻倍，至多 5 秒），从已收到的位置续传，已有的待处理项保留。以其他原因（`deleted` 等）结束时收起其下的待处理项，不再订阅。
    - 提问与 `PendingQuestions` 一样，派发给用户之后才算待回答。审批在 ApprovalDecided、该调用的结果或 Run 结束时收起。
    - 每个待处理项带 kind（question、approval）、sessionId（回答或审批发往的 Session）、callId、runId、agent（子 Agent 名）、via（经由顶层的哪个直接子 Session），以及提问内容或审批说明与能力名。
    - `typedAnswerTarget()` 给出输入框的文字应回答的提问：顶层自己没有待回答的提问、整棵树里恰好一个、且在子 Session 中时返回它，否则文字照旧提交给顶层 Session。顶层尚未派发的提问不挡住子 Agent 的提问。
    - `close()` 取消全部子订阅与等待中的重订。
    - 和 `Draft`、`PendingQuestions` 一样，喂给它的事件要从 seq 0 开始，否则看不到续传位置之前开始的委托与提问。
    - Go SDK 中是等价的 `nodesdk.PendingTracker`（`nodesdk.NewPendingTracker`、`OnEvent`、`Items`、`TypedAnswerTarget`、`Close`）。`OnEvent` 只更新状态，可在 SessionHandler 中调用；订阅子 Session 与 onChange 都在跟踪器自己的 goroutine 中串行进行，不阻塞连接的接收 goroutine。
- **旁听转写。** `client.submitCaptures(sessionId, captureId, segments)` 提交一批带说话人的转写（ADR-0036），返回 `{duplicate, redacted}`；`newCaptureId()` 生成批次 ID。Go SDK 中是 `Client.SubmitCaptures(ctx, *v1.SubmitCaptures)` 与 `nodesdk.NewCaptureID`。
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

麦克风采集、重采样与播放见调试控制台（`internal/webui/static/console/audio.js`）。Go 与移动端的通话接口尚未提供。

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
- **消息类型。** 原生侧用 swift-protobuf / protobuf-javalite 从 `proto/` 生成，绑定层不附带。渲染辅助（卡片、已执行的卡片动作、后台调用）同样不经绑定层，原生端按 §2 的规则从事件里读：卡片动作看输入与回答的 `ToolResult.card_action` 三处；后台调用看带 `backgrounded` 的结果、输入中唯一的 `background_result` 块与 `BackgroundCancelled`。
- **取消后台调用。** `CancelBackground(sessionID, callID, timeoutMillis) → ClientResponse 字节`，语义同网页与 Go SDK。
- **旁听转写。** 片段是重复字段，绑定层不提供单独的方法：原生侧用生成的消息类型构造 `ClientRequest{submit_captures}`，经 `Request` 提交。绑定层像提交输入一样以 `capture_id` 去重、断线后自动重试。`NewCaptureID()` 生成批次 ID，HostApp 随本地缓冲持久保存它。
- **委托。** 绑定层不提供 `PendingTracker`，原生端须自行跟随进行中的委托，规则同 §2 与[委托](./m5-multi-agent.md) §4：
  - 发现 `ToolCallStarted{node_id: "@agent/<子 Session>"}` 即从 seq 0 订阅子 Session；订阅以 `not_found` 或 `error` 结束而委托仍在进行时，退避后从已收到的位置重订；
  - 把其中待处理的提问与审批一并展示，回答与审批发往子 Session；提问在 `ToolCallStarted{node_id: "@user"}` 之后才算待回答；
  - 委托调用出现结果、或所在 Run 结束时退订，并收起这些待处理项；
  - 输入框的文字按同样的规则选择发往子 Session 的 `answer`，还是提交给顶层 Session。

  一个 Session 在绑定层只能有一个订阅（新的取代旧的），原生端同时展示子 Session 时，自己把事件分发给多个视图。

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
  - `Draft` 与 `PendingQuestions` 的规则，含 Run 结束时清除草稿、提问派发之后才待回答；`Cards` 与 `cardActionOf`（含作为回答的卡片动作）；
  - `BackgroundCalls`：转入后台、设备结果、到期、取消，按截止时间排序；`cancelBackground` 发出请求、已了结时为 `conflict`；
  - `submitCaptures` 发出给定的批次 ID 与片段，断线后以同一 ID 重试；没有批次 ID 时不发出；
  - 同一 Session 上应用的订阅与辅助类的订阅并存：从最小的 seq 续订、各自去重、各自取消，应用取消后撤下前台状态；
  - `PendingTracker`：
    - 发现委托即订阅子 Session，子 Session 的提问与审批带 sessionId、agent、via；
    - 递归到孙 Session；
    - 委托调用的结果或 Run 结束时退订并收起，`close` 退订全部；
    - 审批的收起；委托成环时不重复订阅；
    - `typedAnswerTarget` 的各分支：没有提问、恰好一个子提问、两个提问、顶层自己有提问；同一轮的委托与提问，顶层的提问派发之前不挡住子提问；
    - 子订阅以 `not_found`、`error` 结束时退避重订并从已收到的位置续传，`deleted`、委托结束或 `close` 之后不再重订。
- 网页互通（`make check`，`internal/e2e` 的 `TestWebSDK` 启动网关，以 Node.js 运行 `sdk/web/test/interop.ts`）：
  - 两台网页设备，生成中途订阅的一台先收到 Snapshot，两台拼出的草稿都与全文一致，提交后草稿清除；
  - 在场；
  - ask_user 由另一台设备回答，不合法的回答为 `invalid`，重复回答为 `conflict`，两端的问题都收起；
  - 同一输入 ID 再次提交得到 `duplicate`；
  - 委托出去的子 Agent 提问：`PendingTracker` 看到它，`typedAnswerTarget` 选中它，以文字 `answer` 发往子 Session 后父 Run 完成，再次回答为 `conflict`；
  - 他人的 Session 为 `not_found`。
- Go SDK（`make check`）：`nodesdk` 的单元测试覆盖与网页相同的 `PendingTracker` 规则和订阅并存，以及 `CardActionOf`、`BackgroundCalls`；e2e `TestCancelBackgroundOverConnection` 经 Connection 取消后台调用，设备上线后不执行，再次取消为 `conflict`；e2e `TestDelegatedQuestionAnsweredByText` 在真实网关上委托、子 Agent 提问、以文字回答、父 Run 完成；e2e `TestSubmitCapturesOverConnection` 经 Connection 提交旁听转写，不开启 Run，同一批次 ID 再次提交为 `duplicate`，未开启旁听的 AgentDef 为 `invalid`，他人的 Session 为 `not_found`。
- 移动端绑定（`make check`，`TestMobileBinding`）：
  - 以字节收发请求与事件；
  - 原生能力上传工件、返回错误；
  - 拒绝放在响应中；
  - 在场；
  - 启动后不能再声明能力；
  - 旁听转写经 `Request` 提交，同一批次 ID 再次提交为 `duplicate`；
  - 停止后请求立即失败。
- `make mobile`（需要 Xcode 与 JDK，不在 `make check` 中，改动 `sdk/nodesdk`、`sdk/mobile` 或 `node.proto` 后运行）：
  - gomobile 生成 iOS、iOS 模拟器与 macOS 的 `Yanshi.xcframework`；
  - Swift 冒烟程序（`sdk/mobile/smoke`，用 swift-protobuf）先在本机、再在 iOS 模拟器中对网关跑一遍：声明原生能力、创建 Session、订阅、提交、看到能力结果、请求被拒绝；
  - Android 只编译 gobind 生成的 Java 绑定。本机没有 NDK，aar 的构建与真机验证列入[上线检查清单](../launch-checklist.md)。
