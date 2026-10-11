# 调研：实时语音大模型厂商（价格、效果、连通性）

> 状态：调研笔记（2026-10-10），不是设计 · 关联：章程 G2/G6、北极星场景 B、ADR-0001（私有化部署）、ADR-0005（Call 与 Run 分离）
>
> 价格变动频繁，许多数字来自第三方聚合站，签约前须以各家控制台为准。美元按 1 USD ≈ 7.1 CNY 粗算。

## 0. 结论

1. **效果**：唯一有横向可比数据的是 Artificial Analysis（AA）的 Speech-to-Speech 榜（英文评测）。国外头部是 OpenAI GPT-Live-1 / GPT-Realtime-2.1、Gemini 3.8 Live、Grok Voice Think Fast 2.0；国内上榜的只有**阿里 Qwen** 与**阶跃 StepAudio**，推理分（Big Bench Audio）都在第一梯队。**豆包、智谱 GLM、腾讯、MiniMax 都没有上榜**，没有第三方可比数据，只能自测。
2. **价格**：按"每分钟助手说话"的输出成本，从低到高大致是：Qwen 实时系列、Gemini Live、Nova 2 Sonic ≈ 0.1 元左右；GLM-Realtime 0.18–0.3 元（不分输入输出，按通话时长）；Grok 0.35–0.57 元；OpenAI gpt-realtime 约 0.55 元。豆包的单价是按 token 公布的（音频输入 80 元 / 输出 300 元每百万 token），但没查到音频 token 与时长的换算，**每分钟成本需实测**。
3. **连通性**（开发机经本地代理实测，见 §3）：2026-10-10 白天百炼北京站、阶跃、腾讯流式 TTS 在 TLS 握手阶段被断开（与此前方舟相同）；**同日晚间复测，包括方舟在内的全部端点均可达**，证实是本地网络/代理问题而非厂商问题。
4. **对 yanshi 的约束比价格更关键**：
   - **国内 C 端**：OpenAI、Google、xAI、Anthropic 等不向中国内地提供服务（OpenAI 自 2024-07 起封禁不支持地区的 API 流量），且面向境内公众的生成式 AI 服务需要使用已备案的模型——国外模型实际上只能用于海外业务。
   - **私有化部署（ADR-0001）**：所有云 API 都不满足"无外网运行"。私有化场景只能用开源权重：Qwen3-Omni（Apache-2.0）、Step-Audio 2 mini（Apache-2.0）、MiniCPM-o 4.5（9B，支持全双工；许可证待核实）等。
5. **建议**：国内候选按 **Qwen（实时系列）→ 豆包 → 阶跃 → 智谱**的顺序做中文场景 B 的自测；私有化以 Qwen3-Omni 为首选基线。在 Call 层定义与厂商无关的实时模型接口（类似现有 `model.Provider`），用评测决定准入（章程 G9）。

## 1. 厂商一览

### 1.1 国内

