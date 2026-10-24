# yanshi

分布式 Agent 运行时基础设施，Go 实现。`make check` 是提交前的完成标准：必须全绿（需要 Docker：它会启动 PostgreSQL、S3 兼容存储并运行沙箱测试；还需要 Node.js 与 pnpm：网页 SDK 的检查与互通测试。对应测试分别由 `YANSHI_TEST_PG`、`YANSHI_TEST_S3`、`YANSHI_TEST_DOCKER`、`YANSHI_TEST_NODE` 启用，未设置时跳过）。

## 先读

- 本机开发与 Mac mini 环境：开始开发、构建或测试前，若本工作树或主工作树存在 `.local/macmini/README.md`，必须先读并遵循。本机主要编码与 Git 操作，优先通过本地 `dev` 入口同步当前工作区并在 Mac mini 构建、测试或启动服务，中间件使用其 OrbStack；可用命令以文档中的当前实现状态为准，不得把历史源码快照当作当前代码。该文档及辅助配置不提交 Git；文档缺失时先检查本地配置，不假定远程环境已就绪。
- 术语：`CONTEXT.md`。代码、注释、文档只用其中的名词；出现新概念时先在那里定义。
- 目标与原则：`docs/charter.md`。
- 改动执行语义（Session / Run / Event / Attempt / Worker）之前：`docs/design/m0-core-primitives.md`。
- 改动 Node、能力路由、审批或挂起之前：`docs/design/m1-device-nodes.md`。
- 改动云端沙箱之前：`docs/design/m2-sandbox.md`。沙箱内只运行不可信代码，受信逻辑一律放在控制器（ADR-0008）。
- 改动 Connection 协议（`node.proto`）、会话客户端或事件流（`feed`）之前：`docs/design/m3-duplex-channel.md`。一条 Connection 兼任 Node 与会话客户端（ADR-0024）；会话客户端的请求必须经 `service`，与 HTTP API 走同一条路径。`ask_user` 是路由到用户本人的调用，不经 Inbox（ADR-0025）。
- 改动端侧 SDK（`sdk/`）之前：`docs/design/m3-client-sdks.md`。三个 SDK 语义一致，消息类型一律从 `proto/` 生成（ADR-0026）；改动 `sdk/nodesdk`、`sdk/mobile` 或 `node.proto` 后运行 `make mobile`（需要 Xcode 与 JDK），在 macOS 与 iOS 模拟器上实测 gomobile 绑定。
- 改动调试控制台（`internal/webui/static/console`）或脚本化模型（`internal/model/script`）之前：`docs/design/console.md`。新增多端语义时，在控制台的验证场景（`scenarios.js`）中增加覆盖，它们在 `make check` 中运行（`TestConsoleScenarios`）。
- 改动卡片（`internal/card`、`show_card`、`Card` / `CardAction`）之前：`docs/design/cards.md`。卡片只由平台校验后写入 `show_card` 的结果，卡片动作是一条输入、按 `call_id` 与动作去重（ADR-0032）。
- 改动旁听（`CaptureAppended`、`State.Captures`、`service.SubmitCaptures`、`captures` 工具、`ContentBlock.capture`）、按 AgentDef 覆盖的 Retention 或 `agents/xinli-*.yaml` 之前：`docs/design/captures.md`。旁听只收文字，佩戴者的话算用户原话、他人的话算外部内容，判断依据是结构化的说话人字段（ADR-0036）；Capture 不开启 Run，摘要由 Project 经业务线的 MCP Server 拉取，按保留期删除时保留 Memory（ADR-0037）。每日整理按交谈分页、逐段交给业务线，承诺引用原话、由平台附上说话人供业务线核对：改动 `captures` 工具的分页、`xinli-*` 的流程、xinli 的工具或 MCP 登记的 `cite_captures` 之前，另读 `docs/design/captures-by-exchange.md`。
- 改动按住说话的语音识别与播报（`internal/speech`、`/transcriptions`、`/speech`）之前：`docs/design/speech.md`。录音识别成文字后作为普通输入提交，音频不交给模型、不进日志或工件；播报只读 Run 已写入日志的回复。
- 改动服务端故障注入（`internal/faults`、`/v1/debug/*`）之前：`docs/design/console.md` §7。只在 `serve -faults` 时装配，没有规则时不改变任何行为（ADR-0033）。
- 改动 Call（`internal/call`、`internal/realtime`、控制台的通话 `internal/webui/static/console`）之前：`docs/design/m3-call.md`。音频经网关中转，语音模型只通过 `run_task` 派生 Run 做事（ADR-0027）；转写在写日志之前做内容安全检查。实测需要 `VOLC_SPEECH_API_KEY`（`YANSHI_TEST_VOLC=1 go test ./internal/realtime/volc`），`make eval` 的 `call-*` 用例同样需要它。
- 改动模型提供商的接入、调用时限、重试或准入之前：`docs/design/model-calls.md`。已输出过增量的调用不在提供商层重试。
- 改动进程角色、启动参数或部署形态之前：`docs/design/deployment.md`。进程边界只沿扩缩容维度、权限与故障域划分，不按领域拆服务；全部角色合在一个进程里必须仍能运行。
- 改动委托（`internal/delegation`、`agent:` 白名单、`SessionCreated.parent`）之前：`docs/design/m5-multi-agent.md`。委托是路由到派生子 Session 的调用，结果由子 Session 的 Worker 写回（ADR-0031）；子 Run 的能力是自身与每个祖先白名单的交集，委托不扩大权限。
- 改动判断（`internal/decision`、`DecisionMade`、`-injection-check`）或设备歧义消解之前：`docs/design/m6-decisions.md`。判断先记为 Event 再据此行动，只收紧不放宽，失败走默认路径（ADR-0030）；阈值只在评测留出集上确定。
- 改动 MCP 适配之前：`docs/design/m5-mcp.md`。云端不运行 stdio 型 MCP Server；未声明只读的工具默认需要审批（ADR-0017）。
- 改动工件之前：`docs/design/m2-artifacts.md`。Event 中只放 `artifact://` 引用，不放文件内容；工件只对其所属 Session 可见。
- 新增任何存放 Session 数据的地方之前：`docs/design/m2-session-lifecycle.md`。它必须能被 Janitor 按 Session 删除（在 `app.SessionData` 登记），写入入口要"先写、后查"删除记录（`lifecycle.Recheck`，ADR-0015），并在 `internal/sim` 的删除不变量中检查。
- 改动 Memory 的写入、召回或授权之前：`docs/design/m4-memory-grant.md`。外部内容影响下的写入须经用户批准，写入闸门只收紧不放宽（ADR-0023）；健康类须业务线开启（ADR-0022）。
- 改动 AgentDef 的版本选择或发布配置之前：`docs/design/m4-agent-rollout.md`。Session 当前使用的版本读 `State.Agent`（投影），不读 `Created.Agent`；新版本进入灰度或成为 stable 之前，须用 `make eval EVAL_FLAGS="-sandbox docker -agent-version <版本>"` 评测且与基线相比没有回归（ADR-0020）。
- 改动内容安全的检查点或提供商之前：`docs/design/m4-moderation.md`。输入与输出都在写日志之前检查，失败即拒绝（ADR-0021）。上线前须完成 `docs/launch-checklist.md`。
- 改动计量、配额或价格表之前：`docs/design/m4-quota-usage.md`。用量独立于 Event 日志保存，不含 Session 与内容；在模型调用返回后、写日志前计量（ADR-0019）。
- 做出难以逆转的架构决策时：在 `docs/adr/` 新增一条 ADR。

