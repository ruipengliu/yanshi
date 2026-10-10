# 调研：Agent harness 近期论文与开源项目，以及 System 1 决策模型

> 状态：调研笔记（2026-10-10），不是设计，不改变任何不变量 · 术语以 [CONTEXT.md](../../CONTEXT.md) 为准
>
> 结论里凡是要落地的，都须走评测（`make eval`）与 ADR；本文只给出候选与优先级。
> 引用的论文多为 2026 年预印本、未经同行评审；厂商材料（Jev）未经独立验证的部分已标注。

## 0. 摘要

1. **harness 研究的共识正在收敛到三件事**：上下文管理（尤其是"先规则省略、再模型摘要"）、按模型强弱裁剪 harness（规划、工具粒度）、以及把 harness 本身作为可优化对象（Meta-Harness、ACE、HarnessBridge）。yanshi 的"Event 日志 + 评测即基础设施"恰好是后者最需要的底座。
2. **开源项目里与 yanshi 最相关的不是编码 Agent 的 TUI，而是它们的运行时结构**：OpenCode 的 server/SDK 分离、OpenClaw 的"单输入队列 + 每 Session 串行"、Hermes 的磁盘检查点回滚、DeepSeek-Reasonix 的前缀缓存稳定性。yanshi 已覆盖前两项；后两项值得补。
3. **System 1 决策模型**（TypeSafe Jev、开源 Laya、学术版 SearchJev）把 harness 里的"小型类型化决策"从一次 LLM 生成变成一次前向传播输出的概率分布。独立评测显示它们在**工具选择、注入检测、事实一致性**上可用，在**模型路由、RAG 相关性、PII、作为唯一安全闸门**上不可用。
4. **对 yanshi 的直接约束**：Jev 只有托管 API，与私有化部署（ADR-0001）冲突；中文能力几乎没有证据。可行路线是 **SearchJev 式自建**：小开源模型 + 读取选项标签首 token 的 logits + 温度校准 + 置信度闸门回退到 LLM。建议先以"影子模式"在**调用结果的注入检测**上试点。

---

## 1. 论文

### 1.1 harness 组件的实证研究

| 论文 | 要点 | 对 yanshi 的含义 |
|---|---|---|
| *An Empirical Study of Harness Design for Coding Agents*（arXiv 2609.20804） | 4 个模型 × 176 组配置，在 SWE-Bench Verified、Terminal-Bench 2.1 上拆分消融。① 上下文管理的收益主要是**避免溢出导致的失败**，窗口越小越重要；② **先做规则省略、再做 LLM 摘要**最划算；"可找回被省略内容"增加了复杂度，模型却很少使用，也不提高准确率；③ 规划对弱模型提升准确率，对强模型主要是降成本；④ 擅长 bash 的模型用纯 bash 接口即可，且成本更低。 | Compaction 目前只有"截断单条 `ToolResult` + 模型摘要"两级（`internal/runtime/compact.go`）。可在摘要之前加一级**按规则省略旧调用结果**（只保留调用名、参数摘要、`artifact://` 引用与 seq），推迟昂贵的摘要调用。不必为"让模型找回原文"专门造工具——原始 Event 本来就在日志里。规划、工具粒度应当是 AgentDef 可选策略，并按模型分别评测（章程原则 2）。 |
| *Building Effective AI Coding Agents for the Terminal*（OPENDEV，arXiv 2603.05344） | 复合模型路由、规划与执行双 Agent、**工具惰性发现**、**渐进式压缩旧观察**、跨 Session 项目记忆、**事件驱动的系统提醒以对抗指令漂移**。 | 两项可直接借鉴：① MCP 工具多时惰性暴露（与 §3 的工具选择配合）；② 长 Run 中按条件（压缩后、恢复后、插话后）在模型上下文里重述关键约束——这是 Transcript 组装逻辑，不是新 Event，但改动给模型看的文本须评测。 |
| *OneDayAgent*（arXiv 2608.05013） | 把开放式请求变成受管的执行过程：拆成有界子任务、在上下文压力下维护执行记忆、**对最终交付物做校验与修复**。同一 harness 跨 5 个后端模型不调参，AgentIF-OneDay 0.821。 | "交付前校验 + 修复"对北极星场景 A（纪要整理 → 发送）有价值：在需要 Approval 的发送之前加一步自检，可降低用户拒绝审批的比例。可先做成一个 eval 用例衡量。 |
| *Recursive Agent Harnesses*（arXiv 2606.13643） | 递归单元是完整 harness（文件、执行、规划）而不只是模型调用；父 Agent 写脚本并行派生子 harness。同一 GPT-5 下，Codex 基线 71.75% → 81.36%（Oolong 长上下文）。 | 对应章程 G7"跨节点多 Agent 协作"。在 yanshi 里子 harness 的自然形态是**子 Session/子 Run**，天然继承租约、fencing、删除语义。暂不急，但设计多 Agent 时应以"Run 派生 Run"为原语，而不是在一个 Run 内开线程。 |

