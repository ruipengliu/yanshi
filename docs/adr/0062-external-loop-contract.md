---
status: accepted（2026-11-24 定稿；部分实现）
---

# 外部循环的契约：自定义协议、无状态单步与一致性套件

> **实施进度（2026-11-24）**：第 9 条的接缝（`runtime.Loop`）、`loop.proto`、AgentDef 的 `loop`、`loops/*.yaml` 登记与 HTTP 客户端（`internal/extloop`，含 NDJSON 流式）、状态写入日志与投影、事件流剥离状态、"上一步"、回答校验、按步计量、外部循环的压缩上限、评测不覆盖模型已实现；模拟测试中的外部循环与故障注入已实现（2026-11-25，`internal/sim/loop.go`，`TestExternalLoopWithFaults`）；一致性套件已实现（2026-11-25，`internal/extloop/looptest`、`yanshi looptest`，在 `make check` 中对进程内的 Go 参考循环及八个出错变体运行），LangGraph 与 Pydantic AI 的参考适配待做。自报用量（ADR-0056 Q6）已在协议中、平台暂不记录：用量报表还不能按 Kind 分开，混入会把自报的 token 算进模型调用。
>
> **已定稿（2026-11-24）。** 来源：AgentDef 接入方式的讨论（Codex、Gemini 各自独立出方案后互评，2026-11-23）、两轮原型（本地分支 `prototype/langgraph-loop`，2026-11-24）与 Gemini 对本文初稿的评审。修订 [ADR-0041](0041-external-loops-decide-platform-acts.md) 的契约细节，补充 [ADR-0056](0056-replaceable-context-strategy-and-streaming-loops.md) B 期（流式协议沿用其 Q7，不改），修订 [m5 设计](../design/m5-inputs-triggers-loops.md) §7。ADR-0041 的治理原则（只做决定、平台执行，回答写入日志后才生效，只能用本业务线的循环）不变。

ADR-0041 让业务线的外部服务决定 Run 的每一步，契约只有一份草案（m5 §7）。业务线团队会用不同的框架写 Agent（LangGraph、Pydantic AI、自研循环），需要一份与框架无关、可以逐项验收的对接标准。原型回答了三个问题：

- **契约与框架无关。** LangGraph 以工具存根调用 `interrupt()` 交回调用、checkpoint 剥离消息后放进 `state`；Pydantic AI 以 deferred tools 结束一次运行、带结果开下一次。两者都通过了换进程、重复请求、attempt 递增、同一步多个并行调用；Pydantic AI 另测了被拒与失败的结果和中途压缩（LangGraph 只测了中途压缩）。
- **状态放得下。** 40 轮、每步两个各 4 KB 的结果：LangGraph 剥离消息后的状态约 3.2 KiB 且不随轮数增长（不剥离时第 8 轮超过 64 KiB）；Pydantic AI 与简单的 ReAct 图为 0 B。
- **AG-UI 的语义对得上，但生态还不稳。** "一次 AG-UI run 等于一步、遇到宿主执行的工具就以 `RUN_FINISHED` 结束"在 Pydantic AI 与 LangGraph 的官方适配上都成立；但 LangGraph 的官方适配（`ag-ui-langgraph` 0.0.48）没有 checkpointer 就报错，共享 checkpointer 时重复请求返回空的调用列表，仍须自写适配。

## 决定

1. **线上协议自定义，语义与 AG-UI 趋同。** 契约放在 `proto/`（新增 `loop.proto`，只做向后兼容的修改，`buf breaking` 检查），经 HTTP 以 JSON 传输；流式沿用 ADR-0056 Q7（`application/x-ndjson`，逐行增量，最后一行是完整的 `LoopStepResponse`），已输出过增量的步不在连接层重试（ADR-0056 第 6 条）。语义刻意与 AG-UI 1.0 对齐（下文"与 AG-UI 的对应"），业务线若已有 AG-UI 形态的服务，自行在边界转换。不直接采用 AG-UI：它刚到 1.0，各框架的官方适配仍在快速变化，协议变动会迫使内核跟随；自定义协议能原生表达 `attempt`、`turn`、二进制状态与版本、结构化内容块，并沿用 proto 的兼容性治理。

2. **无状态单步，状态只在一个 Run 内有效。** 外部服务不保存任何跨步状态；需要的状态放进 `LoopStepResponse.state`（解码后至多 64 KiB，JSON 中 base64 的膨胀不计入），附外部服务填写的 `state_version`（ADR-0056 第 7 条），随这一步的 `AssistantMessage` 写入日志，同一 Run 的下一步原样带回。新的 Run 与重答（Redo）的第一步 `state` 为空。服务的任何实例都必须能只凭一次请求的内容完成这一步。`state` 不交给客户端：事件流对外剥离它（`internal/feed` 的对外视图），它只用于下一步请求。