| 厂商 / 模型 | 计费 | 折合每分钟（粗算） | 效果（AA 榜） | 备注 |
|---|---|---|---|---|
| **阿里 百炼**：qwen3.5-omni-flash-realtime、qwen3.5-omni-plus-realtime、qwen-audio-3.0-realtime-flash/plus、qwen3.8-omni-flash-realtime | 按 token。官方文档：qwen3.5-omni-flash-realtime 音频输入 27 元/百万 token（北京），**每秒音频 7 token**；Qwen Audio 3.0 第三方给出每秒 12.5 token | AA 标价：Qwen Audio 3.0 Plus 输入 $0.03/h、输出 $0.18/h；Qwen3.5 Omni Plus/Flash Realtime 输入 $0.16/h、输出 $0.82/h → **输出约 0.02–0.1 元/分钟** | Qwen Audio 3.0 Realtime Plus：STS 指数 69.5，BBA 99%，τ-Voice 54.6%，首音 1.54 s；Qwen3.5 Omni Flash Realtime：BBA 59%，首音 **0.79 s** | 国内效果数据最完整、价格最低；支持音频+图片/视频输入；多轮时历史音频会重复计入输入 token。注意：各来源的价格差异很大，以控制台为准 |
| **字节 火山引擎**：豆包端到端实时语音大模型 | 按 token，仅后付费：文本输入 10、音频输入 80、缓存输入 5、文本输出 80、**音频输出 300** 元/百万 token；赠 100 万 token 资源包（半年） | 未查到 token/秒换算，**需实测** | 未上榜 | 章程原选火山引擎。豆包 App 中的新一代全双工模型 SeedRealtime（2026-08 上线 App）据报道**尚未开放 API** |
| **阶跃星辰**：stepaudio-2.5-realtime、stepaudio-3-realtime-preview、Step-Audio R1.1 | stepaudio-2.5-realtime：输入 10（缓存 2）、输出 70 元/百万 token；3-realtime-preview 限时免费 | AA：Step-Audio R1.1 输入 $0.06/h、输出 $1.69/h → 输出约 0.2 元/分钟 | StepAudio 3 Realtime：BBA **99.7%（榜首）**，但首音 **8.83 s**；Step-Audio R1.1：BBA 98%，首音 1.53 s | 推理强、延迟待改善；开源了 Step-Audio 2 mini（Apache-2.0） |
| **智谱**：GLM-Realtime-Flash（约 9B）/ Air（约 32B） | **按时长**：Flash 音频 0.18 元/分钟、视频 1.2 元/分钟；Air 音频 0.3 元/分钟、视频 2.1 元/分钟 | 同左 | 未上榜 | 支持视频通话、打断、服务端/客户端 VAD、Function Calling（仅语音）。限制明显：**通话记忆约 2 分钟**、音频上下文 8K（约 20 轮）、单次音频最长 30 秒、并发 5–20 |
| **腾讯**（TokenHub / 混元） | — | — | 未上榜 | 未找到端到端实时语音模型的公开 API；TokenHub 目前只有文本类模型。腾讯云有"实时语音识别-大模型版"（ASR，$1.1–2/小时），只能用于级联方案 |
| **MiniMax** | — | — | 未上榜 | 本次未查到端到端实时语音 API（以 TTS 为主） |

### 1.2 国外（仅适用于海外业务）

| 厂商 / 模型 | 计费 | 折合每分钟 | 效果（AA 榜） | 备注 |
|---|---|---|---|---|
| **OpenAI** gpt-realtime-2.1 / mini、GPT-Live-1 | gpt-realtime-2.1：音频输入 $32、缓存 $0.40、输出 $64 每百万 token；mini：$10 / $0.30 / $20 | 输出约 $0.077/分钟（≈0.55 元），mini 约 $0.024 | GPT-Live-1（Astra）：**STS 指数 83.2（最高）**，τ-Voice 74.5%；GPT-Realtime-2.1 High：75.2，首音 1.21 s | Agent 能力（τ-Voice）最强；最贵 |
| **Google** Gemini 3.8 Live、3.1 Flash Live | 音频输入 $3、输出 $12 每百万 token（每秒 25 token） | 输入约 $0.005、输出约 $0.018/分钟（≈0.13 元） | Gemini 3.8 Live Extended Thinking：STS 82.6，BBA 98%，τ-Voice 68.6%；Gemini 3.8 Live：76.0，首音 1.18 s | 性价比最高的国外选项 |
| **xAI** Grok Voice Think Fast 2.0 / 1.0 | **按时长**：2.0 为 $0.08/分钟（2026-08-05 起 `grok-voice-latest` 指向它）；固定 1.0 仍为 $0.05/分钟 | 0.35–0.57 元 | Think Fast 2.0 High：STS 81.3，BBA 97%，**首音 0.70 s** | 计费最好预测，延迟低 |
| **AWS** Nova 2 Sonic | 按 token | AA：输入 $0.27/h、输出 $1.08/h → 输出约 $0.018/分钟 | BBA 88%，首音 1.14 s | Nova Sonic v1 于 2026-09-14 停服 |

