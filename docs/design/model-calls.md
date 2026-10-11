# 模型调用的时限、重试与准入

> 状态：已实现（2026-10-11）· 来源：[延展性评审](../review/2026-10-11-scalability-review.md) §4.2、§8.4 第 5 项 · 关联：ADR-0029

## 1. 要解决的问题

- **无时限。** 原来的模型客户端使用 `http.DefaultClient`：建连、等待响应头、读取流式输出都没有时限。上游卡住时，Worker 一直持有租约并按时续约，Run 停在 running。`StepErrorBudget` 只在 Step 返回错误时计时，管不到这种情况。
- **无退避。** 限流（429）和服务端错误（5xx）立即失败，Worker 在下一步（约 100ms 后）马上重试。连续 3 次失败后，Run 失败。配额触顶时所有 Worker 一齐重试，拥塞更重。
- **无准入。** 没有按提供商的并发上限与速率上限。规模上去之后，模型配额是首要的故障来源。

## 2. 做法

每个真实提供商在网关中由 `model.Guard` 包装，各自计数：

| 约束 | 默认 | 配置 |
|---|---|---|
| 一次调用的时限（不含排队与重试等待） | 10 分钟；超时返回 `model.ErrTimeout`，不重试 | `serve -model-timeout` |
| 总时限：准入排队、每次尝试、重试前的等待都计入 | 15 分钟；到期返回 `model.ErrTimeout`；剩余时间等不到下一次重试时立即放弃 | `serve -model-deadline` |
| 流停滞：两次收到数据之间的最长间隔 | 2 分钟；返回 `model.ErrStalled` | `openaicompat.Provider.StallTimeout` |
| 建连、TLS 握手、等待响应头 | 10 秒、10 秒、2 分钟 | `openaicompat.DefaultClient` |
| 暂时性失败的重试 | 2 次，指数退避 1 秒起、最长 30 秒，带抖动；提供商给出更长的 `Retry-After` 时以它为准，不受 30 秒截断（提前重试只会再被限流一次） | `serve -model-retries` |
| 并发上限 | 不限；排队时交互调用优先 | `serve -model-concurrency` |
| 发起速率 | 不限（令牌桶，突发量为一秒的量） | `serve -model-rps` |

- **可以重试的失败**（`model.Retryable`）：HTTP 408、409、425、429、500、502、503、504、529；流中途报告的错误；连接中断与流停滞。
- **不重试**：上下文超长、参数错误、鉴权失败、超时、取消。
- **已输出过增量后不重试**：订阅者已经看到了部分输出，重试会让它们看到两份。这种失败交给 Worker：本步返回错误，下一步重新调用，连续失败计入 `MaxModelErrors`。
- **准入排队按优先级。** 长任务（ADR-0029）的 Step 在 ctx 中标记 `model.WithBackground`。名额释放时先交给等待中的交互调用，同一优先级先来先得。
- **连接复用。** `DefaultClient` 每个主机保留最多 256 个空闲连接。`http.DefaultTransport` 只保留 2 个，并发的流式调用多了，每次都要重新握手。
- 时限、退避与准入只影响调用的节奏和失败方式，不改变执行语义。模拟测试使用脚本化模型，不经过 `Guard`。

## 3. 指标

| 指标 | 说明 |
|---|---|
| `yanshi_model_retries_total{provider}` | 因暂时性失败重试的次数 |
| `yanshi_model_admission_wait_seconds{provider}` | 在速率与并发上限处排队的时间 |
| `yanshi_model_deadlines_total{provider}` | 总时限内没有结果的调用（排队过久或重试耗尽时间） |

## 4. 提供商：火山方舟（2026-10-11 起默认）

对话与嵌入默认使用火山方舟（`ark`，`ARK_API_KEY`；coding plan 的 `ARK_BASE_URL` 为 `…/api/coding/v3`）。以下行为都用真实请求确认过：

- **思考强度写在模型引用的后缀上**：`ark/doubao-seed-2-1-lite-260915:minimal`。后缀取 `minimal | low | medium | high`，以 `reasoning_effort` 发送（`openaicompat.SplitEffort`）；其他带冒号的模型 ID 原样发送。方舟不接受 `thinking: auto`。默认会先思考，首字延迟明显变长。实测（2026-10-12，同一问题）：`low` 先思考约 27 秒才出正文，思考中途最长停顿 5.7 秒；`minimal` 首字约 0.7 秒。各 Agent 取 `minimal`，以响应速度为先。代价是 `doubao-seed-2-1-lite` 在 `minimal` 下不总按指令用 `show_card` 展示结果（`low` 与 `pro:minimal` 都能），留待其他手段改进。价格表按不带后缀的模型计价。
- **图片与音频**：用户消息中的图片以 `image_url`（data URL）发送，语音消息以 `input_audio` 发送（格式 wav、mp3、m4a；不支持 webm、ogg，所以控制台录音在浏览器中编码为 16kHz WAV）。二者都由 Worker 从工件内联（单个不超过 4 MB）；语音消息的内容安全检查须覆盖音频本身（`moderation.Request.Audio`）。
- **多模态嵌入**（`doubao-embedding-vision-*`）只提供 `/embeddings/multimodal`：一次请求的全部输入合成一个向量，所以每段文本单独请求（并发 4）。2048 维。Memory 目前只嵌入文本。
- 流式输出中的思考过程（`reasoning_content`）以单独的增量推送给订阅者，随回复写入 `AssistantMessage.reasoning`（ADR-0034）。它不发回给模型。思考的 token 计入输出用量，另记在 `Usage.reasoning_tokens` 中。

## 5. 遗留

- 上限目前对每个提供商取同一组值。提供商之间的容量差别大时（如 TokenHub 与私有化 vLLM），改为按提供商配置。
- 准入是单进程的。多个进程的总并发等于进程数乘以上限；要做全局准入，需要一个共享的令牌服务。等确定部署形态时一起设计。
