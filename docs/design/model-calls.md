# 模型调用的时限、重试与准入

> 状态：已实现（2026-10-28 更新四类准入与最低份额）· 来源：[延展性评审](../review/2026-10-11-scalability-review.md) §4.2、§8.4 第 5 项 · 关联：ADR-0029

## 1. 要解决的问题

- **无时限。** 原来的模型客户端使用 `http.DefaultClient`：建连、等待响应头、读取流式输出都没有时限。上游卡住时，Worker 一直持有租约并按时续约，Run 停在 running。`StepErrorBudget` 只在 Step 返回错误时计时，管不到这种情况。
- **无退避。** 限流（429）和服务端错误（5xx）立即失败，Worker 在下一步（约 100ms 后）马上重试。连续 3 次失败后，Run 失败。配额触顶时所有 Worker 一齐重试，拥塞更重。
- **无准入。** 没有按提供商的并发上限与速率上限。规模上去之后，模型配额是首要的故障来源。

## 2. 做法

每个真实提供商及压测用的 `bench` 提供商在网关中由 `model.Guard` 包装，各自计数，使用同一组 `serve -model-*` 参数；`echo`、`script` 保持原路径：

| 约束 | 默认 | 配置 |
|---|---|---|
| 一次调用的时限（不含排队与重试等待） | 10 分钟；超时返回 `model.ErrTimeout`，不重试 | `serve -model-timeout` |
| 总时限：准入排队、每次尝试、重试前的等待都计入 | 15 分钟；到期返回 `model.ErrTimeout`；剩余时间等不到下一次重试时立即放弃 | `serve -model-deadline` |
| 流停滞：两次收到数据之间的最长间隔 | 2 分钟；返回 `model.ErrStalled` | `openaicompat.Provider.StallTimeout` |
| 建连、TLS 握手、等待响应头 | 10 秒、10 秒、2 分钟 | `openaicompat.DefaultClient` |
| 暂时性失败的重试 | 2 次，指数退避 1 秒起、最长 30 秒，带抖动；提供商给出更长的 `Retry-After` 时以它为准，不受 30 秒截断（提前重试只会再被限流一次） | `serve -model-retries` |
| 并发上限 | 不限；受限时使用四类优先级与后台 Run 最低份额 | `serve -model-concurrency` |
| 后台 Run 最低在途份额 | 并发上限的 1/8 向上取整（ceil）；不限并发时为 0。须非负且在并发受限时严格小于上限；并发为 1 时须显式设为 0 | `serve -model-min-background`、`model.Limits.MinBackground` |
| 发起速率 | 不限（令牌桶，突发量为一秒的量） | `serve -model-rps` |

- **可以重试的失败**（`model.Retryable`）：HTTP 408、409、425、429、500、502、503、504、529；流中途报告的错误；连接中断与流停滞。
- **不重试**：上下文超长、参数错误、鉴权失败、超时、取消。
- **已输出过增量后不重试**：订阅者已经看到了部分输出，重试会让它们看到两份。这种失败交给 Worker：本步返回错误，下一步重新调用，连续失败计入 `MaxModelErrors`。
- **准入排队按优先级。** 四个队列依次为交互 Run、交互单次调用、后台 Run、后台单次调用，各自先来先得。长任务（ADR-0029）的 Step 标记 `model.WithBackground`；单次调用标记 `model.WithOneshot`，两者组合即后台单次调用。每次释放先扣除离开者的在途数：若后台 Run 在途数小于最低份额且有后台 Run 在等，先交给它；否则按四类顺序交付。后台单次调用不计入最低份额。有空名额且没有任何等待者时直接进入。最低份额只是释放时回填，不抢占、不给后台设上限：空闲时后台仍可占满全部名额。每次重试重新准入，退避期间不占提供商名额。
- **单次调用的并发上限工具。** `model.Slots` 是进程内按键的信号量，零值可用；`model.Slots.Acquire` 接收 ctx、键、限额、最长等待时间，成功返回幂等的释放函数。同键先来先得，限额不大于 0 表示不限，等待时间不大于 0 表示只尝试立即获取；等待超时返回 `model.ErrBusy`，ctx 结束返回其错误。同一键仍有持有者或等待者时，正限额须一致，否则返回配置错误；键空闲即清除。`model.Slots.AcquireAll` 按 `model.SlotReq` 列表顺序获取，部分失败逆序释放全部已持有名额，重复的受限键会被拒绝。每层等待独立计时，总时限由 ctx 控制。`oneshot.Service` 按合计、业务线、声明名的固定顺序构造互不冲突的键，同名各版本共用键，持有到逻辑调用结束（含退避），将 `model.ErrBusy` 映射为 429。服务通过 `app.Options.Oneshots` 与 `app.NewCore` 装配，合计上限默认 8、每层等待默认 2 秒、整个逻辑调用时限默认 60 秒（从声明校验之前开始，覆盖两次模型调用）。业务线配置 oneshots.max_concurrency 为 0 时不再施加该层上限，负数拒绝加载。serve 只有加载到单次调用声明时才校验合计上限必须为正，受限的提供商并发必须严格大于合计上限；声明各版本的并发必须相同且为正。无可用名额返回 429，总时限到期返回 504。
- **连接复用。** `DefaultClient` 每个主机保留最多 256 个空闲连接。`http.DefaultTransport` 只保留 2 个，并发的流式调用多了，每次都要重新握手。
- 时限、退避与准入只影响调用的节奏和失败方式，不改变执行语义。模拟测试使用脚本化模型，不经过 `Guard`。