> AA 的 Speech-to-Speech 指数 2.0 = 语音推理（Big Bench Audio）+ Agent 能力（τ-Voice）+ 竞技场偏好 + 任务成功率，各 25%。评测以**英文**为主，对中文场景 B（口语、打断、方言、噪声厨房环境）只有参考价值。

## 2. 效果对比（AA 榜，截至调研日）

| 模型 | STS 指数 | Big Bench Audio | τ-Voice | 首音延迟 |
|---|---|---|---|---|
| OpenAI GPT-Live-1 (Astra, medium) | **83.2** | 90% | **74.5%** | 1.34 s |
| Gemini 3.8 Live Extended Thinking (High) | 82.6 | 98% | 68.6% | 1.35 s |
| Grok Voice Think Fast 2.0 High | 81.3 | 97% | 56.5% | **0.70 s** |
| Gemini 3.8 Live | 76.0 | 92% | 30.1% | 1.18 s |
| OpenAI GPT-Realtime-2.1 High | 75.2 | 96% | 45.7% | 1.21 s |
| **Qwen Audio 3.0 Realtime Plus** | 69.5 | 99% | 54.6% | 1.54 s |
| **Qwen Audio 3.0 Realtime Flash** | 67.7 | 96% | 35.9% | 1.55 s |
| **StepAudio 3 Realtime** | — | **99.7%** | — | 8.83 s |
| **Step-Audio R1.1 (Realtime)** | — | 98% | — | 1.53 s |
| **Qwen3.5 Omni Plus Realtime** | — | 99% | — | 2.64 s |
| Amazon Nova 2.0 Sonic | — | 88% | — | 1.14 s |
| **Qwen3.5 Omni Flash Realtime** | — | 59% | — | 0.79 s |
| Kyutai Moshi（开源） | — | 4% | — | — |

要点：

- **推理能力（BBA）已不是区分点**：国内的 Qwen、阶跃与国外头部同在 96–99%。
- **差距在 Agent 能力（τ-Voice）与延迟**：τ-Voice 衡量通话中调用工具完成任务的能力，正是场景 B"通话中派生 Run 操作电脑"所需的能力。国内只有 Qwen Audio 3.0 Plus 有该项数据（54.6%），与 Grok 2.0 相当，低于 OpenAI GPT-Live-1 与 Gemini。
- **延迟与能力的权衡**：同一家的快速档（Qwen3.5 Omni Flash Realtime 0.79 s）推理分明显更低；推理强的档位首音普遍超过 1.5 s。
- 豆包、GLM 缺少可比数据，不代表效果差——需要我们自己的中文评测。

## 3. 连通性实测

2026-10-10 在开发机（macOS，经本地代理的 fake-ip TUN，解析地址均为 198.18.x.x）上测试：不带凭证发起 HTTPS 与 WebSocket 升级请求，能拿到 101/400/401 即说明网络可达。各测两轮，结果一致。

| 厂商 | 端点 | 结果 | 结论 |
|---|---|---|---|
| OpenAI | `wss://api.openai.com/v1/realtime` | 101 升级成功，TLS 约 1.1 s | 可达（经代理） |
| Google | `wss://generativelanguage.googleapis.com/ws/…BidiGenerateContent` | 101 | 可达（经代理） |
| xAI | `wss://api.x.ai/v1/realtime` | 401 | 可达 |
| AWS | `bedrock-runtime.us-east-1.amazonaws.com` | 404（HTTPS） | 可达 |
| 阿里 百炼（北京） | `wss://dashscope.aliyuncs.com/api-ws/v1/realtime` | 白天：超时，TLS 未完成（真实 IP 39.96.198.249 同样失败）；**晚间复测：401** | **可达** |
| 阿里 百炼（新加坡国际站） | `wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime` | 401 | 可达 |
| 字节 豆包 | `wss://openspeech.bytedance.com/api/v3/realtime/dialogue` | 400 | 可达 |
| 智谱 | `wss://open.bigmodel.cn/api/paas/v4/realtime` | 401 | 可达 |
| 阶跃星辰 | `wss://api.stepfun.com/v1/realtime`，以及官网 `platform.stepfun.com` | 白天：TLS Client Hello 后被断开（真实 IP 14.103.2.83 相同）；**晚间复测：API 401、官网 200** | **可达** |
| 腾讯 TokenHub | `tokenhub.tencentmaas.com` | 404（HTTPS） | 可达 |