### 1.2 把 harness 当作可优化对象

| 论文 | 要点 | 对 yanshi 的含义 |
|---|---|---|
| *Meta-Harness*（Lee, Khattab, Finn 等，arXiv 2603.28052） | 外层循环搜索 harness **代码**：一个 Agent 提议者从文件系统读取历史候选的源码、分数与**完整执行轨迹**；认为现有文本优化器把反馈压缩得太狠。在文本分类上 +7.7 分且上下文 token 少 4 倍；TerminalBench-2 上超过手工 harness。 | yanshi 已具备三要素：可重放的 Event 日志（轨迹）、固定评测集 + 基线（分数）、AgentDef 版本 + 灰度（安全上线）。可以把"提示词 / Compactor / 召回格式"的改进做成离线外循环，**只使用评测集轨迹，不用生产 Session 数据**（ADR-0015），产出的候选仍走 `make eval` 与灰度（ADR-0020）。 |
| *ACE: Agentic Context Engineering*（ICLR 2026） | 把上下文当作不断演进的"playbook"：Generator 产轨迹、Reflector 提炼、Curator **按条目增量合并**（而非整体重写），避免"简短化坍缩"。DeepSeek-V3.1 在 AppWorld 上追平榜首 GPT-4 Agent。 | 增量条目合并的思路可用于 Compaction：摘要按条目追加/修订，而不是每次全量重写，可减少"已执行的副作用被摘要丢掉"的风险（ADR-0012 的核心担忧）。AgentDef 级的 playbook 是新概念（不同于按 EndUser 归属的 Memory），引入前需在 CONTEXT.md 定义并写 ADR。 |
| *HarnessBridge*（arXiv 2606.12882） | 可训练的 harness 控制器：观察投影（把历史压成紧凑状态）+ 动作投影（把提议动作转成可执行步骤，或基于轨迹拒绝）。token 与轨迹长度显著下降，可从小模型迁移到大模型。 | 方向上与 §3 的 System 1 一致：把 harness 中的判断交给小模型。yanshi 不训练模型（章程非目标），但可以接入他人训练的此类组件。 |
| *DualSpec*（arXiv 2603.07416） | 深度研究 Agent 中，Search 决策不确定性高、需要推理；Visit 决策熵低。对低熵动作做**投机执行**，用轻量置信度校验器把关，端到端最高 3.28× 加速且准确率不降。 | 只读、幂等的 Capability（ADR-0017 中声明只读的工具）可以投机预取；有副作用的不行。需要在 `internal/sim` 中证明投机结果被丢弃时不留痕迹，复杂度高，排在后面。 |

综述入口：RUCAIBox *Agent Systems with Harness Engineering* 阅读清单；RUC-NLPIR *Awesome-Long-Horizon-Agents*（含 2026 综述 *Externalization in LLM Agents*）。

## 2. 流行开源项目（运行时结构视角）

星数取自 best-of-Agent-Harnesses 在 2026-10-04 的快照，各来源差异较大，仅作量级参考。

