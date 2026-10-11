# M3 Call：全双工语音通话

> 状态：已实现（服务端、网页 SDK 与网页通话页；Go 与移动端 SDK 的通话接口待做） · 依赖：[多端双工通道](./m3-duplex-channel.md)、[端侧 SDK](./m3-client-sdks.md) · ADR-0005、ADR-0027 · 契约：[`node.proto`](../../proto/yanshi/v1/node.proto) 的 Call 消息、[`event.proto`](../../proto/yanshi/v1/event.proto) 的 `CallStarted` / `CallTranscript` / `CallEnded`

## 1. 目标

场景 B（章程 §6）："用户做饭时用手机与助手语音通话：帮我看看电脑上那份 PPT 第三页写了什么……标题改得更口语一点。"

- 全双工：用户随时开口，模型的服务端 VAD 判断轮次并打断播报。
- 低延迟：对话由端到端实时语音模型完成，不经"识别 → 文本模型 → 合成"的级联。
- 通话中做事：需要能力的工作派生为后台 Run，结果回注通话并播报（ADR-0005）。
- 跨端一致：通话说过什么写入日志，其他设备、之后的文字对话与 Run 都看得到。

## 2. 架构（ADR-0027）

```
设备（网页 / App）──Connection──▶ 网关进程 internal/call ──WebSocket──▶ 实时语音模型（豆包 Seeduplex）
   麦克风 PCM 16k  ─────────▶  节拍器（20 ms/帧）  ─────────▶  音频
   播放 PCM 24k    ◀─────────  转写、语音、打断  ◀─────────  事件
                                │  转写（内容安全）──▶ service ──▶ Event 日志
                                │  run_task ─────────▶ service.SubmitFromCall ──▶ Run（Worker）
                                └  跟随日志（feed）：Run 的进展、Call 被取代、Session 关闭
```

- **经网关中转。** 设备沿已有的 Connection（ADR-0024）发送音频。网关进程打开实时语音模型的会话并转发。
  - 厂商密钥只在服务端。
  - 转写、计量、派生 Run 都在服务端完成，与 HTTP、文字输入走同一条 `service` 路径。
  - 代价是多一跳：同机房几十毫秒。
- **Call 的状态只在中转进程中。**
  - 进程退出时 Call 随之结束：日志中留下没有 `CallEnded` 的 Call，下一个 Call 开始时以 `replaced` 补上。
  - 连接断开即挂断（`disconnected`），设备重新发起即可。不跨连接续接。
  - 接通之前设备挂断（或放弃等待）：取消开始，不连接模型、不写 `CallStarted`，设备收到 `ended`（`hangup`）。开始恰好已经成功时，接通后立即挂断。
  - 中转进程从写入 `CallStarted` 之后的投影跟随日志（`service.CallStart.State`），不重新加载：重新加载可能已跨过此后的取代或关闭，永远看不到自己的结束。
- **一个 Session 同一时刻至多一个 Call，一条连接同一时刻至多一个 Call。** 另一台设备在同一 Session 开始 Call 时，旧的以 `replaced` 结束。

## 3. 实时语音模型（`internal/realtime`）

`realtime.Provider` 是与厂商无关的接口（G9）：

| 方法 / 事件 | 含义 |
|---|---|
| `Audio(pcm)` | 一帧上行音频：PCM 16 位、单声道、16 kHz、20 ms（640 字节） |
| `Mute(bool)` | 麦克风关闭 / 打开（关闭期间不发音频，模型不会超时） |
| `CancelResponse()` | 中止进行中的回复（界面上的打断） |
| `ToolResult(id, text)` | 回传函数调用结果 |
| `Say(text)` | 原样说出（后台任务结果的播报） |
| `AddContext(turns)` | 补充上下文，不触发回复 |
| 事件 | `SpeechStarted`（用户开口，打断播放）、`UserTranscript`、`AssistantText`、`AssistantAudio`（PCM 16 位、24 kHz）、`ResponseDone`、`ToolCall`、`UsageReport`、`Failed` |

适配器：

- `realtime/volc`：豆包实时语音模型 3.0（Seeduplex，全双工版）。JSON 事件协议，鉴权头为 `X-Api-Key`。
- `realtime/fake`：脚本化的实现，供测试使用。

模型引用写作 `volc/1.2.6.1`，`serve` 在设置了 `VOLC_SPEECH_API_KEY` 时注册 `volc`。

**实测（2026-10-11）**

| 项 | 结果 |
|---|---|
| 识别结束到首包语音 | 约 0.27 秒（无工具调用） |
| 识别结束到函数调用 | 约 0.4 秒 |
| 工具结果回传到首包语音 | 约 1.4 秒 |
| 建立会话（连接 + `session.create`） | 约 1.7 秒 |
| 输出音频 | 约 25 token/秒；按刊例价约 0.45 元/分钟助手语音 |
| 内置文本输入 | 每个会话的第一轮约 5.3K token，之后多数命中缓存（缓存价 5 元/百万） |

