# yanshi

分布式 Agent 运行时基础设施，Go 实现。`make check` 是提交前的完成标准：必须全绿（需要 Docker：它会启动 PostgreSQL、S3 兼容存储并运行沙箱测试；对应测试分别由 `YANSHI_TEST_PG`、`YANSHI_TEST_S3`、`YANSHI_TEST_DOCKER` 启用，未设置时跳过）。

## 先读

- 术语：`CONTEXT.md`。代码、注释、文档只用其中的名词；出现新概念时先在那里定义。
- 目标与原则：`docs/charter.md`。
- 改动执行语义（Session / Run / Event / Attempt / Worker）之前：`docs/design/m0-core-primitives.md`。
- 改动 Node、能力路由、审批或挂起之前：`docs/design/m1-device-nodes.md`。
- 改动云端沙箱之前：`docs/design/m2-sandbox.md`。沙箱内只运行不可信代码，受信逻辑一律放在控制器（ADR-0008）。
- 改动 MCP 适配之前：`docs/design/m5-mcp.md`。云端不运行 stdio 型 MCP Server；未声明只读的工具默认需要审批（ADR-0017）。
- 改动工件之前：`docs/design/m2-artifacts.md`。Event 中只放 `artifact://` 引用，不放文件内容；工件只对其所属 Session 可见。
- 新增任何存放 Session 数据的地方之前：`docs/design/m2-session-lifecycle.md`。它必须能被 Janitor 按 Session 删除，写入入口要"先写、后查"删除记录（ADR-0015），并在 `internal/sim` 的删除不变量中检查。
- 改动 AgentDef 的版本选择或发布配置之前：`docs/design/m4-agent-rollout.md`。Session 当前使用的版本读 `State.Agent`（投影），不读 `Created.Agent`；新版本进入灰度或成为 stable 之前，须用 `make eval EVAL_FLAGS="-sandbox docker -agent-version <版本>"` 评测且与基线相比没有回归（ADR-0020）。
- 改动内容安全的检查点或提供商之前：`docs/design/m4-moderation.md`。输入与输出都在写日志之前检查，失败即拒绝（ADR-0021）。上线前须完成 `docs/launch-checklist.md`。
- 改动计量、配额或价格表之前：`docs/design/m4-quota-usage.md`。用量独立于 Event 日志保存，不含 Session 与内容；在模型调用返回后、写日志前计量（ADR-0019）。
- 做出难以逆转的架构决策时：在 `docs/adr/` 新增一条 ADR。

## 不变量

- **Event 日志是唯一事实源。** 状态只能由 `session.Reduce` 从日志投影得到；所有写入经 `session.Store.Commit`（先校验、再乐观并发追加）。投影快照只是缓存：**改动 `Apply` 或 `State` 的字段时递增 `session.ProjectionVersion`**，旧快照随之作废。
- **契约在 `proto/`。** 修改 `.proto` 后运行 `make gen`，生成代码 `gen/` 一并提交；只做向后兼容的修改（`buf breaking`）。
- **Worker.Step 是确定性的单步。** 时间取自 `clock.Clock`，ID 取自 `ids.Generator`；影响结果的逻辑放在 Step 的同步路径里，以便 `internal/sim` 在任意两步之间注入故障。
- **评测是效果的裁判。** 改动模型、提示词、AgentDef，或召回、压缩、Memory、MCP、审批摘要等给模型看的文本时，运行 `make eval`（需要 `TOKENHUB_API_KEY`）并与 `evals/baselines/` 比较，不得有回归；有意的效果变化用 `-update-baseline` 更新基线并随代码提交。新增给模型看的能力或文本时，同时在 `evals/` 增加覆盖它的用例（`docs/design/m4-eval.md`）。
- **模拟测试是执行语义的裁判。** 改动 runtime / session / service / workqueue 后运行 `make sim`；失败信息里的种子可复现：`go test ./internal/sim -run <Test> -sim.seed=<N>`。新增故障类型或路径时，同时在 `internal/sim` 中注入并加入覆盖统计。
- **新存储实现必须通过一致性套件**（`eventlogtest`、`workqueuetest`、`nodetest`、`ledgertest`、`sandboxtest`、`artifacttest`、`lifecycletest`、`memorytest`、`snapshottest`、`usagetest`），并接入 `internal/sim` 的差分测试。数据库变更只通过新增 `internal/pg/migrations/NNNN_*.sql`，不修改已有迁移。
- **路由调用可以安全重投递，前提是 SDK 以 call_id 去重**（`sdk/nodesdk.Executor`）。修改 Executor 或 Ledger 时，保持"started 状态先落盘、再执行"的顺序。

## 风格

注释与文档使用中文，标识符使用英文。注释说明"为什么"。

日志与指标中只记录 ID（Session、Run、调用、工件），不记录用户输入、模型输出、调用参数或结果：这些是个人数据，只能存放在可被删除的地方（ADR-0015）。