解读：

- 白天百炼北京与阶跃的症状（TCP 被本地 TUN 立即接受、发出 Client Hello 后被断开）与此前方舟一致；**2026-10-10 23:00 复测（两轮）全部可达，方舟 `ark.cn-beijing.volces.com` 也返回 401**，确认是本地代理/出口问题，已解决。Gemini 第二轮出现一次 TLS 失败，随后连续 6 次均正常，视为偶发。
- 国外端点"可达"是经过本地代理实现的；部署在境内 IDC 的服务端通常无法直连，而且 OpenAI 等明确不向中国内地提供服务。
- 这里只验证了网络层；带真实凭证的鉴权、音频首包延迟需要拿到各家 Key 之后再测。

## 4. 开源、可私有化部署的模型

| 模型 | 规模 / 许可 | 说明 |
|---|---|---|
| **Qwen3-Omni**（30B-A3B，含 Thinking 版） | Apache-2.0 | Thinker–Talker MoE，冷启动理论首包延迟 234 ms；与百炼实时模型同源，私有化与云上行为最接近 |
| **Step-Audio 2 mini** | Apache-2.0 | 阶跃开源；完整版 Step-Audio 2 的许可证未确认 |
| **MiniCPM-o 4.5**（9B） | 许可证待核实 | 宣称 Gemini 2.5 Flash 水平，支持**全双工**音视频流，可在手机端运行，有 vLLM 支持 |
| **Kimi-Audio** | 许可证待核实 | 音频理解与端到端对话 |
| Kyutai Moshi | — | 全双工开创者，但 AA 推理分仅 4%，不适合需要做事的助手 |

私有化部署需要客户 IDC 有 GPU；端到端语音模型的并发成本远高于文本模型，"每路通话占用多少显存与算力"需要压测后才能给报价。

## 5. 对 yanshi 的建议

1. **先定接口，再选厂商。** 各家协议都是 WebSocket 双向流，事件形态相近（会话配置、音频追加、VAD、增量输出、打断），但细节各异，本次未逐一核对与 OpenAI Realtime 协议的兼容程度。在 Call 层定义 yanshi 自己的实时模型接口，适配器按厂商实现，与 `model.Provider` 的模型中立思路一致（G9）。
2. **Call 里的工具调用是硬性要求。** 场景 B 要求通话中派生 Run（ADR-0005），模型须支持 function calling。GLM-Realtime 的 Function Calling 仅限语音通话；其他厂商需逐一确认。
3. **建立中文实时评测。** AA 榜是英文的，且没有豆包和 GLM。按场景 B 准备一组中文录音：口语、随时打断、背景噪声、工具调用（读 PPT 第三页、改标题），测首音延迟、打断响应、任务成功率、单分钟成本，作为准入依据（章程原则 10）。
4. **Moderation 与计量。** 实时语音的输出边生成边播放，ADR-0021"写日志之前检查"的前提在 Call 中需要重新设计（例如对转写做流式检查并能中途掐断）；计量需支持"按 token"和"按时长"两种计费口径（ADR-0019 的价格表需要扩展）。
5. **带凭证测延迟。** 网络层已全部打通，下一步是拿到各家 Key 后测鉴权、首音延迟与打断响应。