3. **同一步可能被请求多次，以写入日志的那次为准。** Worker 在写入之前崩溃、被接管、提交冲突或等待内容安全恢复（ADR-0054）时，同一步会以相同或递增的 `attempt` 再次请求，带回的是**本 Run 最近一次写入日志的那一步**的 `state`，两次回答可以不同。外部服务**不得**按 `(run_id, attempt, turn)` 去重或缓存回答（修订 m5 §7 的"按这三个字段做幂等"）；框架内部按 Run 区分会话时用 `run_id`，不拼入 `attempt`。外部服务在自己代码里的副作用会因重复请求与重答而重复发生，这是它自己的责任；对用户与平台的影响一律经平台执行的调用（ADR-0041 第 2 条）。

4. **工具一律由平台执行，不合法的回答不写日志。** 回答中的调用只能指向本步请求 `tools` 中的工具。平台在写日志前校验回答：调用指向请求以外的工具、`state` 超过上限、响应或流式的行无法解析（含未知字段以外的结构错误）、流式缺少最后一行，这一步作为失败处理，不写 `AssistantMessage`，与模型调用失败同口径计入 `MaxModelErrors`（同一 Attempt 连续出错的上限），之后重新请求。`loop.proto` 没有"调用结果"的字段，外部服务无从在协议中声称已经执行了调用；业务线在 AG-UI 等协议与 `loop.proto` 之间转换时，转换层须拒绝 `TOOL_CALL_RESULT` 一类事件。外部服务在自己的代码里做什么，平台管不到（ADR-0041），这一点不因协议而改变。

5. **平台侧的保证**（原型中依赖的，写进契约；标"新增"的须在实施时实现）：
   - **调用与结果一一对应。** 同一 Run 中，上一步的每个调用在下一步请求中都有且只有一条结果，包括审批被拒、能力拒绝与执行失败；这些结果以 `is_error` 标记，文字是平台写的说明（现行措辞见 `internal/runtime/worker.go` 的审批段），`loop.proto` 单独传 `is_error`，不靠文字前缀判断。
   - **调用 ID 由平台改写，按位置可映射（新增"上一步"字段）。** 平台把回答中的调用 ID 改写为全局唯一 ID（与内置循环相同，`internal/runtime/worker.go` 的 `callModel`），之后的上下文都用改写后的 ID。下一步请求另给出"上一步"：该步回答中调用的原顺序、改写后的 ID 与对应结果（含 `is_error`）。依赖自身调用 ID 的框架（如 LangGraph 的中断）按位置映射，不必解析消息尾部。
   - **内容安全拦截即结束 Run。** 回答被内容安全拦截时，平台以拒答文本替换回复、丢弃调用并结束 Run（现行行为，`callModel` 的内容安全段），这一步的 `state` 不写入；同一 Run 不再有下一步。
   - **两步之间的输入排在结果之后，在途的输入排在这一步之前。** 两步之间到达的用户插话（`Steered`）、迟到的后台结果与取消、通话转写，都出现在 `messages` 中上一步的结果之后，外部服务不能假定 `messages` 以工具结果结尾。外部服务处理一步期间到达的插话，在日志中排在这一步的 `AssistantMessage` 之前（提交冲突后重试追加）；这一步没有调用时 Run 随即结束，插话由后续的 Run 处理，与内置循环相同。
   - **压缩不越过最近一步（新增）。** 现行压缩只保证闭合边界与"最新的一个事件保留原文"（`session.State.CanCut`、`runtime.pickCut`），最近一步之后有插话等事件时，最近一步可能整体进入摘要。由外部循环驱动的 Run，截断点不得越过本 Run 最近一条 `AssistantMessage` 之前。内置循环的压缩不变。

6. **AgentDef 的形态。** `loop: {server: <名称>}` 与 `model` 互斥（修订 m5 §7"`model` 仍然必填"）。`context.scope: session`（默认）时必须配 `context.summary_model`，平台照常压缩；`scope: run` 时可以不配，Run 内超出窗口时 Run 失败。`instructions` 照旧作为 `system` 发给外部服务，外部服务可以不用它；`description` 仍是委托时的工具说明；Call 的语音模型独立配置（`call.model`），不受影响。能力白名单、`max_turns`、召回、委托限制等治理字段对外部循环同样生效。媒体（图片、音频）与给模型的请求相同的形态与上限交给外部服务（`Worker.inlineImages`）。

