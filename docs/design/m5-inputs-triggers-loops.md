# 输入、触发与外部循环

> 状态：已确认（2026-10-25）；第 1 步已实现，第 2 步（流上的触发）由 ADR-0050 取消，第 3 步（外部循环）已实现（2026-11-25，见 ADR-0062、ADR-0063） · 依赖：[M0 核心原语](./m0-core-primitives.md)、[旁听](./captures.md)、[旁听业务线 xinli](./xinli-captures.md)、[Memory 与 Grant](./m4-memory-grant.md)、[模型调用](./model-calls.md) · ADR-0039、ADR-0040、ADR-0041 · 术语：`CONTEXT.md` 的 Provenance、Stream、Trigger、Loop
>
> 2026-11-14：ADR-0036、ADR-0039、ADR-0050 与 ADR-0037 的内核部分已整合进 [ADR-0052](../adr/0052-attested-results-and-provenance.md)，ADR-0040 已取消，ADR-0010 已被 [ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 取代。本文引用这些 ADR 之处，现行规则以 ADR-0052、ADR-0053 为准（[ADR 索引](../adr/README.md)）。

ADR-0050 后，内核保留来源推导、统一检查点与外部循环的边界设计。原文、时段与处理进度由业务线负责，流与流上触发已取消。

## 1. 目标

各入口共享平台专属字段校验、内容安全与准入。写入闸门和注入判断只依据平台从事件推导的来源，不接受调用方自报来源。

## 2. 边界

用户输入、通话转写和设备结果经 service；可信页在 Worker 的同步调用路径中校验、审核、渲染后落日志。Agent 的业务编排留在业务线。

## 3. 来源（Provenance）

**定义。** 来源说明一段内容由谁产生、是不是对 Agent 说的：

| 来源 | 举例 | 写入 Memory 的依据 | 注入判断 |
|---|---|---|---|
| 本人 | 用户的输入、对提问的回答、通话中用户说的话、旁听中佩戴者的话 | 可以 | 否 |
| 他人 | 旁听中他人的话 | 不可以：上下文含外部内容 | 是 |
| 外部 | 设备、沙箱、MCP、委托的调用结果，界面上下文，后台调用迟到的结果 | 不可以：上下文含外部内容 | 是 |
| attested 结果 | 同业务线可信登记的 MCP 结构化页 | 佩戴者片段可以，他人片段不可以 | 他人片段合成一段判断；平台页头不判断 |
| 平台定时 | 带 `RunRequested.schedule` 标记的输入（`session.FromSchedule`） | 不是用户依据，只凭它须审批 | 否，不是外部内容 |
| 平台 | 平台写的说明（分页提示、拦截占位）、触发写入的输入（§6）、卡片 | 不可以，但不算外部内容 | 否 |
| 模型 | Agent 的回复与调用（含外部循环给出的，§7） | — | — |

依据与判断的规则与现在相同（[Memory 设计](./m4-memory-grant.md)、ADR-0023、ADR-0030、ADR-0036），这里只是把它们集中到一处。

**由平台推导，不由写入方填写。** 来源是"事件 + 内容块"的纯函数，放在 session 包中，例如：带 `schedule` 标记的 `RunRequested` → 平台定时；普通 `RunRequested` 中用户直接提交的内容 → 本人；名称含 `__` 的普通调用结果 → 外部（attested 结果除外）；attested 结果中的旁听片段 → 按 `speaker` 字段分为本人或他人。不在每个内容块上存一个来源字段：存储的字段就可以由写入方自报，这正是要防的。唯一推导不出、需要写入的是旁听的说话人，现在已经是结构化字段（`CaptureSegment.speaker`），且只能由平台写入 attested 标记的 MCP 结果写入（`Apply` 校验）。

**谁用它。** 写入闸门（依据与外部内容）、注入判断（哪些结果待判断、交给判断的是哪些文字）、`cite_captures` 附上的说话人、给模型看的标注（`model.CaptureText` 中的 `[佩戴者]`、`[他人 other-1]`）。新增一种输入时，只在推导函数里登记它的来源，闸门与判断自动适用。

**第 1 步的验收。** 规则不变，只是集中：给模型看的文本不变，闸门与判断的单元测试原样通过，`make eval` 与基线一致。

## 4. 统一检查点

所有写进日志的入口按同一顺序做同一组检查，每个入口声明自己用哪几步：

| 入口 | 拒绝平台专属内容 | 去重 | 规范化 | 内容安全 | 准入与配额 | 写入后 |
|---|---|---|---|---|---|---|
| 用户输入（含卡片动作） | ✓ | `input_id` | 媒体、设备标注 | 整条 | 新 Run 时 | 开启 Run、插话或回答提问 |
| 定时创建 | 只收纯文字 | schedule_id，相同声明幂等 | 时区默认取 Session，规则规范化 | 整条 | 不检查，到点 Run 自行处理 | 原子唤醒，重新计算停放 |
| 通话转写 | — | — | — | 整条 | — | 只追加 |
| 设备结果、回答、审批 | 已有的校验 | `call_id` | — | 照旧 | — | 唤醒 Session |

- **"平台专属内容"** 是只能由平台写入的内容块与字段：`background_result`、`capture`、卡片，可信页的 attested 标记，以及 `RunRequested.schedule`、`RunRequested.redo_of`。客户端提交中带有它们一律拒绝，`Apply` 再校验一次；重答只能经 `Service.Redo`，平台复制原输入并校验资格、逐块一致和无平台来源。重答不改变来源推导，安全检查仍看到被替代 Run 的外部内容。
- **实现方式。** service 中的一组共用步骤，按上表组合，不是一个通吃的函数；它们最后都经 `session.Store.Update` 决定并追加，"先校验、再乐观并发追加"不变。
- **输出走同一套规则。** 模型或外部循环（§7）的回复，在写日志之前做内容安全检查（已有，ADR-0021）；调用按能力白名单与审批执行；`memory_save` 经过写入闸门，闸门按来源（§3）判断。检查点不区分输出来自哪种循环。

## 5. 流与读取位置（已退役）

按 ADR-0050，内核不再声明流、接收原文或记录读取位置；分页与处理进度由业务线管理。AgentDef 中的 streams 为未知字段，加载时报错。

## 6. 触发（已取消）

流上静默触发不再实施。业务线判断静默后提交固定整理输入，动态原文经可信页读取。

## 7. 外部循环（Loop）

> **2026-11-24**：本节的契约由 [ADR-0062](../adr/0062-external-loop-contract.md) 修订，冲突处以它为准。已改动的段落：声明（`loop` 与 `model` 互斥）、重复请求（不得按三个字段去重）、登记（只接业务线自己运营的服务）；新增"平台侧的保证"与一致性套件，见 ADR-0062 第 5、8 条；下面的 proto 草案只是历史记录，现行契约是 `proto/yanshi/v1/loop.proto`（已补 `state_version`、自报用量、"上一步"与结果的 `is_error`），实现在 `internal/extloop`，进度见 ADR-0062 开头。

> **2026-11-25**：外部服务的模型调用由 [ADR-0063](../adr/0063-loop-model-gateway-and-step-token.md) 纳入内核：`loop.models` 声明了模型的 AgentDef，外部服务经内核的模型端点调用模型，每步请求带步令牌。"声明""登记""内容安全与计量"三段已按它修订。

**声明。** AgentDef 的 `loop` 指定由谁决定每一步做什么，不写时就是现在的内置循环。`loop` 与 `model` 互斥；需要平台压缩时配 `context.summary_model`（2026-11-24，ADR-0062 第 6 条）：

```yaml
loop:
  server: xinli-loop        # loops/*.yaml 中的登记
  models: [ark/doubao-seed-2-1-260915]  # 可选：经内核模型端点可用的模型（ADR-0063 第 3 条），评测换模型时替换
context:
  summary_model: ark/doubao-seed-2-1-lite-260915:minimal  # 只在需要平台压缩时
```

登记与远程 MCP 相同（`extloop.Server`，目录由 `serve -loops` 指定，默认 `loops/`）：`name`、`business_line`、`url`、`headers`（`${VAR}` 从环境变量注入）、`timeout`、`token_keys`（签发步令牌的 HMAC 密钥，`kid` 与 `secret`，同样从环境变量注入；2026-11-25，ADR-0063 第 2 条）。启动时核对 AgentDef 与登记（`cmd/yanshi/loops.go`）：AgentDef 指向的循环在任何业务线都没有登记、业务线按名称允许了 AgentDef 却没有登记它的循环、声明了 `loop.models` 的 AgentDef 指向的登记没有 `token_keys`，都使启动失败。只有同一业务线的 Session 能用这个循环，否则 Run 以配置错误失败。登记的服务须由该业务线自己运营；三方开发的 Agent 经出站能力接入（2026-11-24，ADR-0062 第 7 条）。

**每一步一问。** 内置循环中"调用模型"的那一步，换成调用业务线的服务；其余步骤不变。平台照常组装上下文（含召回与压缩）、照常执行调用。契约在 proto 中定义（拟新增 loop.proto），经 HTTP 以 JSON 传输：

```proto
message LoopStepRequest {
  string session_id = 1;
  string run_id = 2;
  uint32 attempt = 3;
  uint32 turn = 4;
  string end_user = 5;
  AgentRef agent = 6;
  string system = 7;                  // AgentDef 的指令
  repeated LoopMessage messages = 8;  // 与模型请求相同的上下文视图（经压缩）
  repeated ToolSpec tools = 9;        // 本 Run 可用的工具，按名称排序
  bytes state = 10;                   // 上一步返回的 state；Run 的第一步为空
}

message LoopStepResponse {
  repeated ContentBlock content = 1;  // 回复
  repeated ToolCall tool_calls = 2;   // 为空表示 Run 完成
  bytes state = 3;                    // 至多 64 KiB，随 AssistantMessage 写入日志，下一步原样带回
}
```

**语义：**

- **循环只做决定，不做事。** 它只返回回复与调用请求；对用户设备、Memory 与其他能力的一切影响，都经平台执行的调用发生，审批、写入闸门、能力白名单照常生效。外部服务在自己的系统里做什么，平台管不到，这与业务线的 MCP Server 相同（ADR-0017）。
- **确定性。** 返回的结果作为 `AssistantMessage` 写入日志，回放读日志，不再调用外部服务，`Worker.Step` 的确定性不变。
- **重复请求。** Worker 在写入之前崩溃或被接管时，同一步会以相同或递增的 `attempt` 再请求，两次的回答可以不同，以写入日志的那次为准。外部服务不保存跨步状态，也不得按 `(run_id, attempt, turn)` 去重或缓存回答（2026-11-24，ADR-0062 第 2、3 条：接管时 attempt 递增，键对不上；缓存的旧回答会错过插话）。
- **状态。** `state` 是外部服务给自己的不透明数据，存在日志里、随 Session 删除，外部服务因此可以不保存状态。它不进入模型上下文，也不出现在客户端的事件流中。
- **失败与时限。** 按模型调用的规则处理（[模型调用](./model-calls.md)）：暂时性失败本步重试，同一 Attempt 连续失败超过 `MaxModelErrors` 使 Run 失败；不合法的回答（调用指向请求以外的工具、状态超限、无法解析）同样计入，且不写日志（2026-11-24，ADR-0062 第 4 条）。流式按 ADR-0056 B 期与外部循环一并实施，已输出增量的步不在连接层重试（原"第一版不流式"不再成立）。
- **内容安全与计量。** 回复在写日志之前检查，与模型输出相同。用量按"步"计（`Kind="loop"`，价格表 `loops:` 以 `<业务线>/<名称>` 为键）；`max_turns` 照常限制步数。外部服务经内核模型端点的调用另按模型计量（2026-11-25，ADR-0063 第 4 条，见下一条）；不经端点的调用平台看不到，自报的用量不记录（ADR-0062）。
- **内核模型端点与步令牌（2026-11-25，ADR-0063）。** 登记配置了 `token_keys` 时，每步请求带 `step_token`：以登记的第一把密钥签发的 HS256 JWT，声明业务线、循环、Session、Run、Attempt、Turn、EndUser 与 AgentDef，有效期是这一步的时限（`extloop.SignStep`）。外部服务把 base URL 配成 `<yanshi>/v1/loop`、以步令牌为 API key，按 OpenAI Chat Completions 调用模型（`extloop.Endpoint`）：
  - 只接受有效的步令牌（不经业务线令牌的中间件），只看签名、有效期与删除记录（Session 或 EndUser 已删除时 404 `session_deleted`，调用返回后再查一次），不核对 Attempt 是否当前；调用与读请求体都不超过令牌的有效期。有效期不得长于登记的一步时限，EndUser 不得为空，令牌中的 AgentDef 须属于令牌的循环、且业务线允许它（2026-11-25 评审补充）。
  - 合计并发上限 `-loop-model-concurrency`（默认 8，须小于提供商并发上限），每步至多 2 个并发调用，排队超过 2 秒返回 503 `busy`；部署要点见[部署](./deployment.md) §9a。
  - 只接受 AgentDef 的 `loop.models` 中的模型；经模型网关调用，准入（后台 Run 的步沿用后台类别）、限流、重试与内置循环相同。
  - 调用前检查配额，超出时 429 `quota_exceeded`（与提供商限流的 `rate_limit_exceeded` 区分，外部服务不应在这一步内重试）；下一步之前 Worker 的检查让 Run 挂起。调用返回后按模型计量（`Kind="model"`，记在令牌声明的业务线、EndUser 与 AgentDef 下，含缓存命中），价格表须有 `loop.models` 中模型的价格。
  - 不做内容安全检查：模型的输出只经这一步的回答到达用户与日志，回答照旧在写日志前检查。
  - 媒体只接受内联数据（data: URL 与 base64；图片 png、jpeg、webp、gif，音频 wav、mp3、m4a），不转发远程地址；`temperature` 等采样参数与 `max_tokens` 之外的输出控制忽略。`max_tokens` 由外部服务给出，平台不另设上限：配额在调用前检查，并发上限约束同时进行的调用能超出多少。
  - 每步请求的 `models` 是 `loop.models`（评测时是评测的模型）：外部服务从中选择、默认用第一个，不要写死。
  - `looptest -step-token`（`looptest.Config.TokenKey`）在每步请求中签发步令牌、给出 `models`，检查服务把令牌转给了模型调用、用的是给出的模型；声明了 `loop.models` 的服务须这样通过套件（ADR-0063 第 6 条）。
  - 评测以 `yanshi eval -loop 名称=URL` 驱动正在运行的外部服务（[评测](./m4-eval.md) §3）。
  - 参考适配：`sdk/python`（LangGraph 与 Pydantic AI，无状态、按上面的方式调用模型），`make looptest-adapters` 对它们运行 `looptest -step-token`。
- **数据离开平台。** 请求带着完整的上下文，包括旁听中他人的话与设备的结果，会到达业务线的服务。MCP 调用只带参数，外部循环看到的范围大得多：业务线要承担保留与删除的义务，不应保存超出 `state` 的副本。这一点写进接入文档，由业务线在登记时确认。外部地址只来自显式的登记，私有化部署时可以不登记（ADR-0001）。
- **不变的部分。** 前缀缓存由外部服务的框架负责：经内核端点的调用上报缓存命中，评测按 Session 计入报告的"缓存命中"列，但平台不保证前缀稳定（ADR-0063 第 4 条）。实时增量随 NDJSON 流式一并提供（ADR-0056 Q7），草稿不进日志，整条回复写日志前审核。

**为什么是"每步一问"，而不是把整个 Run 交出去。** 把整个 Run 交给外部（A2A 式），外部 Agent 自带工具、自己执行，平台的审批、写入闸门、内容安全与删除都管不到它的内部，平台只能把最终结果当作外部内容。每步一问让外部服务只决定"下一步做什么"，而所有动作仍经过统一检查点（§4）。代价是每一步往返一次，延迟比内置循环高，适合复杂流程，不适合闲聊。A2A 作为另一种能力，以后另行设计（§12）。

## 8. 容量与成本

原文存储和分页在业务线；内核日志只保存实际读取、经审核的页副本。重读需要重审，副本随工作 Session 删除，容量见 ADR-0050。

## 9. 不变量核对

Event 仍是唯一事实源。可信页只由 Worker 校验、审核后写入，Apply 再校验来源标记；输入、设备结果不得伪造片段。Worker.Step 保持确定性的单步，来源推导只依赖日志。

## 10. 测试与评测

保留 attested 来源、写入闸门、注入判断及模拟故障覆盖。Persona 分页与冻结 golden 对比；流、内核提交、读取位置和回执的测试已删除。外部循环的超时、状态与回放验证仍按 §7 实施。

## 11. 兼容与实施

内核 Capture 路径已退役，旧 CaptureAppended 与读取位置、回执消息的定义也已于 2026-11-15 删除（ADR-0052），退役之前的日志不再回放。删除的传输和快照字段保留编号与名称，不复用，使用 WIRE_JSON 检查兼容性。外部循环见 §7。

## 12. 不在这一版

外部循环的流式增量已由 ADR-0056 B 期与 ADR-0062 定稿（2026-11-24）；平台模型代理、沙箱循环及工具结果只存引用仍待独立设计。流上的触发、采集泛化与内核原文存储已由 ADR-0050 取消。