## 6. 级联方案：腾讯云 ASR + 文本模型 + TTS

不用端到端实时语音模型时，Call 可以用"流式 ASR → 文本模型（如 TokenHub 上的 deepseek）→ 流式 TTS"级联。好处是复用现有文本模型、Moderation 可以检查完整的文本、转写天然可得；代价是延迟更高（三段串行），语气、情绪等副语言信息会丢失，打断与轮次判断要自己做（VAD）。

### 6.1 能力与价格（腾讯云官方计费页，ASR 页面更新于 2026-09-08，TTS 页面更新于 2026-08-10）

**语音识别（ASR）**

| 产品 | 后付费 | 预付费最低单价 | 说明 |
|---|---|---|---|
| 实时语音识别 · **大模型 2.0 版** | **1.0 元/小时**（≈0.017 元/分钟，无阶梯） | 0.85 元/小时 | WebSocket 流式，适合 Call |
| 实时语音识别 · 大模型 1.0 版 | 4.8–3.0 元/小时（按日用量阶梯） | 2.5 元/小时 | |
| 实时语音识别 · 标准版 | 3.2–1.2 元/小时 | 1 元/小时 | |
| 一句话识别 | 3.2–1.2 元/千次 | 1.2 元/千次 | 60 秒以内短音频，HTTP API |
| 录音文件识别 · 大模型 2.0 版 | 0.8 元/小时 | 0.72 元/小时 | 适合场景 A 的会议录音转写 |

- 每月免费：实时识别 5 小时、录音文件 10 小时、一句话 5000 次。后付费默认关闭，需在控制台开启。
- 并发叠加包：大模型 2.0 版实时识别 200 元/并发/月（上限 50）；页面没有写基础并发数。

**语音合成（TTS）**

| 音色 | 后付费（元/万字符） | 资源包最低 | 默认并发 | 折合每分钟语音* |
|---|---|---|---|---|
| 精品音色 | 0.3 | 0.15 | 20 | ≈0.008 元 |
| 大模型音色 | 1.2 → 0.55（按日用量阶梯） | 0.4 | 20 | ≈0.03 元 |
| 超自然大模型音色 | 6.5 → 4.9 | 4.8 | 10 | ≈0.18 元 |

\* 按中文语速约 270 字/分钟粗算。

- 接口：基础合成（HTTP，整段返回）、实时语音合成（`wss://tts.cloud.tencent.com/stream_ws`）、**流式文本语音合成**（`wss://tts.cloud.tencent.com/stream_wsv2`，面向大模型逐字输出，单会话 ≤1 万字，10 分钟无文本自动关闭，不支持 SSML）。实时与流式共享并发。
- 免费额度（领取后 3 个月）：超自然 2 万字符、大模型音色 10 万字符、精品 800 万字符。并发叠加包 200–350 元/并发/月。
- 官网有"一句话声音复刻"（5–15 秒训练音频），计费页未列价格。

**级联成本粗算**：ASR 0.017 元 + TTS 0.03 元（大模型音色）≈ **0.05 元/分钟**，另加文本模型的 token 费用；用超自然音色约 0.2 元/分钟。与 Qwen 实时系列同一量级，低于豆包、OpenAI。注意**并发叠加包按月收费**，规模上来后可能成为主要成本（每路并发每月 200–350 元）。

另：腾讯云 TRTC 有"AI 实时对话"方案（RTC + ASR + LLM + TTS 托管编排），本次未查到计费与能否接入第三方模型，未纳入比较。

### 6.2 连通性（2026-10-10，同 §3 方法）

| 端点 | 用途 | 结果 |
|---|---|---|
| `asr.tencentcloudapi.com` | ASR 的 API 3.0（一句话、录音文件） | **通**（无签名请求返回参数错误 JSON） |
| `wss://asr.cloud.tencent.com/asr/v2/<appid>` | 实时语音识别 | **通**（WebSocket 101） |
| `tts.tencentcloudapi.com` | TTS 的 API 3.0（基础合成，非流式） | **通** |
| `wss://tts.cloud.tencent.com/stream_ws`、`/stream_wsv2` | 实时 / 流式语音合成 | 白天不通（TLS Client Hello 后被断开，真实 IP 43.140.59.206 同样失败）；**晚间复测：101，可达** |