| 项目 | 规模 | 值得借鉴的结构 | yanshi 现状 / 建议 |
|---|---|---|---|
| **OpenClaw** | ~391k | 常驻 Gateway 作控制面；消息、心跳、定时、webhook 进**同一输入队列**；**每个 Session 一条串行通道**，防止对话记录被并发写坏；技能以 `SKILL.md` 分发。 | 已等价覆盖：Session 内至多一个未终态 Run + 乐观并发 Commit。它的安全问题（特权 shell、宽文件访问）正是 ADR-0008 沙箱边界要避免的。定时/心跳类输入可作为未来"Run 触发源"的参考。 |
| **Hermes** | ~251k | 把经验沉淀为可复用技能；持久用户模型；**磁盘检查点 + 回滚**；模型中立。 | `snapshottest` 只是 Session 投影快照，沙箱 workspace 目前没有检查点。可考虑为 workspace 增加按 Event 对齐的检查点（属于 Session 数据，须能被 Janitor 删除），让 EndUser 在 Approval 被拒或结果不满意时**回滚 workspace 到某个 Event 对应的快照**，这在 Event 日志模型下很自然。 |
| **OpenCode** | ~212k | `opencode serve`：无头 HTTP 服务 + OpenAPI 3.1 + SSE 事件流，TUI/Web/IDE 都只是客户端；从 OpenAPI 生成官方 SDK。 | 结构一致（契约在 `proto/`，多端订阅事件流）。可借鉴：**从契约自动生成多语言客户端 SDK** 并纳入 `make gen`。 |
| **Codex CLI / Gemini CLI / Pi / oh-my-pi** | 107k–128k | 沙箱化工具调用循环；Pi 主打极简、省 token；oh-my-pi 的**哈希锚定编辑**（编辑以内容哈希定位，防止并发/过期编辑）与长驻执行内核。 | 哈希锚定编辑适合 Device 上的文件写入 Capability：与"按 call_id 去重"（`nodesdk.Executor`）互补，防止基于过期内容的写入。 |
| **OpenHands** | ~90k | Docker 沙箱、事件流驱动的会话、micro-agent。 | 与 yanshi 最像的开源参照；其 event stream 是进程内的，yanshi 的是持久化事实源，更强。 |
| **DeepSeek-Reasonix** | ~36k | 专门为长会话的**前缀缓存稳定**调优。 | **值得立即关注**：yanshi 默认用 DeepSeek 系模型，小时级 Run 的成本大头是重复前缀。Transcript 组装应保证系统提示、Recall、工具定义的字节顺序稳定，只在尾部追加；每次 Compaction 都会使缓存失效，压缩阈值应把缓存命中率算进去。建议在 `model.Usage` 里区分缓存命中 token 并接入计量（ADR-0019）。**已确认 TokenHub 报告缓存命中**：同一长前缀连续两次请求，第二次 `prompt_tokens_details.cached_tokens` 为 2688/2743（2026-10-10 实测）。 |
| **Prime Agent** | ~22k | daemon 托管的 Session 在终端关闭后存活；按轮数/token/时间的预算；Agent 可编辑自己的提示词与技能。 | 预算：AgentDef 已有预算概念，按时间/费用的 Run 预算是 m2-long-runs 明确未做的项。"自改提示词"在 C 端多租户下风险高，只应在离线外循环（§1.2）中做。 |

## 3. System 1 决策模型

### 3.1 是什么

借用 Kahneman 的双过程理论：**System 2** 是慢速、逐 token 推理的 LLM；**System 1** 是对"给定状态 + 有限合法选项"的问题，**一次前向传播直接输出各选项概率**的模型，不生成文本、不解析 JSON。harness 里大量判断本质上是分类：选哪个工具、调用结果里是否有注入、回答是否有依据、是否需要人工确认……用一次完整 LLM 调用做这些判断又慢又贵，且置信度不可用。

