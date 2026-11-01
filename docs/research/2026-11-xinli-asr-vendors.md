# 调研：xinli 旁听的语音识别供应商

> 状态：公开资料调研（2026-11-01），不是选型结论；未调用付费 API、未做音频实测、未取得商务报价或专项数据协议。
>
> 关联：[选型与评测](../design/xinli-asr-selection.md)、[评分表](../design/xinli-asr-scorecard.md)、[领夹与识别](../design/xinli-asr-hardware.md)。价格、配额与接口能力以明确的资源 ID、模型版本、区域和最终订单为准。

## 1. 结论

1. **火山、腾讯、讯飞进入第一轮技术与商务预审，补阿里为第四家。** 建议先测火山文件识别 2.0、腾讯会议 2.0、讯飞新录音文件转写大模型的“分离＋佩戴者声纹”配置；阿里 Fun-ASR 与 Paraformer-v2 先作转写/片内分离对照，补齐佩戴者判定后才比较完整方案。以下是基于公开能力的研究判断，不代表这些方案已达到 xinli 的效果门槛。
2. **没有一家可以仅凭本次公开资料判定 A1 全部通过。** 不训练、识别后删除音频、境内处理和删除证明必须逐项落实到所购接口；产品隐私政策、消费端产品承诺或其他区域的零留存政策不能替代。
3. **说话人分离不等于识别佩戴者。** 片内编号不能直接映射为 `wearer`，也不能直接当作跨分片、跨天身份。声纹注册、比对与硬件信号融合分别评测，不能因为供应商提供其中一项就认为 A3 已满足。
4. **标准批处理仍要与流式比较。** 火山公开基础价为文件 2.0 的 0.8 元/小时、流式 2.0 的 1 元/小时；这 25% 差价不足以预先排除流式。标准档公开完成时间也没有证明分钟级 P95。[火山计费说明](https://docs.volcengine.com/docs/doubao-voice/billing-instructions?lang=zh)
5. **先保留忠实转写，再评测整理效果。** 首轮只测直接 ASR 指标，不运行整理 Agent，A4/B2 留到 2–3 个候选的端到端验证。关闭语义顺滑、去口误、书面语改写及 LLM 后处理；否定、改期和自我纠正是承诺依据，不能为了文本通顺先删掉。
6. **华为、百度留在第二批。** 华为具备会议分离等相关能力，但有参数兼容性待确认；百度当前接口已有多人分离，主要短板是公开返回时效与词级信息，不能简单归类为“不支持说话人”。见 §4.5。

## 2. 本次比较的边界

- 目标是中文日常自然对话的领夹录音：佩戴者近场、他人远场，夹杂重叠、媒体声、衣物噪声、口音和改口。
- 产出是带说话人与说话时间的文字片段，接入应用层 `Recognizer`；内核继续只接收业务线提供的可信结构化文字。
- 本次按每天送识别 3 小时、每月 30 天，即 **90 小时/台/月**计算基础价。它是画像假设，不是实际语音占比，也不是全年合同报价。
- 首轮关注 A3 他人误标佩戴者、A5 关键错误率、A10 召回与完整性，以及从说完话到文字可供业务使用的延迟；A4 下游损失只用于短名单。公开功能支持不能代替这些测量。
- 同厂的标准、极速、流式，及同接口的不同模型或资源 ID，均作为独立配置。本文不合并它们的价格、语言、热词、分离和保留期。
- 未公开或文档相互冲突的能力标为“待确认”；未找到承诺不等于供应商一定不能提供，但不能据此填“通过”。

## 3. 第一轮候选

| 候选配置 | 纳入理由 | 最先验证的限制 | 建议位置 |
|---|---|---|---|
| 火山 `volc.seedasr.auc`，按片长选择 SSD 200/300，加佩戴者 `voiceprints` | 文件 2.0、时间戳、热词及分离配置明确 | 标准异步分钟级 P95；具体数据协议 | 主候选 |
| 腾讯 `16k_zh_en_meeting`，另测 `16k_zh_en_2.0`，加角色认证 | 会议重叠定位与中英多方言配置可分别比较 | 相同开关在两种引擎上的效果；结果保留口径 | 主候选 |
| 讯飞新录音文件转写大模型，`roleType=3`＋佩戴者 `featureIds` | 方言覆盖、盲分与独立声纹接口 | 新 API 的请求级热词、准确报价、不训练协议 | 主候选 |
| 阿里 Fun-ASR 文件识别，北京区域 | 文件分离、词句时间戳、词表热词 | 独立佩戴者判定、长音频分离稳定性、北京异步链路保留期 | 组件候选，补齐后比较完整方案 |
| 阿里 Paraformer-v2 文件识别，北京区域 | 基础价明显更低，保留完整链路比较价值 | 关键错误率、重叠与独立佩戴者判定成本 | 组件成本基线 |

矩阵的功能依据分别为[火山标准 API](https://docs.volcengine.com/docs/doubao-voice/task-submission-http-1?lang=zh)、[腾讯 CreateRecTask](https://cloud.tencent.com/document/product/1093/37823)、[讯飞新文件转写 API](https://www.xfyun.cn/doc/spark/asr_llm/Ifasr_llm.html)、[阿里 Fun-ASR API](https://help.aliyun.com/zh/model-studio/fun-asr-recorded-speech-recognition-http-api)与[Paraformer API](https://help.aliyun.com/zh/model-studio/paraformer-recorded-speech-recognition-restful-api)；“建议位置”是本次研究判断。

## 4. 各家能力与不能混用的边界

### 4.1 火山引擎 / 豆包

**首选实测文件 2.0 标准版。** `volc.seedasr.auc` 使用 `submit/query` 异步接口；官方已将 `volc.bigasr.auc` 1.0 列为历史版本。输入最大 512 MB、5 小时，支持单/双声道；文档称通常 3 小时内完成，高峰可能延迟。[任务提交](https://docs.volcengine.com/docs/doubao-voice/task-submission-http-1?lang=zh)

- `show_utterances=true` 返回分句与逐字毫秒时间戳；分离结果为 `utterances[].additions.speaker`。这是音频中的编号，接口并未替 xinli 指定谁是佩戴者。[结果查询](https://docs.volcengine.com/docs/doubao-voice/result-query-http-1?lang=zh)
- 默认中英模型支持上海话、闽南、四川、陕西、粤语等；最新提交页另列冀鲁、兰银、江淮官话。指定语言可使用日语、韩语等扩展语种，但分离仅在不指定语言或指定 `zh-CN` 时生效。[任务参数](https://docs.volcengine.com/docs/doubao-voice/task-submission-http-1?lang=zh)
- 支持请求内热词、预建热词表和上下文；标准版合计最多 5000 个热词、上下文 500 tokens，不能与自动语种检测同时使用。首轮保持 `enable_ddc=false`。[任务参数](https://docs.volcengine.com/docs/doubao-voice/task-submission-http-1?lang=zh)

| 分离变体 | 参数 | 官方定位 | xinli 的测试用途 |
|---|---|---|---|
| SSD 200 普通 | `ssd_version="200", ssd_mode=0` | 3 分钟以内；二人较优，建议五人以内 | 2 分钟分片 |
| SSD 200 长音频 | `ssd_version="200", ssd_mode=1` | 3 分钟以上非会议；减少过度拆分身份 | 5/10 分钟日常交谈 |
| SSD 300 | `ssd_version="300"` | 长音频会议、录音笔、笔记等 | 5/10 分钟多人会议 |

以上均需同时开启 `enable_speaker_info` 和 `show_utterances`。额外声纹匹配要求预先注册，一次请求最多带 50 个声纹 ID；没有注册声纹的片内分离不能当作跨天身份。[分离最佳实践](https://docs.volcengine.com/docs/doubao-voice/speaker-separation?lang=zh)

**极速版另列。** `volc.bigasr.auc_turbo` 的 `recognize/flash` 一次请求返回，≤2 小时、≤100 MB，支持 WAV/MP3/OGG Opus 和 URL/Base64；多声道会增加处理时间。请求字段沿用标准版但排除回调及部分客服分析字段；具体 SSD 配置仍需验证。[极速版 API](https://docs.volcengine.com/docs/doubao-voice/large-model-recording-file-lite-recognition-api?lang=zh)

官方模型列表给出极速版 30 分钟音频约 10 秒返回、闲时版 24 小时内返回。这是档位介绍，不能当作高峰 P95；列表将这些档位并列，也不能据此给仍使用 `bigasr.auc_turbo` 的请求套用标准 2.0 单价。[模型列表](https://docs.volcengine.com/docs/doubao-voice/model-list?lang=zh)

**流式 2.0 是必要对照。** `volc.seedasr.sauc.duration` 配合 `bigmodel_async` 和 `enable_nonstream=true` 可做二遍识别；`bigmodel_nostream` 是整句输出模式。实时结果有句/字时间戳与 `definite`；只将最终态交给业务。[实时 API](https://docs.volcengine.com/docs/doubao-voice/bidirectional-streaming-automatic-speech-recognition-websocket?lang=zh)

流式分离最多区分 10 人，二遍尾包延迟官方描述增加 40–80 ms；双向流式默认热词与上下文合计仅 100 tokens，开二遍后按单向规则核对。文件版的语种和热词上限不可直接复制到所有流式模式。[分离最佳实践](https://docs.volcengine.com/docs/doubao-voice/speaker-separation?lang=zh)、[实时参数](https://docs.volcengine.com/docs/doubao-voice/bidirectional-streaming-automatic-speech-recognition-websocket?lang=zh)

### 4.2 腾讯云

**首测两个 2.0 引擎。** `16k_zh_en_meeting` 面向会议多人重叠，`16k_zh_en_2.0` 的官方详细说明及产品功能表称支持中英与 31 种方言；同一 API 的简短枚举摘要只写普通话与少量英语，目标方言仍须实测。保持其他输入相同，分别作为候选。文件 URL 输入≤5 小时/1 GB，本地数据≤5 MB。[CreateRecTask](https://cloud.tencent.com/document/product/1093/37823)、[产品功能](https://cloud.tencent.com/document/product/1093/35682)

- 片内分离对照配置为 `ChannelNum=1, SpeakerDiarization=1, SpeakerNumber=0, ResTextFormat=2/3`；完整方案另测 `SpeakerDiarization=3` 配合 `SpeakerRoles` 的佩戴者身份认证，具体参数组合待确认。文档明确这两个 2.0 引擎支持自动说话人分离，最多 20 人；该上限不是二十人场景的准确率保证。[请求参数](https://cloud.tencent.com/document/product/1093/37823)
- 返回句级 `StartMs/EndMs`、词级 `OffsetStartMs/OffsetEndMs` 与 `SpeakerId`；词偏移需按接口定义还原到音频时间轴。[结果查询](https://cloud.tencent.com/document/product/1093/37822)
- `HotwordList` 可按请求直传≤128 个词，文档说明临时热词不留存。首轮 `FilterModal=0, FilterDirty=0`，不启用书面语或 LLM 后处理。[请求参数](https://cloud.tencent.com/document/product/1093/37823)
- 提交限频 20 次/秒；官方描述通常 1 小时录音约 1–3 分钟处理，最长 3 小时且不含下载时延。突发量另有限制，不能据此宣布分钟级端到端 P95 已达标。[请求说明](https://cloud.tencent.com/document/product/1093/37823)

**极速版不是上述 2.0 的同配置加速。** 当前极速文档列≤2 小时/100 MB，普通并发 20、大模型 1.0 并发 5；未在该接口引擎参数中确认 2.0，不能沿用会议 2.0 的能力和价格。[极速版 API](https://cloud.tencent.com/document/product/1093/52097)

**身份能力分开看。** 实时 V2 的 `speaker_context_id` 有 24 小时有效期，不能当长期跨天身份库；声纹注册另有接口。文件接口的 `SpeakerDiarization=3` 与 `SpeakerRoles` 属于收费角色认证：通用引擎可传参考声音，meeting 文档列预注册 `VoicePrintId`，具体组合与兼容性描述仍需供应商确认。[实时 V2](https://cloud.tencent.com/document/product/1093/131127)、[SDK 参数](https://cloud.tencent.com/document/product/1093/133356)、[声纹注册](https://cloud.tencent.com/document/product/1093/94483)

**价格解读限于基础方案。** 文件 2.0 基础价 0.8 元/小时；结合请求参数的收费标记与计费表，普通分离 `=1` 和 `ResTextFormat=2/3` 未单列增值收费，暂按基础费用估算，尚未用账单验证。`ResTextFormat=4/5/7` 分别涉及 NLP 分段、书面语和 LLM 后处理，`SpeakerDiarization=3` 涉及声纹角色认证；这些是参数值，不是单价，对应增值能力按各自计费项另收费。[请求参数](https://cloud.tencent.com/document/product/1093/37823)、[计费说明](https://cloud.tencent.com/document/product/1093/35686)

### 4.3 讯飞开放平台

**首测新录音文件转写大模型。** 异步输入≤5 小时/500 MB，支持 8/16 kHz、16 bit、单声道及 Opus 等格式；`autodialect` 覆盖中英与 202 种方言，37 种语言能力需另申请。公开覆盖数不能代替 xinli 口音数据上的准确率。[新文件转写 API](https://www.xfyun.cn/doc/spark/asr_llm/Ifasr_llm.html)

- `roleType=1` 做盲分，`roleType=3` 配合最多 64 个已注册声纹。句时间使用毫秒、词信息使用 10 ms 帧单位，适配器必须统一单位。[新文件转写 API](https://www.xfyun.cn/doc/spark/asr_llm/Ifasr_llm.html)
- 声纹注册音频要求 10–60 秒，另有修改、删除等管理接口；注册与删除闭环是独立能力，不代表 wearer 识别已过安全门槛。[声纹管理](https://www.xfyun.cn/doc/spark/asr_llm/voice_print.html)
- **本次未在新 API 确认请求级热词参数。** 旧标准版的 `hotWord` 支持≤200 词、每词 2–16 字符，不能据此填写新模型的能力。旧版还建议至少 5 分钟录音，结果 72 小时删除。[旧标准版 API](https://www.xfyun.cn/doc/asr/ifasr_new/API.html)

**极速版单独评估。** 支持 16 kHz/16 bit 单声道 WAV/PCM/MP3，≤5 小时/500 MB；MP3 不支持角色分离。文档给出的 1 小时音频约 1 分钟、短音频约 20 秒是参考时效，结果保留 7 天，均需与新文件模型区别处理。[极速版 API](https://www.xfyun.cn/doc/asr/speedTranscription/API.html)

新实时转写 API 可在标准文件时效不足时补测，第一轮先避免同时铺开所有档位。[实时转写大模型](https://www.xfyun.cn/doc/spark/asr_llm/rtasr_llm.html)

**价格暂填询价。** 产品页搜索索引曾出现 198 元/80 小时至 36 万元/30 万小时，折合约 1.2–2.475 元/小时；动态页面未完整复核，且未确认适用的新模型、角色与声纹配置，不能用于排名。并发、取整、附加费及最新延迟也待商务书面确认。[转写产品页](https://static.xfyun.cn/services/lfasr)

### 4.4 阿里云百炼

**Fun-ASR 为转写/分离候选，Paraformer-v2 为低成本基线。** 本次未确认同一文件 API 提供注册佩戴者匹配；要成为完整候选，须另配身份组件或经验证的硬件融合，费用与效果一起比较。 文件识别链路支持句词时间戳和说话人分离；分离限单声道并建议音频≤2 小时，而文件任务本身上限可达 12 小时/2 GB。这是不同限制，不能合成“12 小时均能稳定分离”。[模型概览](https://help.aliyun.com/zh/model-studio/asr-model/)、[文件识别指南](https://help.aliyun.com/zh/model-studio/non-realtime-speech-recognition-user-guide)

- Fun-ASR API 明确提供 `vocabulary_id`、`speaker_id`、`diarization_enabled` 和 `speaker_count`；后者可设 2–100，是模型参考信息而非百人分离质量承诺。可选北京区域端点。[Fun-ASR API](https://help.aliyun.com/zh/model-studio/fun-asr-recorded-speech-recognition-http-api)
- Fun 的即时热词在不同文档版本中适用模型描述不一致，首轮使用预建 `vocabulary_id`；不能保证 Fun 支持与其他模型相同的请求级即时热词。
- Paraformer-v2 同样有词表、说话人分离与词句时间戳，可设置 `disfluency_removal_enabled=false`。还须核对默认敏感词过滤对原文及关键错误率的影响。[Paraformer API](https://help.aliyun.com/zh/model-studio/paraformer-recorded-speech-recognition-restful-api)
- Fun-ASR 实时版、Fun-ASR-Flash 与文件版的说话人能力不同，不能把实时或 Flash 当作文件版的无损替换。[模型概览](https://help.aliyun.com/zh/model-studio/asr-model/)

**新模型作为后续质量对照。** 当前官方另列 Qwen-Audio-3.1-ASR-Flash-Filetrans；其参数、请求级热词与计费应独立确认，不套用旧 Qwen3-ASR 或 Fun-ASR。北京定价表采用输入 0.8、输出 2.7 元/百万 tokens；本次未核实音频 tokens 与时长的转换，不能填成每小时 0.8 元。[模型概览](https://help.aliyun.com/zh/model-studio/asr-model/)、[模型价格](https://help.aliyun.com/zh/model-studio/model-pricing)

Fun-ASR、Paraformer、Qwen 3.1 文件模型的公开限流分别为 600、1200、600 RPM；这是请求速率，不是同时处理数或完成时间保证。本次未找到分钟级 P95 承诺。[限流说明](https://help.aliyun.cn/zh/model-studio/rate-limit)

### 4.5 第二批：华为与百度

| 供应商 | 已确认的相关能力 | 不优先的原因与待确认项 |
|---|---|---|
| 华为 SIS | `chinese_16k_media`、16 kHz 单声道会议识别，自动分离最多 10 人；有 `vocabulary_id`、`need_word_info`，异步任务 | `need_analysis_info` 旧描述“仅 8 kHz”与 16 kHz 会议分离描述不完全一致；其他 8 kHz/双声道场景仅双人。价格、保留期及参数组合需确认 |
| 百度长语音 | 当前 `pid=8953` 支持单声道多人 ID；16 kHz/16 bit，≤500 MB，段级 `begin_time/end_time` | `words_info` 仍为预留；一般 12 小时内返回；本次未见该接口请求级热词，暂作离线补充 |

来源：[华为长录音 API](https://support.huaweicloud.com/api-sis/sis_03_0092.html)、[百度长语音 API](https://ai.baidu.com/ai-doc/SPEECH/Klbxern8v)、[百度产品页](https://ai.baidu.com/tech/speech/aasr)。百度 API 页面更新于 2026-05-26，不能沿用旧资料判断它没有多人分离。

## 5. 统一成本口径

以下均是公开基础推理价或待确认状态，按 **90 小时输入/月、单音轨**估算；不含存储、流量、重试、二次识别、声纹和额外并发。报价不能替代用同一批音频取得的实际账单。

| 配置 | 公开基础单价 | 90 小时/月 | 适用限制与依据 |
|---|---:|---:|---|
| 火山文件 2.0 标准 | 0.8 元/小时 | 72 元 | `volc.seedasr.auc`；[计费](https://docs.volcengine.com/docs/doubao-voice/billing-instructions?lang=zh) |
| 火山流式 2.0 小时版 | 1 元/小时 | 90 元 | 不含并发增购；[计费](https://docs.volcengine.com/docs/doubao-voice/billing-instructions?lang=zh) |
| 火山大模型极速 | 4.5 元/小时 | 405 元 | 计费页仍单列此档；不能套标准 2.0 单价；[计费](https://docs.volcengine.com/docs/doubao-voice/billing-instructions?lang=zh) |
| 腾讯文件 2.0 基础 | 0.8 元/小时 | 72 元 | 不含增值后处理与角色认证；[计费](https://cloud.tencent.com/document/product/1093/35686) |
| 腾讯流式 2.0 基础 | 1 元/小时 | 90 元 | 不含并发或增值能力；[计费](https://cloud.tencent.com/document/product/1093/35686) |
| 讯飞新文件大模型 | 询价 | 待定 | 索引价格尚不足用于所选 API 排名；[产品页](https://static.xfyun.cn/services/lfasr) |
| 阿里 Fun-ASR，北京 | 0.00022 元/秒，即 0.792 元/小时 | 71.28 元 | 按输入音频、单音轨；[价格](https://help.aliyun.com/zh/model-studio/model-pricing) |
| 阿里 Paraformer-v2，北京 | 0.00008 元/秒，即 0.288 元/小时 | 25.92 元 | 按输入音频、单音轨；[价格](https://help.aliyun.com/zh/model-studio/model-pricing) |

腾讯按秒向上取整；阿里多音轨独立计费。火山直接 ASR 2.0 的最小粒度、双声道倍率、静音/失败请求、SSD/声纹附加费仍待确认，不能套用 Coze 或 LAS 的转售链路规则。[腾讯计费](https://cloud.tencent.com/document/product/1093/35686)、[阿里 Fun API](https://help.aliyun.com/zh/model-studio/fun-asr-recorded-speech-recognition-http-api)、[火山计费概述](https://docs.volcengine.com/docs/doubao-voice/billing-overview?lang=zh)

火山文件服务公开默认 20 QPS，半小时提交音频不超过 500 小时；极速版默认 5 并发、增购 400 元/并发/月。流式容量行列默认 10 并发、增购 100 元/并发/月，但没有像价格行一样明确标 2.0，须以实际资源控制台/订单确认。[火山配额与增购](https://docs.volcengine.com/docs/doubao-voice/billing-instructions?lang=zh)

**研究判断：** Paraformer-v2 值得作为完整链路基线；首轮须补齐佩戴者判定并通过 A3/A5/A10，短名单再验证 A4，低价才有完整决策意义。峰值并发、VAD 余量、重叠分片、复核比例与失败重试应进入 B7，不能只比较刊例小时价。

## 6. 数据条款与上线门槛

| 供应商 | 公开资料能支持的事实 | A1 尚缺什么 |
|---|---|---|
| 火山 | AI 技术服务专用条款 §4.3 包含语音用户数据用于服务改进、模型优化的授权；页面较早，当前 ASR 2.0 适用性待确认 | 所购资源的专项不训练协议，音频/转写/诊断副本 TTL，境内处理范围、主动删除与证明 |
| 腾讯 | FAQ 称当次音频不保存、文本保留 7 天；查询 API 的 TaskId/任务查询窗口为 24 小时，两者作用对象不同 | 查询失效不等于物理删除；需确认各层保留期、不训练、实际处理地域及删除证明 |
| 讯飞 | 隐私政策允许匿名化/去标识语音用于训练；境内存储条款另有例外；不同 API 结果期限不同 | 覆盖所买 API 的专项不训练、保留/删除、地域和例外处理协议 |
| 阿里 | 百炼隐私说明明确不用于模型训练，同时说明会存储模型/应用调用数据 | 北京 ASR 的音频、转写、异步缓存与诊断数据 TTL；删除操作与证明 |

来源：[火山 AI 技术服务条款](https://docs.volcengine.com/docs/6456/70590?lang=zh)、[腾讯 FAQ](https://cloud.tencent.com/document/faq/1093/35802)与[结果查询](https://cloud.tencent.com/document/product/1093/37822)、[讯飞隐私政策](https://www.xfyun.cn/doc/policy/privacy.html)与[转写服务协议](https://www.xfyun.cn/doc/asr/ifasr_new/agreement.html)、[阿里隐私说明](https://help.aliyun.com/zh/model-studio/privacy-notice)。本表是条款核对线索，不认定旧协议自动适用于当前采购。

阿里国际站零留存说明面向其适用区域与模式，异步任务链路另有排除；不能拿它证明北京文件 ASR 零留存。[国际站数据保留政策](https://www.alibabacloud.com/help/zh/model-studio/data-retention-policy)

测试音频、转写、人工金标与供应商原始结果均放受控、可删除的数据集存储；仓库只放合成/脱敏夹具和不含个人内容的清单。评测也要遵守数据协议，不能先上传真人数据再补确认。

**可用性与时效分开验收。** 火山公开智能语音 SLA 对大模型文件/流式识别列 99.00% 可用性，超配额限流等不计入；它不承诺准确率或分钟级 P95。其他厂商也须分别填写服务可用性、任务时效与效果证据。[火山 SLA](https://docs.volcengine.com/docs/DoubaoVoice/VolcanoEngine-SpeechAndAudioGradeOfServiceAgreement?lang=zh)

## 7. 可执行的首轮评测

### 7.1 固定输入与候选配置

1. 火山、腾讯、讯飞先测“分离＋注册佩戴者身份”完整配置；阿里两种模型先测组件，未补齐身份能力时不能与完整方案混排。火山按片长选择 SSD，腾讯补另一 2.0 引擎作为独立变体。每片复用同一佩戴者注册身份，不能复用局部编号认人。
2. 分片取 2、5、10 分钟，保留同一连续录音作为参考。记录 VAD、编码、声道、设备与前端配置，先比较共同基线，再允许只在调参集上优化。
3. 真人小样本前移。覆盖近场/远场、二人/多人、重叠、衣物摩擦、走路、媒体声、不同佩戴位置，以及否定、日期、金额、人名和改口；身份场景包含佩戴者整片不发言、极短回应、跨片换号、跨天变化和信号失效。
4. 热词仅取上线时已有的联系人/项目词，不从金标提取；记录各接口截断或不支持的词量。讯飞新 API 的热词在确认前按“未验证”记录。
5. 明确关闭顺滑、去口误及额外改写。保存供应商原始响应与归一化结果供复算，不能只保存供应商已美化的全文。

### 7.2 最少应产出的结果

| 项 | 报告要求 |
|---|---|
| A3 佩戴者安全 | 他人→wearer 单列，媒体声与重叠单列；报告分母、样本量、置信区间与拒识比例 |
| A4 下游损失（首轮暂缓） | 仅短名单验证；先修正评分器，让分母与修订状态由参考音频时间轴独立决定，漏句、漏取消和错标均计损失 |
| A5 关键错误 | 直接对照人工参考，否定、改口、日期金额、人名分别统计；不调用整理 Agent，阈值不依赖退化曲线 |
| 完整性 | VAD/传输漏音、重复、分片边界损失、跨午夜与时间漂移，均进入整条链路结果 |
| 延迟 | 从实际说话到业务可读取，拆出切片等待、上传、排队、识别、按序提交；高峰与补传分别报 P50/P95/P99 |
| 成本 | 实际计费音频、附加能力、重试、复核、并发、流量与存储；按轻/典型/重度画像折算 |
| 身份 | 片内区分、跨片佩戴者、可选他人跨天身份分别报告；实际阈值下测误识/误拒；不得把编号 0/1 默认当 wearer |

真人数据先用于校准合成/重录偏差；最终安全样本量由所需误归属上限与置信区间决定，不能只凭固定几小时宣布通过。调参集与留出集按说话人、会话和场景隔离。

### 7.3 分阶段决策

- **资料预审：** 核对具体接口能力、申请配额、并行谈数据协议；此阶段只产生“可以进入测试”的名单。
- **技术初筛：** 用合成或取得同意且协议允许的数据跑上述配置；淘汰明显不兼容的接口，保留原始指标而非先压成总分。
- **新鲜度复核：** 文件标准版 P95 不足时，加火山流式 2.0 二遍以及腾讯/讯飞极速；每个档位重新核对价格、分离、热词与保留期。
- **首轮短名单：** 按真人直接 ASR 指标、跨片佩戴者、端到端时效和完整成本选出 2–3 个候选；硬门槛按通过/不通过/证据不足记录。
- **端到端验证：** 完成选型设计 §8.2 的评分器修正，再启用 A4/B2；各业务损失的置信上限不得超过容忍值，证据不足不能转为上线结论。
- **备份方案：** 备份供应商也应通过数据与效果门槛，明确声纹、热词、结果格式及删除动作的迁移方法，再做经授权的影子比对。

## 8. 发给每家商务与技术支持的核对清单

以下清单仅为准备材料，本次未向供应商发送消息。

1. 指定模型/资源 ID、端点、区域和全部开关后，哪些能力可同时启用？可否锁版本，更新如何通知？
2. 音频、转写、声纹、热词、请求上下文、诊断样本、缓存与备份分别保存多久，由谁处理？是否用于训练、评测或人工质检？
3. 能否签署覆盖所购 ASR 的不训练协议，约定识别结束后的音频删除、境内处理、分包方及删除证明？
4. 用户删除或注销时，能否主动删除在排队/处理中任务及结果、声纹和热词？迟到完成任务如何阻止恢复已删内容？
5. 说话人分离、声纹匹配、词时间戳、动态热词同时开启后的完整价目是什么？静音、重试、失败、双声道如何计费？
6. 2/5/10 分钟分片在约定峰值下的端到端 P95 是多少？QPS、在途并发、提交音频量、下载超时与排队上限分别是多少？
7. 请求级热词是否真正按用户隔离？最大词量、更新延迟、缓存范围和删除行为是什么？
8. 分离编号作用域是什么？跨请求复用要保存哪些状态？声纹误识、拒识、防重放与重叠场景有没有可核验的测试说明？

## 9. 仍未决的事项

- 火山极速档与 SSD 各版本的完整兼容性、当前模型命名与价格映射、2.0 容量配额、专项数据条款。
- 腾讯 24 小时任务查询窗口之外的各层保留与删除规则、会议引擎角色认证适用性、基础分离与时间戳的实际账单。
- 讯飞新文件大模型请求级热词、动态页面价格、角色/声纹附加费、并发、取整和专项不训练条款。
- 阿里北京异步 ASR 数据保留与主动删除、Fun 即时热词、Qwen 3.1 Filetrans 的版本参数和音频 token 成本。
- 华为参数描述冲突、百度词级输出和热词，以及两家的完整商业与数据条款。
- 所有候选的真实领夹效果、佩戴者误归属、端到端高峰 P95、VAD 后日时长分布与规模报价；这些都需要实测或书面确认。
