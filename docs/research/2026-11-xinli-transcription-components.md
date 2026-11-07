# 调研：xinli 转写链路中识别以外的组件

> 状态：公开资料调研（2026-11-01），不是选型结论；没有实测，没有调用付费 API，没有下载模型，没有联系厂商。“自报”指数字来自作者或厂商自己的仓库、模型卡或论文，没有独立复现。
>
> 关联：[转写方案](../design/xinli-transcription.md)、[评测](../design/xinli-transcription-eval.md)、[评分表](../design/xinli-transcription-scorecard.md)。商用 ASR 接口见 [ASR 厂商调研](./2026-11-xinli-asr-vendors.md)，本文不重复。
>
> 访问限制：help.aliyun.com、xfyun.cn、docs.volcengine.com 在调研环境中无法直接抓取，涉及这几家的内容只来自搜索摘录或国际站，文中标注“仅搜索摘录”，决策前须人工核对原页面。

## 1. 结论

1. **增强（降噪）后再送识别通常更差，这一条证据最充分。** Google 的语音识别最佳实践写明送识别前降噪“通常会降低识别准确率”，并要求关闭自动增益与降噪；NTT 的研究指出增强引入的失真（artifact）是识别变差的主因，把原始信号按比例加回可以缓解；2025–2026 年在 Whisper、Parakeet 等模型上的实验，增强后错误率全部或大多数变差。这些都不是中文日常对话，仍须在 xinli 数据上实测（§3.1）。
2. **“增强信号只给 VAD 用、原始音频送识别”没有直接对照实验**，还有厂商建议 VAD 用原始输入。只能作为评测变体（§3.1）。
3. **VAD 主候选是 FireRedVAD 与 Silero VAD v6**，许可证都干净；TEN VAD 有“不得与 Agora 竞争”的附加条款，FSMN-VAD 漏检最低但误检很高。所有公开指标都来自朗读或会议语料，**没有领夹远场与短回应的公开评测**（§3.3）。
4. **单麦克风区分电视外放与真人对话没有成熟方案。** “媒体声不能算佩戴者的话”主要靠佩戴者判定保证，声音事件只作弱信号（§3.4）。
5. **16 kHz 足够，编码不要压到很低码率。** 英语实验中 Opus 在 6 kb/s 明显变差，9–20 kb/s 基本无损；中文、LC3 与蓝牙 mSBC 没有查到识别数据。**领夹到手机若走 HFP 的 CVSD（8 kHz 窄带），损失可能最大，须优先核实**（§3.5）。
6. **开源中文识别第一档是 FireRedASR2-AED 与 Qwen3-ASR-1.7B**，都是 Apache-2.0，数字均为自报；前者有词级时间戳与置信度，后者有独立对齐器与上下文偏置。Whisper 中文明显落后且有幻觉记录，只适合作幻觉对照（§4）。
7. **佩戴者判定没有现成的开源整套方案。** 可行的组合是：开源说话人分离给出匿名分组，再用注册声纹做**簇级**比对（CAM++ 或 ERes2NetV2，20 万说话人中文模型，Apache-2.0，CPU 可跑），近场声级只作辅助或否决特征。短语音声纹不可靠：英文 1 秒片段等错误率约 8–18%，中文 CN-Celeb 全长约 6%（§5）。
8. **许可证是硬约束。** DiariZen 公开 DER 最好，但权重 CC BY-NC，不能商用；pyannote community-1 是 CC-BY-4.0、下载有门禁；FunASR 系列的模型协议与模型卡标注不一致（§9）。
9. **后处理只标记、不改写。** 幻觉检测有现成的工程信号；大模型纠错在中文上容易过度纠正；热词可能插入没说过的名字；商用接口的顺滑、去语气词、默认敏感词过滤与忠实转写冲突，必须关闭（§6）。
10. **一站式服务的“身份”不是声纹。** 通义听悟的身份识别是按文字描述推断角色，让内容决定信任，不能用来判定佩戴者（§7）。

## 2. 边界

- 场景：全天佩戴的领夹，中文日常自然对话，佩戴者近场、他人远场，重叠与电视外放常见。
- 现有硬件不能改：没有端侧 VAD、没有振动传感，可能单麦单声道；处理只能放在 HostApp 或云端。
- 音频识别完成即删除；数据须境内处理；声纹是敏感个人信息。
- 只要忠实转写；漏掉的话不可恢复，偏向召回；他人与媒体声不能被当作佩戴者的话。