| 模型 | 形态 | 已知情况 |
|---|---|---|
| **Jev**（TypeSafe AI，jev-1.13，2026-09） | 托管 API；非自回归 | 输入：文本 state + 一组类型化问题（Choice：选项键 → 描述；Yes/No：返回 p(yes)）。输出：每题的概率分布，置信度 = 最大概率。独立评测测得 p50 延迟 83–325 ms（取决于网络路径），每次决策约 759 token，约 $32 / 百万次决策。厂商宣称"比 LLM 快约百倍"、用强化学习训练校准，**未经独立验证**。演示多为游戏、无人机仿真。 |
| **Laya** | 开源（Apache-2.0 权重，`pip install laya`） | 英文与多语言两个 checkpoint；RTX 2080 Ti 上 p50 31 ms，M4 CPU 上 200 ms。对选项顺序敏感（反转顺序改变 30% 答案），候选多且相似时显著退化。 |
| **SearchJev**（arXiv 2610.05107） | 学术，Qwen3.5 0.8B / 4B + LoRA | 每个决策是 schema 定义的有限选项问题；**读取首个输出位置上单 token 选项标签（A/B/…）的 logits，在合法选项上归一化**；训练用软标签 + 交叉熵 + Brier 损失；训练后**按输出类型拟合温度**做校准。置信度 ≥ δ 则采用，否则交给 System 2 回答同一问题。单次决策 28–37 ms，比同尺寸自回归 JSON 快 5.2–5.3×；BrowseComp-Plus 上准确率 45% → 54%（4B），活跃时长缩短 3.7–4.7×（仅 100 题、单次运行）。 |

### 3.2 独立评测：哪里能用，哪里不能

*Fast Models, Slow Evidence*（arXiv 2610.02267）在 11 个决策点上做了配对、可复现的评测（7,283 基础用例 + 6,640 鲁棒性变体；LLM 参照恰好是 DeepSeek-V4.1-Flash，与 yanshi 开发所用模型同系）：

| 决策点 | Jev | Laya | 结论 |
|---|---|---|---|
| 工具选择（随机干扰项） | 99.5% | 84.0% | 可用（LLM 参照 98.5%） |
| 工具选择（相似干扰项） | 84.5% | 54.0% | Jev 与 LLM（83.0%）相当 |
| 大标签集意图分类（60+ 类） | 81.0% | 35.0% | Jev 可用 |
| 注入检测 | AUC 0.98 | 0.88 | **可用**；来自调用结果通道时 0.97 |
| 事实一致性（groundedness） | AUC 0.95 | 0.79 | 可用 |
| 调用前守卫 / 执行前安全 | AUC 0.86 / 0.77 | 0.56 / 0.64 | 只能作辅助信号 |
| PII 外发 | AUC 0.81 | 0.41 | **不可用**（LLM 0.85） |
| RAG 相关性闸门 | 61.0% | 60.5% | **不可用** |
| 零样本模型路由 | 随机水平 | 随机水平 | **不可用** |

作者对自己第一版结论的"自审"比结果本身更有参考价值：

- 漏算前置筛查的成本，宣称节省 23.9%，实际 4.3%；更严格的阈值甚至比直接调用审查者更贵。
- 把闸门准确率（45–58%）误当成端到端质量（实际约 98% 的可答问题仍被回答）。
- 阈值在样本内调出：目标漏检 5%，留出集上达到 17%。
- "快 10 倍"只在一条网络路径上成立。

**中文证据几乎为零**：唯一的中文数据是 MASSIVE 中文子集的 10 条（Jev 50%、Laya 30%），只能看趋势。

### 3.3 在 yanshi 中的应用场景

按"收益 × 可行性"排序。所有场景共同遵守的规则见 §3.4。