7. **只接同一信任域。** 外部循环的登记（`loops/*.yaml`）以 `(业务线, 名称)` 为键（与 MCP Server 相同，ADR-0060 第 5 条），只属于一条业务线，且只能是该业务线自己运营的服务：请求携带完整的上下文视图（含他人旁听中的话与设备的结果，ADR-0041 Consequences）。三方开发的 Agent 不作为外部循环接入：经出站能力接入（ADR-0048）；将来托管阶段三方开发者成为独立业务线时，也不能登记外部循环，只能用内置循环（重审条件）。

8. **一致性套件是对接标准。** 内核提供黑盒的一致性套件（拟名 `looptest`），经 HTTP 驱动一个外部循环服务，覆盖：换进程恢复、同一步重复请求、attempt 递增、同一步多个并行调用、被拒与失败的结果、调用 ID 按位置映射、`messages` 不以工具结果结尾（插话、后台结果）、新 Run 与重答时 `state` 为空、中途压缩、状态上限、超时与不合法的回答、流式的最后一行。业务线的循环服务须通过它才能登记上线，与存储实现须通过一致性套件同理。内核同时提供 LangGraph 与 Pydantic AI 的参考适配（原型中的坑由适配处理：LangGraph 的并行调用不能用普通边直连 `ToolNode`，须用 `Send` 扇出或一次交出整组调用；Pydantic AI 的 AG-UI 或消息转换须保留平台的系统指令），位置与语言在实施时确定。
   2026-11-25 确定：套件是 Go 包 `internal/extloop/looptest`，以 `yanshi looptest -url <服务>` 运行，在 `make check` 中对一个进程内的 Go 参考循环运行（另有一组故意出错的变体，证明套件能查出它们）。黑盒的套件不掌握被测服务用的模型，只查格式就查不出上面那类框架转换错误，所以套件同时托管一个脚本化的 OpenAI 兼容模型，被测服务须能配置模型地址、在测试时指向它：套件据此既检查服务的回答，也检查框架交给模型的内容（系统指令、按位置配对的结果、插话）。参考适配是 sdk/python 下（待建）的 Python 包（uv 管理、锁定版本），由单独的 `make looptest-adapters` 启动适配并运行 `yanshi looptest`；它需要 Python 与联网安装依赖，与 `make verify` 一样不在 `make check` 中。

9. **内核内部的接缝。** `Worker.callModel` 中"调用模型"的部分抽成决策者接口：外层照旧负责上下文组装、工具解析与祖先交集、配额、调用 ID、内容安全与写日志；内置实现生成的请求逐字节不变，前缀缓存不退化。先做不改变行为的重构，再接外部循环。

## 与 AG-UI 的对应（非规范，供业务线转换）

| yanshi `loop.proto` | AG-UI 1.0 | 说明 |
|---|---|---|
| 一次 `LoopStepRequest` / `LoopStepResponse` | 一次 run（`RunAgentInput` → 事件流 → `RUN_FINISHED`） | 遇到平台工具即结束本次 run |
| `run_id`；`session_id`、`attempt`、`turn` | `threadId`；`forwardedProps` | AG-UI 没有 attempt 与 turn |
| `messages`、`tools`、`system` | `messages`、`tools`、`SystemMessage` | AG-UI 每条消息要求 `id`，由转换方派生 |
| `state`（字节）+ `state_version` | `state`（JSON）/ `STATE_SNAPSHOT` | 二进制须 base64 |
| NDJSON 增量行 | `TEXT_MESSAGE_CONTENT`、`REASONING_*` | |
| `tool_calls` | `TOOL_CALL_START/ARGS/END` | 转换层遇到 `TOOL_CALL_RESULT` 须拒绝（第 4 条） |
| 这一步失败 | `RUN_ERROR` | |

卡片、卡片动作、旁听片段、后台结果等结构化内容块在两边都须渲染为文字，结构化字段在外部服务一侧不可见（来源推导仍由平台从日志进行，不受影响）。

## Considered Options

