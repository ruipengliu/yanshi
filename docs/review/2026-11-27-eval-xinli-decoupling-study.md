# 调研：把 xinli 的评分移出 `internal/eval`

> **状态** 调研结论，2026-11-27。**本轮不实施**：评分留在内核评测运行器是现行设计（[Persona 架构](../design/persona-architecture.md) §3.4"评测夹具契约"：被测方不做自己的裁判），且只有一条业务线时解耦没有收益。本文保存触点清单与候选方案，供重审时直接使用。
>
> **重审条件**：第二条业务线需要自己的业务断言（不只是用例与基线），或 xinli 的评分改动频繁到需要由业务线独立发布。触发后从下文方案 A 开始定稿，并写 ADR。
>
> 来源：[架构评估评审](./2026-11-22-architecture-evaluation-review.md) §8 第 2 条。用例、数据与基线已于 2026-11-27 拆到 `apps/xinli/evals/`（[评测设计](../design/m4-eval.md) §2）。下文以提交 `18daf3b` 的源码为准，行数为粗估，混合函数的范围有交叠；本次未运行测试或评测。

## 1. 触点清单

| 类 | 内容 | 位置 | 约行数 |
|---|---|---|---|
| (a) 通用能力，命名或示例带 xinli | 追加 Run、场景时钟隔离与拨钟、通用评分、过期 Memory 的日期解析；拆分 `captureZone` 时须保留 Memory 的上海时区口径 | `internal/eval/spanruns.go`、`scenario.go`、`schedule.go`、`judge.go` 前半、`runner.go`、`suite.go` 少量 | 110 |
| (b) 夹具进程与数据契约 | 启停、就绪行、MCP 登记与凭据、PG 配置、`Seed/State/Bind/BindDay/Submitted/Edit`、旁听提交、处理进度与预览、reset、擦除与删除待办；`State` 镜像 | `internal/eval/xinlifixture/` | 700 |
| (b) 夹具调用与用例数据 | `setup.xinli`、`captures[_file]`、元数据与噪声校验、相对日期、业务日拨钟与换绑、分批与批内说话人编号 | `suite.go`、`capture_metadata.go`、`scenario.go`、`runner.go` 多处 | 550 |
| (c) 业务断言 | `expect.xinli`（交谈、项目、承诺、合并、核对率与分组）、金标加载与匹配、`.gold.txt`、`judge_state` 投影与覆盖率评分 | `gold.go`、`xinli_text.go`、`judge.go` 后半、`suite.go`、`runner.go` 多处 | 1,000 |
| (c) span-runs | 派生 AgentDef 时追加指令、轮内按预览与接受记录逐 Run 决策、共享原轮超时、中断在途 Run、失败时计入已用成本；单次 `Check` 承载不了 | `spanruns.go`、`suite.go`、`runner.go` | 260 |
| (d) 装配、CLI 与报告 | 夹具选择（主、换轮、委托 Agent 的白名单；真正的 Oneshot 跳过夹具）、默认时区、`-xinli-*` 参数、实验报告标记、指标方向（`fragmentation` 越低越好，`commitments_count` 豁免数值退化） | `runner.go`、`schedule.go`、`cmd/yanshi/evalcmd.go`、`report.go` | 90 |

## 2. 候选方案

**A · 内核运行器驱动业务线的评测适配器（推荐，重审时的起点）。** `yanshi eval` 继续驱动内核运行与基线比较；`apps/xinli` 提供独立的评测进程（不是被测的 `xinli dev`），负责解析业务用例、驱动夹具、判定业务断言。

- 协议：版本化 JSON/HTTP。就绪行 `{version, url}`，之后 `DescribeCase`（业务侧读原 YAML 与相对路径资产，返回通用用例配置、AgentDef 覆盖与不透明用例 ID）、`StartInstance`（返回 MCP 登记）、`StartTrial`、`AdvanceTurn`（换日、建 Session、输入前后交换观察与下一步动作）、`Check`、`FinishTrial`。
- 宿主只执行限定在当前 trial 的动作：`AdvanceClock`、`RunInput`、`Interrupt`、`CapturePageOptions/RenderPreview`（复用 `internal/capture` 的分页校验与预算）、`RequestJudge`（沿用 `-judge`，适配器拿到原始评分响应）。
- `Check` 返回断言 `{name, pass, detail, infra}`、指标与诊断；套件声明指标方向，宿主沿用 `report.go` 的聚合与比较。
- 影响：用例 YAML 与两份基线原样保留；`make eval-xinli` 由业务侧入口承接原参数；PG 夹具路径不变。第二条业务线提交自己的适配器、用例与基线即可，不改内核。
- 代价与风险：约 2,500–3,500 行、25–35 个文件；跨进程超时、操作重投与评分输入漂移。
- 与现行原则的关系：评分进程归业务线，"被测方不做自己的裁判"须改写为"评分进程与被测进程分开"，写入 ADR 并修订 Persona 架构 §3.4。

**B · 内核模块内的扩展包 `internal/eval/xinli`，静态注册。** Go 接口 `DecodeCase/Start/BeforeTurn/RunTurn/Check`，`evalcmd` 注册实现。YAML、基线、Make 入口与 PG 路径都能原样接续，约 2,100–2,800 行、15–25 个文件，迁移风险最低；但业务规则仍编入内核模块，第二条业务线要改注册并重编内核。只适合作为 A 的过渡。

**C · 业务评测二进制驱动公开的评测 SDK。** SDK 是版本化 JSON/HTTP 客户端（`OpenTrial/CreateSession/RunTurn/Observe/Judge/FinalizeReport`），内核子进程执行原语；不能直接包装 `internal/eval`（依赖检查）。约 3,500–5,000 行、30–45 个文件；公开控制面宽，各业务驱动之间的超时、记账与跳过规则容易漂移。

## 3. 分阶段计划（方案 A）

每阶段可独立合入，门槛是 `make check` 全绿、两份基线文件零差异。

1. **冻结口径。** 用两套历史报告及缺指标、缺样本、评分失败的样本锁住 `Compare` 与 `MergeBaseline`。补录合成用例的逐轮观察、编辑前后的 State、引用标签、金标状态与两类评分的请求和响应：历史报告只够回放比较器，`-logs` 的 Event 补不齐这些输入。
2. **引入可选协议。** 先用模拟的第二条业务线接通进程生命周期、MCP、拨钟、断言与指标，现有入口照走原路径；指标方向改为套件配置，以历史报告逐项比对回归输出。
3. **迁评分与解析，双路判定。** 业务侧读原 YAML，保留相对路径、字段存在性与 `{}` 的语义；新旧判定读同一份冻结观察与原始评分响应，逐项比对提示字节、断言、指标、Infra、完成数与评分样本。
4. **切换编排与入口，删除旧实现。** 用脚本模型比对跨日换绑、无预置 MCP、真正的 Oneshot，以及 span 续页、拒收重交、无进展、超时中断与部分成本；双路一致后切换 Make 入口，以两份基线运行 `make eval`，另跑 PG 夹具回归。

## 4. 重审时须拍板

- 采纳 A 时，把"评分进程与被测进程分开"写入 ADR，取代现行"夹具不提供 Check"的表述；
- `yanshi eval -xinli-*` 参数的兼容窗口；
- 协议 v1 只承诺同一提交构建，还是支持运行器与适配器跨版本配对。