| 优先级 | 场景 | 现状 | System 1 的角色 | 证据 |
|---|---|---|---|---|
| **P1** | **调用结果、文件、网页中的提示注入检测** | 章程原则 6 把外部输入视为不可信；有 `prompt-injection-file` 评测用例，但运行时没有检测 | 每条 `ToolResult` 写日志前打分；高分时在模型上下文中加隔离标记，或让后续高风险 Capability 强制 Approval。**只能收紧，不能放宽** | 最强（AUC 0.97–0.98） |
| **P1** | **MCP / Capability 的工具选择与惰性暴露** | AgentDef 的全部 Capability 一次性给模型 | 每步从几十上百个工具中选出 top-k 暴露给模型，置信度低时全量暴露 | 强（84–99%）；与 OPENDEV 的惰性发现一致；同时减少前缀长度 |
| **P2** | **交付前的事实一致性检查** | 无 | 在需要 Approval 的发送类 Capability 前，检查拟发送内容是否有上下文依据；不一致时让 System 2 自检修复（OneDayAgent 式） | 较强（AUC 0.95） |
| **P2** | **Call 中的快速判断**（章程原则 5 快慢分离） | Call 派生 Run | 用户这句话是闲聊、追问进度、插话（Steer），还是需要派生新 Run；是否打断播报 | 无直接证据，但延迟预算（百毫秒级）最契合 System 1；**需要中文领域数据自训** |
| **P3** | **审批辅助** | 未声明只读的工具默认需审批（ADR-0017） | 只能对"声明只读但看起来有副作用"的调用**追加**审批，永不免除审批 | 中（AUC 0.77–0.86） |
| **P3** | **Moderation 预筛** | 提供商可替换，失败即拒绝（ADR-0021） | 只做便宜的预筛来跳过明显安全的内容——但自审显示实际节省可能仅约 4%，且合规审核通常须用有资质的提供商 | 弱；**不得作为唯一闸门** |
| ✗ | 模型路由（快答 vs 长任务、便宜 vs 强模型） | — | 零样本不可用；除非用自有数据训练 | 随机水平 |
| ✗ | Memory Recall 相关性过滤 | — | 不可用 | 约 61% |
| ✗ | PII 检测（日志脱敏、Memory 敏感类别） | ADR-0022 健康类 Memory 需同意 | 不可用 | AUC 0.41–0.81 |

### 3.4 若引入，必须满足的约束

1. **私有化部署（ADR-0001）**：Jev 只有托管 API，不能用于客户 IDC，除非厂商提供私有化版本。可行路线是 **SearchJev 式自建**：自托管小开源模型（如 Qwen3.5 0.8B/4B），读取选项标签首 token 的 logits。这在**不训练**的情况下也能先做原型（只需推理端点支持 `logprobs` / `top_logprobs`），训练（LoRA）留给效果不够时再做。注：`internal/model` 当前没有 logprobs 支持；**已确认 TokenHub 不返回 logprobs**（2026-10-10 实测 `deepseek-v4.1-flash`，`logprobs: true` 时返回 `null`），因此这条路线需要自托管推理端点（如 vLLM 部署的小模型）。
2. **确定性（Worker.Step）**：System 1 的结果与模型调用一样不确定。凡影响后续执行的判断，必须像 Recall（ADR-0016）、Compaction（ADR-0012）那样**先作为 Event 写入日志，再据此行动**，重放时读日志而不是重新调用；并在 `internal/sim` 中注入"判断调用失败/超时"故障。
3. **失败即保守**：System 1 不可用时退回 System 2 或退回"不做优化"的默认路径（全量暴露工具、按原规则审批），绝不能因为分类器挂了而放行。
4. **个人数据（ADR-0015）**：判断的输入含用户内容，只能存在可按 Session 删除的地方；日志、指标只记 ID 与分数分桶。
5. **计量（ADR-0019）**：每次判断都是一次计量的模型调用。按自审的教训，评估节省时要计入**全部组件成本**。
6. **评测纪律**：阈值 δ 只在留出集上确定；报告调用率（被 System 1 处理的比例）与留出集漏检率，而非闸门准确率；测选项顺序扰动；**建立中文用例**（`evals/` 下新增，按决策点分组）。
7. **术语**：若采纳，"System 1 判断"是新概念，先在 CONTEXT.md 定义（候选名：Decision / 判断），并写一条 ADR 说明"判断是 Event、只收紧不放宽、失败退回默认路径"。