## 3. 识别前的前端处理

### 3.1 增强对识别的影响

| 证据 | 类型 | 结论 |
|---|---|---|
| [Google Cloud STT 最佳实践](https://docs.cloud.google.com/speech-to-text/docs/best-practices) | 厂商文档 | 送识别前降噪“通常会降低识别准确率”；关闭 AGC 与降噪；16 kHz；避免有损编码 |
| [Iwamoto 等，Interspeech 2022](https://arxiv.org/abs/2201.06685) | 论文 | 增强误差分解为噪声与失真，失真是识别退化的主因；把原始信号按比例加回（observation adding）在模拟与真实录音上都改善识别 |
| [Ochiai 等，arXiv 2404.14860](https://arxiv.org/abs/2404.14860) | 论文 | 干扰、噪声、失真三类误差中失真“特别有害” |
| [arXiv 2512.17562](https://arxiv.org/abs/2512.17562)（医疗识别） | 预印本 | MetricGAN+ 增强后，Whisper、Parakeet、Gemini 等 40 个配置的语义错误率全部变差 |
| [arXiv 2603.04710](https://arxiv.org/abs/2603.04710) | 预印本 | 分离后感知指标上升，5 个 Whisper 变体的错误率全部上升 |
| [Pandey 等，SLT 2021](https://research.facebook.com/file/300037572046942/Dual-Application-of-Speech-Enhancement-for-Automatic-Speech-Recognition.pdf) | 论文 | 识别侧有可训练模块混合原始与增强特征时，相对改善约 8% |
| [Fun-ASR 技术报告](https://arxiv.org/html/2509.12508v4) | 厂商论文 | 训练中加入噪声与纯噪声段，餐厅、超市测试集相对改善超过 30%：鲁棒性主要在模型内部解决 |
| [Microsoft Speech SDK](https://learn.microsoft.com/en-us/azure/ai-services/Speech-Service/speech-devices) | 厂商文档 | 单麦克风或低信噪比时，自带降噪“效果很小”，需要麦克风阵列 |
| [ai-coustics 文档](https://docs.ai-coustics.com/reference/deprecated/energy-vad-to-dedicated-vad.md) | 厂商文档 | 建议 VAD 用原始输入，增强音频不在 VAD 的训练分布内 |

**研究判断：** 对在大规模含噪数据上训练的商用识别，默认假设“增强后送识别有害或无益”。某些场景实测有益时，优先试加回 0.2–0.5 倍原始信号（比例要调），不送纯增强信号。单麦没有空间信息，前端处理的收益主要在语音检测。

### 3.2 开源增强模型

| 模型 | 许可证 | 采样率 | 流式 | 规模与速度（自报） | 维护 |
|---|---|---|---|---|---|
| [GTCRN](https://github.com/Xiaobin-Rong/gtcrn) | MIT | 16 kHz | 是 | 48.2K 参数，RTF 0.07（i5-12400） | 活跃；已接入 [sherpa-onnx](https://k2-fsa.github.io/sherpa/onnx/speech-enhancment/models.html) |
| [DPDFNet](https://huggingface.co/Ceva-IP/DPDFNet/blob/main/README.md)（Ceva） | Apache-2.0 | 8/16/48 kHz | 是，因果 | 16 kHz 基线 2.31M 参数、0.36 GMACs | 2025 年底发布，已接入 sherpa-onnx |
| [RNNoise](https://github.com/xiph/rnnoise) | BSD-3-Clause | 48 kHz | 是 | 极小 | 2025-02 仍有提交 |
| [DeepFilterNet 2/3](https://github.com/Rikorose/DeepFilterNet) | MIT 或 Apache-2.0 | 48 kHz | 是 | RTF 0.19（i5-8250U） | 2023-08 后基本停更 |
| FRCRN（ModelScope `speech_frcrn_ans_cirm_16k`） | Apache-2.0（元数据） | 16 kHz | 未说明 | 约 58.5 MB | 并入 ClearerVoice |
| [ZipEnhancer](https://arxiv.org/abs/2501.05183) | Apache-2.0（元数据） | 16 kHz | 未说明 | 2.04M 参数、62.41 GFLOPs | 2025 |
| [ClearerVoice-Studio](https://github.com/modelscope/ClearerVoice-Studio)（MossFormer2 等） | Apache-2.0 | 16/48 kHz | 未提及 | Demo 依赖 GPU | 2025-06 仍有更新 |

都没有找到中文识别下游的评测。**研究判断：** 若给 VAD 提供增强副本，候选是 GTCRN 或 DPDFNet 基线（16 kHz、因果、算力最低）；ZipEnhancer、MossFormer 只适合作“增强能否帮识别”的上限参照。算力按“每天音频小时 × 单线程 RTF”估 CPU 核时：每天 5 小时，GTCRN 约 0.35 核时（桌面 CPU 自报数字，服务器上须压测）。

### 3.3 VAD

| VAD | 许可证 | 公开指标 | 移动端 | 规模与速度 |
|---|---|---|---|---|
| [FireRedVAD](https://github.com/FireRedTeam/FireRedVAD) | Apache-2.0 | FLEURS-VAD-102（作者自报，测试集尚未公开）F1 97.57、误检 2.69%、漏检 3.62%；同表 Silero 95.95/9.41/3.95、TEN 95.19/15.47/2.95、FunASR 90.91/44.03/0.42、WebRTC 52.30/2.83/64.15（[技术报告](https://arxiv.org/abs/2603.10420)）。在 Silero 维护者的多域集上 ROC-AUC 0.94，Silero v6 为 0.97（[第三方](https://github.com/snakers4/silero-vad/wiki/Quality-Metrics)） | ONNX、NCNN | 约 0.6M 参数；有流式版与“语音/唱歌/音乐”分类 |
| [Silero VAD v6](https://github.com/snakers4/silero-vad) | MIT | 维护者自报：AliMeeting 准确率 0.91、AISHELL-4 0.85 | ONNX、ExecuTorch | 每 30 ms 块单线程不到 1 ms |
| [TEN VAD](https://github.com/TEN-framework/ten-vad) | Apache-2.0 **加附加条件**：不得以与 Agora 竞争的方式部署（[LICENSE](https://github.com/TEN-framework/ten-vad/blob/main/LICENSE)） | 自报只有曲线；第三方多域 ROC-AUC 0.93 | Android、iOS、WASM | RTF 约 0.01 |
| FSMN-VAD（FunASR） | 代码 MIT；模型为 FunASR 自定义协议 | 只有 FireRedVAD 作者的对比：漏检 0.42%、误检 44.03% | 社区 ONNX | 未查到 |
| [NeMo MarbleNet v2.0](https://huggingface.co/nvidia/frame_vad_multilingual_marblenet_v2.0) | NVIDIA Open Model License | 自报 ROC-AUC：AISHELL-4 92.27，明显低于英文集 | ONNX | 91.5K 参数 |
| WebRTC VAD | BSD 类 | 多域 ROC-AUC 0.73 | 任意设备 | 极低 |
| Picovoice Cobra | 商用闭源 | 自报 0 dB SNR、FPR 5% 时 TPR 98.9% | 端侧 | 价格未公开 |

默认参数偏向截断短回应：Silero 默认 `min_speech_duration_ms=250`、`speech_pad_ms=30`。**研究判断：** 阈值低于默认值（如 0.3–0.4），最短语音不超过约 150 ms，前后补足余量；FireRedVAD 与 Silero v6 为主候选，FSMN-VAD 作“高召回”对照，TEN VAD 须法务确认后才进入候选。公开集都不能代表领夹场景，必须自建帧级标注集。

### 3.4 声音事件与媒体外放

| 模型 | 许可证 | 规模 | 能力 |
|---|---|---|---|
| FireRedVAD 的声音事件分类 | Apache-2.0 | 约 0.6M 参数 | 只有语音、唱歌、音乐三类；没有公开指标 |
| [PANNs](https://github.com/qiuqiangkong/audioset_tagging_cnn) | MIT | 未列出 | AudioSet 527 类，有 16 kHz 版本 |
| [YAMNet](https://github.com/tensorflow/models/blob/master/research/audioset/yamnet/README.md) | Apache-2.0 | 3.7M 参数 | 521 类，平衡 mAP 0.306 |
| [CED](https://github.com/RicherMans/CED) | **GPL-3.0** | 5.5M–86M 参数 | mAP 48.1–50.0，有 ONNX |
| [BEATs](https://github.com/microsoft/unilm/tree/master/beats) | MIT | 未核实 | mAP 50.6（自报），较重 |

AudioSet 本体有 [Television、Radio](https://research.google.com/audioset/ontology/sound_reproduction_1.html) 等“放出来的声音”类。媒体外放检测方面，[扬声器放音与自然语音的区分](https://arxiv.org/abs/1901.11291)只在自建数据集上约 90% 准确率；[ASVspoof 2021](https://arxiv.org/abs/2210.02437) 的真实房间重放检测泛化差；多麦方案用不上；**没有找到面向单麦可穿戴设备的开源模型或基准。**

**研究判断：** 声音事件只当标记，不据此删除音频。媒体声的硬约束交给佩戴者判定：电视里的人声通常与佩戴者声纹不匹配。可作弱特征的有 Television/Radio 分数、长时间无轮流对话的独白、背景音乐与人声同时出现，都需要自建数据评测。CED 是 GPL-3.0，只在服务端使用也须法务确认。

### 3.5 编码与采样

- Google 建议 16 kHz 或更高、避免重采样与有损编码。
- Opus（英语 LibriSpeech，[NoLACE 论文表 1](https://arxiv.org/html/2309.14521)）：未编码 2.01%；6 kb/s 为 3.08%，9 kb/s 为 2.15%，20 kb/s 为 2.03%。[Amazon 的研究](https://arxiv.org/abs/2002.00122)报告低于 16 kb/s 明显变差；Opus 按感知质量而不是识别准确率调参。
- LC3 只有感知质量资料，没有识别评测。蓝牙 HFP 的 CVSD 是 8 kHz 窄带，mSBC 是 16 kHz、64 kb/s。
- 主流中文识别模型的输入是 16 kHz 单声道。

**研究判断：** 领夹到手机为 16 kHz 宽带，手机到云端 Opus 不低于 16–24 kb/s 或无损；先核实领夹到手机的实际编码。（2026-11-07 补记：已确认领夹到手机为 Opus、16 kHz、20 ms 一帧、约 16 kb/s，见转写方案 §4.1。）评测加一组 PCM 与 Opus 16/24/32 kb/s（及适用的 LC3）对照。

## 4. 开源中文识别

数值均为自报；“WER”在中文上实际按字计。

| 模型 | 许可证 | 规模 | 中文基准 | 时间戳与置信度 | 热词与上下文 |
|---|---|---|---|---|---|
| [FireRedASR2-AED](https://github.com/FireRedTeam/FireRedASR2S) | Apache-2.0 | 1B+ | AISHELL-1 0.57；WenetSpeech net/meeting 4.57/4.53；KeSpeech 3.60；19 个方言集平均 11.67 | **词级时间戳与置信度** | 未提及 |
| FireRedASR2-LLM | Apache-2.0 | 8B+ | WenetSpeech 4.44/4.32；KeSpeech 3.08 | 示例中没有 | 未提及 |
| [Qwen3-ASR-1.7B / 0.6B](https://github.com/QwenLM/Qwen3-ASR) | Apache-2.0 | 1.7B/0.6B | 1.7B：WenetSpeech 4.97/5.88；KeSpeech 5.10；内部极端噪声集 16.17（Whisper 63.17） | 独立的 ForcedAligner-0.6B（每段 ≤5 分钟）；流式无时间戳 | 系统提示词可放上下文，无量化结果 |
| [Fun-ASR-Nano-2512](https://github.com/FunAudioLLM/Fun-ASR) | 代码 Apache-2.0；模型以模型卡为准 | 0.8B | WenetSpeech meeting/net 6.60/6.01 | ModelScope 版有字级时间戳 | 支持 `hotwords` |
| [Paraformer-large](https://huggingface.co/funasr/paraformer-zh) | 模型卡 Apache-2.0，与 FunASR 模型协议不一致 | 220M | 卡片未给 | 流水线输出时间戳、标点与说话人 | SeACo 热词（未核实） |
| [SenseVoice-Small](https://huggingface.co/FunAudioLLM/SenseVoiceSmall) | 模型协议（未核实） | 小 | 只有图表 | 未提及 | 未提及 |
| [Dolphin](https://github.com/DataoceanAI/Dolphin) | Apache-2.0 | 公开 0.1B/0.4B | 主页未给 | 词级时间戳 | 有 prompt 变体 |
| Whisper large-v3 | MIT | 1.55B | Qwen 报告测得 WenetSpeech meeting 19.11、KeSpeech 28.79 | 有 | prompt |
| [MOSS-Transcribe-Diarize](https://huggingface.co/OpenMOSS-Team/MOSS-Transcribe-Diarize) | Apache-2.0 | 0.9B | 一次完成转写与分离；cpCER AISHELL-4 15.83、AliMeeting 22.17；标签匿名 | 有 | 未提及 |
| [VibeVoice-ASR](https://huggingface.co/microsoft/VibeVoice-ASR) | MIT | 9B | 一次完成转写、分离与时间戳；指标只在图中 | 有 | 支持热词 |

幻觉记录：[Careless Whisper](https://arxiv.org/abs/2402.08021) 发现约 1% 的转写含音频中不存在的内容，非语音段越长越容易出现。Qwen3-ASR 训练时加入非语音数据、被训练成不跟随提示词指令，但没有量化幻觉率。FireRedASR2-AED 支持约 60 秒以内的输入，更长可能产生幻觉，过长会报错（[FAQ](https://github.com/FireRedTeam/FireRedASR2S#faq)）：调用片段须按模型上限切分。

**研究判断：** 自建候选为 FireRedASR2-AED（时间戳与置信度便于与说话人分段对齐、做不确定标注）与 Qwen3-ASR-1.7B（吞吐有公开数据、可放上下文）；Paraformer-large 或 Fun-ASR-Nano 作低成本基线（许可证须法务确认）；Whisper 只作幻觉对照。LLM 类识别在电视外放与非语音上的编造对承诺账本是安全问题，必须自测。

**自建成本（带假设的估算）：** Qwen3-ASR 技术报告自报 1.7B 离线并发 128 时约 980 秒音频/秒，未写 GPU 型号。按实际部署打 10–50 倍折扣、每 GPU 小时 20–100 音频小时、GPU 3–30 元/小时推算，约 0.03–1.5 元/音频小时。**不确定性超过一个数量级，不能作预算依据；也不能据此断言识别解码不是自建的主要成本**，还要计入说话人模块、闲置容量与运维。

## 5. 说话人区分、声纹与佩戴者判定

### 5.1 开源说话人分离

DER 均为自报，含重叠、无 collar 的口径以各自说明为准。

| 方案 | 许可证 | 中文会议远场 | 第一人称佩戴场景 | 流式 |
|---|---|---|---|---|
| [pyannote community-1](https://huggingface.co/pyannote/speaker-diarization-community-1) | 权重 CC-BY-4.0，HF 门禁；代码 MIT | AISHELL-4 11.7；AliMeeting 20.3 | **Ego4D 46.8** | 否 |
| [3D-Speaker](https://github.com/modelscope/3D-Speaker) | Apache-2.0（重叠检测依赖 pyannote 门禁模型） | AISHELL-4 10.30；AliMeeting 19.73 | 未查到 | 未提及 |
| [DiariZen](https://github.com/BUTSpeechFIT/DiariZen) | 代码 MIT；**权重 CC BY-NC，仅研究** | AliMeeting far 10.8 | 未查到 | 否 |
| [Sortformer 4spk v2.1](https://huggingface.co/nvidia/diar_streaming_sortformer_4spk-v2.1) | NVIDIA Open Model License（可商用，附条件） | AliMeeting far 13.55（30 秒延迟）、15.60（1 秒延迟） | 未查到；最多 4 人，主要用英文训练 | 是 |
| [WeSpeaker](https://github.com/wenet-e2e/wespeaker) | 代码 Apache-2.0 | 未给 | 未查到 | 未提及 |

跨仓库的对比表数值互不一致，不可直接采信。**研究判断：** 生产候选是 pyannote community-1、3D-Speaker 与 Sortformer；DiariZen 只作上限参照。xinli 更接近 Ego4D 而不是会议集，必须用自采佩戴数据评测；首要指标是他人被误标为佩戴者，不是 DER。

### 5.2 声纹

| 模型 | 参数 | 等错误率（自报） | 中文预训练模型 |
|---|---|---|---|
| [CAM++](https://arxiv.org/abs/2303.00332) | 7.2M | VoxCeleb1-O 0.65%；**CN-Celeb 6.78%** | `speech_campplus_sv_zh-cn_16k-common`（20 万说话人） |
| [ERes2NetV2](https://arxiv.org/abs/2406.02167) | 17.8M | VoxCeleb1-O 0.61%，3 秒 0.98%，2 秒 1.48%；**CN-Celeb 6.14%** | `speech_eres2netv2_sv_zh-cn_16k-common`（20 万说话人） |
| ERes2Net base/large | 6.6M/22.5M | CN-Celeb 6.69%/6.17% | 有 |

- 短语音（英文）：1 秒片段基线约 18%、改进方法约 8%（[arXiv 2609.25007](https://arxiv.org/pdf/2609.25007)）。**中文按 1/2/3 秒切分的数据未查到。**
- 鲁棒性：[SVeritas](https://arxiv.org/abs/2509.17091) 显示跨语言、年龄与编解码会造成明显退化；情绪语音错误率明显更高（[arXiv 2412.00319](https://arxiv.org/abs/2412.00319)）；感冒与跨天漂移未查到量化研究。
- 防伪：单独的反欺骗模型在 SASV 上接近随机，需与声纹分数融合（[arXiv 2509.12668](https://arxiv.org/abs/2509.12668)）；中文、针对重放的开源方案未查到。

**研究判断：** CAM++ 与 ERes2NetV2 是佩戴者声纹的首选候选，体量小、CPU 可跑。逐句判定必须是三态（佩戴者、他人、不确定），不确定在交出时按 `other` 处理；声纹比对在分离之后按簇累积语音（如 10 秒以上）再与注册声纹比较，短片段跟随所属的簇。簇整体匹配不证明每句话都来自佩戴者，误并入的他人短话会随簇升级，须专门评测。外放佩戴者本人的录音会被判成佩戴者，是已知残余风险。注册时采集多种状态并支持重新注册；“自动用高置信片段更新声纹”可能被投毒，需要单独评估。

### 5.3 目标说话人检测与近场特征

- [Personal VAD](https://arxiv.org/abs/1908.04284)（以注册声纹为条件逐帧判断目标说话人）：论文 2026-10-19 的 v7 附有作者从零实现的独立复现（[personal_vad](https://github.com/wq2012/personal_vad)，Apache-2.0）及 1.0、2.0 的预训练权重，复现结果在英文 MLS 数据上，所用说话人特征提取器与原论文不同。中文领夹的适用性与效果未经验证。
- 单麦本人说话检测的[助听器研究](https://arxiv.org/abs/2603.02724) 1 秒片段准确率约 90%，对领夹位置的适用性未验证。
- Ego4D 的佩戴者说话检测：早期方法只用音频（[arXiv 2210.07764](https://arxiv.org/abs/2210.07764)）；从约 80% 到 86% 的 mAP 提升来自音视频联合的图方法（[arXiv 2306.10608](https://www.arxiv.org/pdf/2306.10608v2)），不能作为单音频或声级特征有效的证据。

**研究判断（待测假设）：** 领夹贴近佩戴者的嘴，声级差与直达混响比可能比头戴设备更明显，是廉价可解释的辅助特征，但没有找到领夹单麦上的直接证据；电视近距离外放、他人贴近说话、佩戴者小声说话会破坏它，只能作加分或否决。Personal VAD 的开源复现可作探索对照，用中文佩戴数据评估后再决定是否微调或自训。

### 5.4 重叠

pyannote、3D-Speaker、Sortformer 能估计重叠；单通道中文的分离与目标说话人提取不成熟：[MossFormer2_SS_16K](https://huggingface.co/alibabasglab/MossFormer2_SS_16K) 没有公开性能数字，[WeSep](https://github.com/wenet-e2e/wesep) 自称研究预览、没有预训练模型。**研究判断：** 先检测重叠，重叠段不标佩戴者；不要指望靠分离还原重叠内容。重叠是他人内容混进佩戴者片段的主要来源之一，评测单独统计。

### 5.5 商用声纹

| 厂商 | 能力与限制 | 删除 |
|---|---|---|
| 腾讯 TRTC 声纹（[注册](https://cloud.tencent.com/document/product/647/135725)、[验证](https://cloud.tencent.com/document/product/647/135726)） | 注册与 1:1 验证都要 **10–45 秒**音频；阈值固定 60，不能改；单账号默认 1000 条；未提防重放 | [软删除](https://cloud.tencent.com/document/product/647/135730)，音频后台异步清理 |
| 讯飞声纹（[API](https://www.xfyun.cn/doc/voiceservice/isv_new/API.html)） | 特征库、1:1、1:N；价格未查到（仅搜索摘录） | 有删除接口 |
| 火山、阿里 | 未查到独立声纹产品 | — |

商用文件识别接口内置的声纹或角色认证见 [ASR 厂商调研](./2026-11-xinli-asr-vendors.md)。**研究判断：** 已核到的腾讯接口按“长段注册、长段验证”设计、删除为软删除，不能逐句判定；其他厂商的时长要求与删除方式须逐家确认。自建时声纹只是自己库里的向量，境内处理与删除更容易满足。

## 6. 识别后的后处理

### 6.1 幻觉

- 解码侧信号（Whisper 开源实现）：no-speech 概率与平均 logprob、gzip 压缩比过高判为重复（[transcribe.py](https://raw.githubusercontent.com/openai/whisper/main/whisper/transcribe.py)）。
- [Barański 等，ICASSP 2025](https://arxiv.org/abs/2501.11378) 在纯非语音上统计“幻觉词袋”，用于事后检测。
- 中文大模型识别在噪声下有重复与“幻听”（[arXiv 2408.09491](https://arxiv.org/pdf/2408.09491)）；[LCAR](https://arxiv.org/abs/2608.30776) 归纳的大模型识别幻觉类型有意外翻译、执行指令、重复、整段删除。
- 阿里 Paraformer 的文件识别输出没有置信度字段；火山未查到。

**研究判断：** VAD 先切段；对输出做重复、压缩比、字速、在 xinli 静音与噪声样本上统计的中文幻觉词表、置信度检测；命中只标记。删除条件以[转写方案](../design/xinli-transcription.md) §4.6 为准（加时间余量、排除不确定而整段送识别的区间），VAD 自身会漏检，过滤须与 VAD 联合评测。

### 6.2 标点与数字规范

- [WeTextProcessing](https://github.com/wenet-e2e/WeTextProcessing)（Apache-2.0，WFST 规则式中文 TN/ITN）；FireRedPunc（Apache-2.0）自报优于 FunASR 标点模型。
- 火山录音文件识别标准版（含 `volc.seedasr.auc`）的[官方参数表](https://docs.volcengine.com/docs/doubao-voice/large-model-recording-file-recognition-standard-version-api?lang=en)：`enable_itn`“默认为true”，`enable_punc`、`enable_ddc`（语义顺滑）“默认为false”（评审时核对原文；搜索摘录曾误称三者都默认关闭）。接入时显式设定 `enable_itn=false` 并检查实际输出，不依赖省略参数；阿里 Paraformer 的 `disfluency_removal_enabled` 默认关闭，**内置敏感词过滤默认开启**，会把词替换成 `*` 或删除（[Paraformer API，国际站](https://www.alibabacloud.com/help/en/model-studio/paraformer-recorded-speech-recognition-restful-api)）。

**研究判断：** ITN 有歧义（“一点”“两三个”），原文不做 ITN；若需要规范化的展示或检索文本，另存一层并能按字符对齐回原文。顺滑、去语气词、敏感词过滤一律关闭，并用含“嗯”“不是不是”“我是说”的音频验证；敏感词过滤能否关闭须与厂商确认，合规上需要过滤时在内容安全环节显式处理。

### 6.3 热词

阿里热词权重 1–5、建议从 4 开始，权重过高“会影响其他词的识别”（[自定义热词](https://www.alibabacloud.com/help/en/model-studio/custom-hot-words)）；Fun-ASR 报告了热词召回但没有讨论误插入；大模型识别会幻造列表中的热词（[Kong 等](https://arxiv.org/abs/2512.21828)）；评测用偏置词与非偏置词分开统计的错误率（[arXiv 2508.17796](https://arxiv.org/pdf/2508.17796)）。**研究判断：** 热词只取用户确认过的联系人与项目，控制规模与权重；评测必须包含热词未出现的负例。

### 6.4 大模型纠错

[ASR-EC（EMNLP 2025）](https://arxiv.org/abs/2412.03075)：中文上提示词纠错“无效，且常导致过度纠正，长句尤甚”，结合音频的方法最好。直接用上下文学习纠错常常提高错误率（[Naderi 等，Interspeech 2024](https://publications.idiap.ch/attachments/papers/2024/Naderi_INTERSPEECH_2024.pdf)）；只纠低置信句子可避免改坏正确句（[arXiv 2407.21414](https://arxiv.org/html/2407.21414v1)）。**研究判断：** 原始转写是唯一事实；纠错若做，只针对低置信片段、另存并带差异与版本，禁止改动否定、数字、日期、人名，评测单独统计“改对为错”的比例。

### 6.5 多系统融合

中文按字符级对齐的融合相对最佳单系统降低约 10% 字错率（[ISCSLP 2006](https://www.microsoft.com/en-us/research/?p=152213)），要求各系统准确率相当。**研究判断：** 全量双系统使识别成本翻倍，不建议；最多对低置信片段调用第二系统，不一致处只标记，不投票改写。

## 7. 一站式转写服务

| 服务 | 要点 |
|---|---|
| 阿里通义听悟 | 离线转写 0.6 元/小时（含说话人分离）；章节、摘要、口语书面化等大模型功能单独开关、单独计费；**身份识别是按文字描述推断角色，不是声纹**（[计费](https://help.aliyun.com/zh/tingwu/pricing-and-billing-rules/)、[身份识别](https://help.aliyun.com/zh/tingwu/identity-recognition)，仅搜索摘录） |
| 火山豆包语音妙记 | [文档目录](https://www.volcengine.com/docs/6561/1798094?lang=en)可见，参数未取得 |
| 讯飞、腾讯 | 未查到面向开发者的一站式会议纪要接口细节 |

**研究判断：** 一站式产品的价值在摘要与纪要，这一段 xinli 自己做；有用的只是“转写＋分离”，已被已调研的识别接口覆盖。听悟的身份识别让内容决定信任，不能用于佩戴者判定。可以作为只开转写与分离的商用参照配置。

## 8. 建议纳入评测的候选

| 环节 | 候选 | 用途 | 最先验证 |
|---|---|---|---|
| 编码 | PCM 对照 Opus 16/24/32 kb/s；领夹到手机的实际编码 | 确定上传配置 | 中文关键错误与短回应召回 |
| 增强 | GTCRN、DPDFNet（供 VAD 的变体）；ZipEnhancer、MossFormerGAN 及加回原始信号（送识别的变体） | 验证增强是否有益 | 各场景的关键错误、VAD 漏检与误检 |
| VAD | FireRedVAD（流式＋非流式）、Silero v6；FSMN-VAD 作高召回对照 | 送识别门控 | 短回应与远场召回、电视误检、计费时长 |
| 声音事件 | FireRedVAD 分类、YAMNet、PANNs；CED 须法务确认 | 标记与幻觉诊断 | Television 类在真实外放上的召回 |
| 识别（自建参照） | FireRedASR2-AED、Qwen3-ASR-1.7B；Paraformer-large 或 Fun-ASR-Nano 作低成本基线；Whisper 作幻觉对照 | 参照链路与自建评估 | 电视、远场、方言下的关键错误与幻觉；批量吞吐 |
| 说话人分离 | pyannote community-1、3D-Speaker、Sortformer；DiariZen 只作上限参照 | 匿名分组 | 自采佩戴数据上的表现；重叠检测 |
| 声纹 | CAM++、ERes2NetV2（簇级比对，可融合） | 佩戴者判定的主要依据 | 按时长分档的误识与误拒、跨天、领夹信道 |
| 近场特征 | 声级、相对佩戴者基线、混响估计 | 辅助或否决 | 电视近放、他人贴近、佩戴者小声 |
| 一体化参照 | MOSS-Transcribe-Diarize、通义听悟（只开转写与分离） | 对照 | 忠实度、幻觉、数据条款 |
| 后处理 | 幻觉规则与自建词表；热词负例 | 标记与过滤 | 误删、误插入 |

## 9. 许可证与合规待确认

- TEN VAD 的附加竞争限制条款。
- FunASR 模型协议（署名、禁止诋毁、可单方修订）与 HF 模型卡 Apache-2.0 不一致，以哪份为准。
- CED 的 GPL-3.0（服务端使用是否构成分发）。
- pyannote community-1 在 HF 门禁下离线部署的流程；NVIDIA Open Model License 的条件。
- DiariZen 权重不可商用。
- 商用声纹的软删除与物理清除时限、防重放能力，须书面确认。

## 10. 未决事项

1. 增强只给 VAD 用是否有净收益：只能自测。
2. 领夹到手机的编码已确认（Opus、16 kHz、20 ms、约 16 kb/s）；码率模式、FEC 与 DTX，以及固件 OTA 能否调整，仍待确认。
3. VAD 在领夹远场与短回应上的表现；FireRedVAD 声音事件分类的准确率。
4. 中文按时长切分的声纹误识与误拒，跨天、感冒、情绪的影响；近场特征在领夹单麦上能区分到什么程度。
5. 媒体外放检测没有可用的开源模型与单麦基准。
6. LLM 类识别（Qwen3-ASR、FireRedASR2-LLM、VibeVoice）在电视外放与非语音上的幻觉率。
7. 自建识别与说话人模型在境内 GPU/CPU 上的实际吞吐与单价。
8. 火山、阿里中文文档与通义听悟参数（口语书面化开关、保留期、删除），火山妙记的接口细节，须人工核对。