协议上的差异：

- 上行须严格按实时节奏发送（过快过慢都会报错）。
- 函数调用挂起期间写入的上下文会被忽略。
- 回传结果可以延迟：实测挂起 30 秒仍正常回复。
- 被中止的回复只有 `response.canceled`。
- 下行音频不带 `response_id`。

## 4. 中转（`internal/call`）

- **节拍器。** 设备的音频进入缓冲区，按 20 ms 一帧转给模型，没有音频时发静音（模型依赖连续的音频流）。网络抖动造成的积压上限是 300 ms，超出时丢弃最旧的（`call_audio_dropped_frames_total`），对话延迟因此有界。
- **转写。** 双方每句话（用户的整句、助手的整句）按顺序写入日志（`CallTranscript`），写入在单独的协程中进行，不阻塞音频转发。
  - 助手的整句在语音播完之前就已生成（文本比语音快得多），此时检查并写入。
  - 用户开口时，尚未生成完的回复按"被打断"（`interrupted`）写入已生成的部分，写在用户这句话之前。
  - 结束时立即停止中转，但已收下的转写写完（至多 5 秒）再写 `CallEnded`：用户说完立即挂断时，最后一句话不丢，且排在结束之前。
- **进入上下文。** `runtime.Transcript` 把转写呈现为用户消息：`[语音通话] 用户：…` 或 `[语音通话] 语音助手：…`，调用尚无结果时顺延到结果之后。之后的 Run 与文字对话都能看到通话内容。
- **开始时的上下文。** 新 Call 以 Session 的近期对话（用户输入、助手回复、之前通话的话，至多 20 句、每句 300 字）初始化语音模型的上下文，压缩过的以摘要开头（`call.RecentTurns`）。

## 5. 通话中做事：run_task

语音模型只有一个工具 `run_task(task)`：把需要能力的工作交给 Session 的 AgentDef 作为 Run 执行。设备 Node、沙箱、Memory、审批、挂起等都走现有路径，不在 Call 里重做一遍。

- **提交。** `service.SubmitFromCall` 与文字输入的语义相同：
  - 有活跃 Run 时并入（插话）；
  - Run 正在等用户回答提问时，任务内容就是回答（ADR-0025）；
  - 输入带 `from_call` 与输入 ID（`<call>/<工具调用 ID>`，重试去重）。
  - Call 已不在进行中时拒绝。
- **朗读提示。** Call 派生的输入在上下文中附上 `model.FromCallNote`："你的最终回复会被朗读：用一两句口语说出结果，不要用 Markdown、列表、表格或链接"。
- **结果回注。** 中转进程跟随日志，每个 Run 只有一个跟随者：
  - 挂起期间（`HoldFor`，默认 20 秒）Run 完成、失败，或开始等待审批、提问时，把它作为工具结果交还模型，由模型自然地说出。
  - 超时则先回答"正在后台进行，完成后会告诉用户"。之后的进展在通话空闲（没有进行中的回复与挂起的工具调用）时以 `Say` 播报，并补进模型上下文。
  - 同一 Run 后来的任务（插话、回答）接管挂起。
- **审批与提问。**
  - 审批：模型告诉用户"需要在屏幕上确认"。语音审批暂不支持：语音模型的转述不能作为用户的批准。
  - 提问：模型把问题问出来，用户口头回答后，模型用 run_task 转达，成为打字回答（ADR-0025）。回答带 `from_call`，标明它是转述（§6）。
- **工具说明与提示词是给模型看的。** `call.RunTaskTool`、结果文字、`agents/*.yaml` 的 `call.instructions` 改动时运行 `make eval`（§9）。

## 6. 内容安全与 Memory（ADR-0021、ADR-0023）

- **转写写日志之前检查。** 用户的话按输入、助手的话按输出检查（`service.AppendTranscript`）。违规或无法检查时：
  - 不写日志；
  - 中止进行中的回复，通知设备停止播放（`CallSpeech`）；
  - 用户的话违规时说出拒答。
- **播出的语音依赖厂商审核。** 语音边生成边播放，无法"先审后播"：
  - 会话开启 Seeduplex 的严格审核（`strict_audit`）；
  - 我们的检查在助手整句生成时（通常早于播完）进行，违规时还来得及中止剩余的播放。
  - 这与 ADR-0021"写日志之前检查"的差异写在 ADR-0027 与上线检查清单中。
- **Memory 写入闸门。**
  - 用户在通话中说的话（转写）是用户本人的话。
  - Call 派生的任务与转述的回答（`ToolResult.from_call`）是语音模型的转述，不是原话，不计入"用户本人的话"（`runtime.userText`）。
  - 转述本身也使闸门生效（`runtime.tainted`）：即使上下文中没有外部内容，只出现在转述里的内容要写入时也需要批准。

## 7. 计量与配额