### 3.5 建议的第一步

在**影子模式**下做 P1 注入检测：Worker 对每条 `ToolResult` 调用自托管小模型打分，分数作为 Event 字段记录，但**不影响执行**；用现有 `prompt-injection-file` 用例加一批中文注入样本（网页、文档、设备文件、MCP 返回）离线评测；收集一到两周的分数分布和延迟后，再决定是否让它驱动"强制 Approval"。

## 4. 其他可落地的 harness 改进（不依赖 System 1）

| # | 改进 | 依据 | 涉及 |
|---|---|---|---|
| 1 | Compaction 之前加"规则省略旧调用结果"一级 | 2609.20804 | `runtime/compact.go`、Transcript；须评测（`compaction-recall` 等用例） |
| 2 | 前缀缓存友好的 Transcript 组装 + 计量缓存命中 token | DeepSeek-Reasonix | Transcript、`model.Usage`、ADR-0019 |
| 3 | 压缩、恢复、插话之后在上下文中重述关键约束 | OPENDEV | Transcript；新增长 Run 漂移用例 |
| 4 | 发送类 Capability 前自检 | OneDayAgent | AgentDef 策略；场景 A 用例 |
| 5 | 基于评测轨迹的离线 harness 外循环 | Meta-Harness、ACE | `internal/eval`；只用评测数据 |
| 6 | 规划、工具粒度按 AgentDef 与模型可配，分别评测 | 2609.20804 | AgentDef |
| 7 | 增量条目式摘要（替代整体重写） | ACE | Compactor 实现；ADR-0012 关注的副作用保留 |

## 5. 来源

论文

- [An Empirical Study of Harness Design for Coding Agents (arXiv 2609.20804)](https://arxiv.org/abs/2609.20804)
- [Building Effective AI Coding Agents for the Terminal (arXiv 2603.05344)](https://arxiv.org/abs/2603.05344)
- [OneDayAgent (arXiv 2608.05013)](https://arxiv.org/abs/2608.05013)
- [Recursive Agent Harnesses (arXiv 2606.13643)](https://arxiv.org/abs/2606.13643)
- [HarnessBridge (arXiv 2606.12882)](https://arxiv.org/abs/2606.12882)
- [Meta-Harness (arXiv 2603.28052)](https://arxiv.org/abs/2603.28052)
- [DualSpec (arXiv 2603.07416)](https://arxiv.org/abs/2603.07416)
- [SearchJev (arXiv 2610.05107)](https://arxiv.org/abs/2610.05107)
- [Fast Models, Slow Evidence (arXiv 2610.02267)](https://arxiv.org/abs/2610.02267)，代码与数据：github.com/David-DL-Space/sys1-eval
- ACE（ICLR 2026）介绍：[Towards AI](https://pub.towardsai.net/agentic-context-engineering-ace-456337c0b0ea)
- 阅读清单：[RUCAIBox/awesome-agent-harness](https://github.com/RUCAIBox/awesome-agent-harness)、[RUC-NLPIR/Awesome-Long-Horizon-Agents](https://github.com/RUC-NLPIR/Awesome-Long-Horizon-Agents)

开源项目与厂商材料

- [best-of-Agent-Harnesses（星数快照）](https://github.com/RyanAlberts/best-of-Agent-Harnesses)
- [OpenCode server 文档](https://www.mintlify.com/anomalyco/opencode/server)
- [OpenClaw Architecture — Part 1](https://theagentstack.substack.com/p/openclaw-architecture-part-1-control)
- [Jev 介绍（MindStudio）](https://www.mindstudio.ai/blog/jev-system-one-model-launch)、[TypeSafe Jev 发布文（Friday）](https://www.tryfriday.ai/blog/typesafe-jev-system-one-launch)、[System 1 and System 2（Joche Ojeda）](https://jocheojeda.com/2026/09/25/system-1-and-system-2-ai-model-types/)