`tts.cloud.tencent.com` 白天的症状与百炼北京、阶跃、方舟一致；晚间复测已可达，开发机上可以直接使用流式合成。

## 7. 来源

- [Artificial Analysis：Speech to Speech 榜](https://artificialanalysis.ai/speech-to-speech) · [方法说明](https://artificialanalysis.ai/methodology/speech-to-speech-benchmarking) · [指数发布说明](https://artificialanalysis.ai/articles/announcing-the-artificial-analysis-speech-to-speech-index)
- 阿里：[Qwen-Omni 实时模型文档](https://help.aliyun.com/zh/model-studio/realtime) · [qwen3.5-omni-flash-realtime](https://help.aliyun.com/zh/model-studio/qwen3-5-omni-flash-realtime) · [百炼模型价格](https://help.aliyun.com/zh/model-studio/model-pricing) · [Qwen Audio 3.0 Realtime 价格（第三方）](https://wavespeed.ai/blog/cost-and-billing/qwen-audio-3-realtime-pricing/)
- 字节：[豆包语音计费说明](https://docs.volcengine.com/docs/6561/1359370?lang=zh) · [SeedRealtime 报道](https://www.aitoollab.cn/articles/seedrealtime-bytedance-audio-video-full-duplex-2026/)
- 阶跃：[定价与限速](https://platform.stepfun.com/docs/zh/guides/pricing/details) · [StepAudio 2.5 Realtime 介绍](https://ai-bot.cn/stepaudio-2-5-realtime/) · [Step-Audio 2 mini](https://huggingface.co/stepfun-ai/Step-Audio-2-mini)
- 智谱：[GLM-Realtime 文档](https://docs.bigmodel.cn/cn/guide/models/sound-and-video/glm-realtime) · [API 定价](https://docs.bigmodel.cn/cn/guide/start/pricing)
- 腾讯：[混元 TokenHub 调用指南](https://cloud.tencent.com/document/product/1823/132252)
- OpenAI：[gpt-realtime-1.5 模型页](https://developers.openai.com/api/docs/models/gpt-realtime-1.5) · [Realtime 价格估算（Layer3）](https://www.layer3labs.io/guides/openai-realtime-api-pricing) · [不支持地区封禁（SCMP）](https://scmp.com/tech/policy/article/3267971/tech-war-openai-further-block-access-mainland-china-hong-kong-based-developers)
- 对比文章：[OpenAI vs Gemini vs Qwen Realtime](https://tech-insider.org/openai-vs-google-vs-qwen-voice-ai-apis-2026/) · [GPT-Live vs GPT-Realtime vs Gemini Live](https://micdrop.dev/blog/gpt-live-vs-gpt-realtime-vs-gemini-live) · [Grok Voice Think Fast 2.0 价格](https://www.eesel.ai/blog/grok-voice-think-fast-2-pricing) · [Grok API 价格指南](https://www.cometapi.com/grok-api-pricing-cost-guide/)
- 腾讯云语音：[语音识别计费（在线版）](https://cloud.tencent.com/document/product/1093/35686) · [实时语音识别 WebSocket](https://cloud.tencent.com/document/product/1093/48982) · [语音合成计费](https://cloud.tencent.com/document/product/1073/34112) · [流式文本语音合成](https://cloud.tencent.com/document/product/1073/108595) · [实时语音合成](https://cloud.tencent.com/document/product/1073/94308)
- 开源：[MiniCPM-o 4.5](https://huggingface.co/openbmb/MiniCPM-o-4_5) · [Qwen3-Omni 技术报告](https://arxiv.org/abs/2509.17765)