- 每轮交互的用量（文本 / 音频输入、缓存、文本 / 音频输出）以 `usage.Call` 记录（kind = `call`，ID 为 `call_<CallStarted 的事件 ID>/<轮次>`）。不用客户端生成的 Call ID：复用它会使用量因计量记录去重而丢失。价格表的模型条目可配 `input_audio`、`output_audio` 单价（`pricing.yaml` 中的 `volc/1.2.6.1` 为刊例价）。
- 开始 Call 时检查配额，用尽时拒绝（`quota_exceeded`）；通话中每轮之后复查，用尽时挂断（`quota`）。

## 8. 客户端

**Connection 上的消息**（`node.proto`）：

| 方向 | 消息 |
|---|---|
| 设备 → 网关 | `CallStart{call_id, session_id}`、`CallAudio{call_id, pcm}`、`CallControl{call_id, action: mute / unmute / interrupt / hangup}` |
| 网关 → 设备 | `CallEvent{call_id, ready / audio / speech / text / response_done / task / ended / error}` |

- `ready` 之前的音频丢弃。
- `speech` 表示用户开口：立即停止播放，并丢弃该回复迟到的音频。
- 其他设备不收 `CallEvent`，它们经订阅看到日志中的 `CallStarted`、`CallTranscript`、`CallEnded`。

**网页 SDK**：`client.startCall(sessionId, handler)` 在接通后返回 `Call`（`sendAudio`、`mute`、`interrupt`、`hangup`）。handler 的 `onAudio` 收到 `Int16Array`，连接断开时 `onEnded("disconnected")`。

**网页通话页**：`serve` 提供 `/call`（`internal/webui`），用于开发与演示。

- 麦克风经浏览器的回声消除与降噪，AudioWorklet 重采样为 16 kHz 后按 20 ms 分帧；播放按到达顺序排队，打断时清空。
- 页面同时显示对话（含通话转写、后台任务、审批与提问卡片），可打字。
- 浏览器只在 localhost 或 HTTPS 下允许使用麦克风。外放时回声消除不一定完全有效，建议戴耳机。
- 页面使用的 SDK 打包产物 `internal/webui/static/yanshi.js` 随代码提交，`make check` 检查它与源码一致。

## 9. 测试与评测

- `session`：同一时刻至多一个 Call；转写与派生输入只属于进行中的 Call；关闭前须结束 Call；快照覆盖进行中的 Call。
- `service`：开始、取代、转写（内容安全）、派生任务（去重）、关闭时结束。
- `runtime`：转写进入上下文（调用尚无结果时顺延），派生任务附朗读提示；Memory 闸门只把用户的话算作用户本人的话。
- 模拟测试：中转进程的写入与 Run、关闭、删除、故障交错。
  - 被取代、关闭或删除之后的写入须被拒绝，日志始终合法。
  - 被拦截的转写不进日志。
  - 覆盖统计：`CallsStarted`、`CallsReplaced`、`CallTranscripts`、`CallTranscriptsBlocked`、`CallTasks`、`CallAnswers`（任务恰好回答了 Run 的提问）、`CallConflicts`。
- e2e（`internal/e2e/call_test.go`，脚本化语音模型）：
  - 中转与日志顺序；
  - 短任务在挂起期间完成；
  - 用户开口打断长回复，记为被打断；
  - 长任务先回答"在后台进行"、结束后播报；
  - 审批后播报；
  - 被另一台设备取代；
  - 关闭 Session、断开连接时结束；
  - 后续 Call 以通话内容为上下文。
- 网页 SDK：单元测试，以及 `TestWebSDK` 中经网关的通话（`make check`）。
- 实测（不在 `make check` 中，需要 `VOLC_SPEECH_API_KEY`）：
  - `YANSHI_TEST_VOLC=1 go test ./internal/realtime/volc`：识别、函数调用、语音回复、用量、语音合成。
  - 网页通话页以 headless Chrome 对真实模型跑通（以 WAV 代替麦克风）。
- **评测**（`evals/call-*.yaml`，`call: true`）：每轮的 input 经豆包语音合成为用户语音，按实时节奏送入真实的 Call，检查派生任务（`tasks`、`task_contains`）、调用、Memory、语音回复。
  - `call-chitchat`：闲聊与常识不派生任务。
  - `call-remember-allergy`：让助手记住健康信息时交给后台保存，不口头答应。
  - `call-time`：问时间交给后台（clock_now）。
  - `call-scenario-b`：读电脑上的文件并口语转述，改写标题经审批后写回。
  - 首版指令下，语音模型被要求"记住"时只口头答应、不派生任务（0/1），补充"你自己记不住通话之外的事"后 3/3。

## 10. 未做

- Go SDK 与移动端绑定的通话接口（下一步）。
- 通话中的语音审批。
- Opus 编码与弱网下的抖动缓冲（目前 PCM 经 WebSocket，约 256 kbps 上行 + 384 kbps 下行）。
- 跨连接续接。
- 视频通话。
- 其他厂商的适配器（阿里 Qwen-Omni 实时、私有化的 Qwen3-Omni 等，见调研笔记）。