## 不变量

- **Event 日志是唯一事实源。** 状态只能由 `session.Reduce` 从日志投影得到；写入方经 `session.Store.Update` 写入：看着投影决定事件，先校验、再乐观并发追加，冲突后重新决定（不要另写重试循环）。投影快照只是缓存：**改动 `Apply` 或 `State` 的字段时递增 `session.ProjectionVersion`**，旧快照随之作废。
- **契约在 `proto/`。** 修改 `.proto` 后运行 `make gen`，生成代码（`gen/` 与 `sdk/web/src/gen/`）一并提交；只做向后兼容的修改（`buf breaking`）。
- **Worker.Step 是确定性的单步。** 时间取自 `clock.Clock`，ID 取自 `ids.Generator`；影响结果的逻辑放在 Step 的同步路径里，以便 `internal/sim` 在任意两步之间注入故障。
- **请求前缀保持稳定。** 提供商的前缀缓存是长 Run 成本的主要杠杆：系统指令只放 AgentDef 的指令，不放每个 Run 都变化的内容（召回放在 Run 的位置上，见 `runtime.Transcript`）；工具按名称排序；Run 内的上下文只在尾部追加。改动 Transcript 或请求组装时，用评测报告的"缓存命中"列确认没有退化。
- **评测是效果的裁判。** 改动模型、提示词、AgentDef，或召回、压缩、Memory、MCP、审批摘要等给模型看的文本时，运行 `make eval`（需要 `ARK_API_KEY`）并与 `evals/baselines/` 比较，不得有回归；有意的效果变化用 `-update-baseline` 更新基线并随代码提交。新增给模型看的能力或文本时，同时在 `evals/` 增加覆盖它的用例（`docs/design/m4-eval.md`）。
- **模拟测试是执行语义的裁判。** 改动 runtime / session / service / workqueue 后运行 `make sim`；失败信息里的种子可复现：`go test ./internal/sim -run <Test> -sim.seed=<N>`。新增故障类型或路径时，同时在 `internal/sim` 中注入并加入覆盖统计。
- **新存储实现必须通过一致性套件**（`eventlogtest`、`workqueuetest`、`nodetest`、`ledgertest`、`sandboxtest`、`artifacttest`、`lifecycletest`、`memorytest`、`snapshottest`、`usagetest`、`presencetest`、`notifytest`），在装配包 `internal/app`（`app.Stores`，serve、模拟测试、评测与 e2e 共用）中构造，并接入 `internal/sim` 的差分测试。核心对象图同样只在 `app.NewCore` 装配：新增跨切面的策略（故障注入、判断、提醒、计量、准入等）接入 `app.Options` 与 `NewCore`，serve / sim / eval / e2e 不再各自布线；测试替身在 Stores 的 seam 上包装，或在返回的 `Core` 上替换字段。外部写入之后须唤醒 Session 的（输入、审批、回答、Node 结果）在决策中置 `session.Decision.Wake`：日志与队列在同一事务中提交（`eventlogtest.RunWaking`）。数据库变更只通过新增 `internal/pg/migrations/NNNN_*.sql`，不修改已有迁移；大表上建索引用 `CREATE INDEX CONCURRENTLY`，迁移首行写 `-- yanshi:no-transaction`（只含这一条语句，且可重复执行）。新表须在 `pg.Tables` 登记分片键（Session 或 EndUser）；在线路径按键访问，一条 SQL、一个事务只涉及一个键（ADR-0028）。
- **路由调用可以安全重投递，前提是 SDK 以 call_id 去重**（`sdk/nodesdk.Executor`）。修改 Executor 或 Ledger 时，保持"started 状态先落盘、再执行"的顺序。

## 风格

注释与文档使用中文，标识符使用英文。注释说明"为什么"。TypeScript 由 prettier 格式化（`cd sdk/web && pnpm format`）。

日志与指标中只记录 ID（Session、Run、调用、工件），不记录用户输入、模型输出、调用参数或结果：这些是个人数据，只能存放在可被删除的地方（ADR-0015）。