## 3. 指标

| 指标 | 说明 |
|---|---|
| `yanshi_model_retries_total{provider}` | 因暂时性失败重试的次数 |
| `yanshi_model_admission_wait_seconds{provider,class}` | 成功准入的每次尝试在速率与并发上限处排队的时间；class 为 interactive、oneshot、background、background_oneshot，只记类别，不记业务线与声明名 |
| `yanshi_model_deadlines_total{provider}` | 总时限内没有结果的调用（排队过久或重试耗尽时间） |

## 4. 提供商：火山方舟（2026-10-11 起默认）

对话与嵌入默认使用火山方舟（`ark`，`ARK_API_KEY`；coding plan 的 `ARK_BASE_URL` 为 `…/api/coding/v3`）。以下行为都用真实请求确认过：

- **思考强度写在模型引用的后缀上**：`ark/doubao-seed-2-1-lite-260915:minimal`。后缀取 `minimal | low | medium | high`，以 `reasoning_effort` 发送（`openaicompat.SplitEffort`）；其他带冒号的模型 ID 原样发送。方舟不接受 `thinking: auto`。默认会先思考，首字延迟明显变长。实测（2026-10-12，同一问题）：`low` 先思考约 27 秒才出正文，思考中途最长停顿 5.7 秒；`minimal` 首字约 0.7 秒。各 Agent 取 `minimal`，以响应速度为先。代价是 `doubao-seed-2-1-lite` 在 `minimal` 下不总按指令用 `show_card` 展示结果（`low` 与 `pro:minimal` 都能），留待其他手段改进。价格表按不带后缀的模型计价。
- **图片与音频**：用户消息中的图片以 `image_url`（data URL）发送，语音消息以 `input_audio` 发送（格式 wav、mp3、m4a；不支持 webm、ogg）。控制台的按住说话不用它，而是先识别成文字（[按住说话](./speech.md)）。二者都由 Worker 从工件内联（单个不超过 4 MB）；语音消息的内容安全检查须覆盖音频本身（`moderation.Request.Audio`）。
- **多模态嵌入**（`doubao-embedding-vision-*`）只提供 `/embeddings/multimodal`：一次请求的全部输入合成一个向量，所以每段文本单独请求（并发 4）。2048 维。Memory 目前只嵌入文本。
- 流式输出中的思考过程（`reasoning_content`）以单独的增量推送给订阅者，随回复写入 `AssistantMessage.reasoning`（ADR-0034）。它不发回给模型。思考的 token 计入输出用量，另记在 `Usage.reasoning_tokens` 中。

## 5. 遗留

- 上限目前对每个提供商取同一组值。提供商之间的容量差别大时（如 TokenHub 与私有化 vLLM），改为按提供商配置。
- RPS 令牌桶仍由四类调用共享，取令牌在并发准入之前：RPS 成为瓶颈时，低优先级调用可能先取走令牌。优先级与最低份额只保证并发名额的交付，验收结论要求 RPS 有余量。
- 准入与单次调用的按键上限仍是单进程的。多进程须按持有提供商的进程数静态分摊账号并发与 RPS，并分摊单次调用上限；扩缩容时联动配置，不能把每个进程的最低份额当成全局保证。共享令牌服务尚未实现。