- **线上协议采用 AG-UI 1.0 的子集**：生态框架（Pydantic AI、Agno、Mastra 等）可直接对接，流式事件现成；但协议与官方适配仍在快速演进，LangGraph 的官方适配不满足无状态与重复请求的要求，内核须跟随外部变动。语义对齐后，业务线自行转换的成本低（用户裁决，2026-11-24）。
- **同时支持自定义协议与 AG-UI**：一致性套件与故障面加倍，没有对应的需求。
- **允许外部服务自存状态**（例如 LangGraph 的持久 checkpointer）：协议更小，但状态不随 Session 删除，回放时拿不回当时的状态（ADR-0041 已否决）。
- **以 `(run_id, attempt, turn)` 为幂等键缓存回答**：接管时 attempt 递增，键对不上；同一键下返回旧回答还会错过插话后的新输入。
- **内容安全拦截后保留状态、让外部服务继续**：外部服务会基于被拦截的那步继续推理，而它看不到拦截发生；与内置循环一致地结束 Run 更简单。

## Consequences

- m5 §7 的契约草案按本 ADR 修订；`loop.proto` 在实施时新增（含 `state_version`、自报用量与"上一步"）。`AssistantMessage` 增加状态字段，改动 `State` 与 `Apply` 时递增 `ProjectionVersion`。
- **计量与配额须补齐。** 外部循环按步计量（ADR-0056 Q6 的 `Kind="loop"`），价格表须有按步的单价，启动时的计量校验（`cmd/yanshi/serve.go` 的 `metering`）对 `loop` 的 AgentDef 检查它，而不是检查 `model`；否则外部循环的步数不计费、绕过金额配额。
- **评测须适配。** 评测运行器的派生（`internal/eval/runner.go`）对 `loop` 的 AgentDef 不覆盖模型、不要求 `model`，并以回放式的假循环固定；外部循环的效果仍由 `make eval` 裁判。
- 内核多一个一致性套件与参考适配要维护。平台不计量外部服务自己的模型用量（自报用量只用于报表）；前缀缓存由外部服务负责。

## 必须验证

- 模拟测试：外部循环的超时、不合法的回答、写入前崩溃、重复请求、旧 attempt 的回答迟到、内容安全等待与拦截；回放时对外部服务的请求数为 0；状态随 Session 删除、不出现在对外事件流中；由外部循环驱动的 Run 压缩后最近一步仍是原文。实施前按[验证方案](../formal-verification/2026-11-23-verification-plan.md)先为"提交、重试与接管"建模：已完成（2026-11-24，`docs/formal-verification/external-loop/`，随 `make verify` 运行），覆盖本文第 2–5 条的状态链、Attempt 隔离、调用与结果一一对应、不合法与被拦截的回答、压缩不越过最近一步，八个违反本文的变体均被查出；实现后补由模型调度驱动的 Go 场景：已完成（2026-11-25，`internal/sim/loop.go`）。模拟中的请求经真实的 `extloop.Client` 由进程内传输应答，回答由脚本化模型给出，循环的故障用独立随机流注入：超时、5xx、五种不合法的回答、收到回答后写日志途中崩溃、租约在请求途中过期并被另一个 Worker 接管（旧 Attempt 的回答迟到）；Worker 崩溃、存储故障、压缩与内容安全的等待和拦截沿用既有注入。每一步请求对照日志检查状态、步序与"上一步"（含压缩后最近一步仍是原文、结果按位置对应），回放与快照恢复不请求外部服务；日志中每个 Run 的第 k 条回答必须带第 k 步的状态，被拦截的回答不带状态且结束 Run，对外事件流不含状态；状态只在日志与快照中，随 Session 删除由既有的删除不变量检查。去掉压缩上限、`refuse` 保留状态、不校验回答、不带回状态、去掉 Attempt 隔离，五个变体均被查出。模拟同时观察到：Session 被删除后，持有租约的 Worker 在两步之间沿用本地投影，仍会发出下一步请求、写入时才发现删除（`TestExternalLoopWithFaults` 的 `LoopRequestsAfterDeletion`）；内置模型调用同样如此，回答不会写入。
- 决策者接口重构后：`make check`、`make sim` 通过，`make eval` 无回归且"缓存命中"不退化。
- 参考适配通过 `looptest`；原型分支中的场景全部进入套件，并补上原型未覆盖的：LangGraph 的被拒与失败结果、调用 ID 改写后的按位置映射、`messages` 不以工具结果结尾。

## 重审条件

- AG-UI（或同类协议）出现稳定的版本承诺并被多数目标框架原生支持，业务线普遍要求直接对接时，重审第 1 条；
- 真实业务的状态超过 64 KiB，或需要跨 Run 的状态时，重审第 2 条；
- 需要三方开发的 Agent 作为外部循环接入（含托管阶段的三方业务线）时，重审第 7 条（同时与 ADR-0048 一并修订）；
- 开始实施时，核对上文的保证是否仍与 Worker 的实现一致，并去掉"尚未实现"的标注。
