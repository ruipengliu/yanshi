# M4 AgentDef 灰度发布

> 状态：已实现（见 §8） · 依赖：章程原则 10、M4 评测（ADR-0018）· ADR-0020

## 1. 目标

新的 AgentDef 版本（换模型、改提示词、改能力或上下文策略）先通过评测，再按比例分给一部分 EndUser，比较效果后全量或回滚。回滚不需要重启。

不做：同一 Session 内的 A/B（一个 Session 只用一个版本）；按业务线配置不同的发布（AgentDef 文件本身不分业务线；哪些业务线可以使用它，由业务线配置的 `agents.allow` 声明，ADR-0060）；自动按指标回滚（先人工判断）。

## 2. 发布配置

每个 AgentDef 文件定义一个 (name, version)，随部署上线。发布配置放在定义所在目录的 `releases.yaml`，如内核的 `agents/releases.yaml`。业务线也在自己的 AgentDef 目录维护发布配置。

```yaml
assistant:
  stable: "1"                  # 默认版本
  canary:                      # 可选：灰度版本
    version: "2"
    percent: 10                # 分给 10% 的 EndUser
    end_users: [qa-zhang]      # 白名单：内部测试人员总是进入灰度
  withdrawn: ["2-rc1"]         # 撤回的版本（回滚）
```

- 没有出现在 `releases.yaml` 中的 Agent 沿用原规则：默认版本为最后加载的版本。
- 发布条目只能引用同一目录的定义及其已加载版本。`stable` 与 `canary.version` 必须是未撤回的版本。
- `serve` 每 10 秒检查各目录的发布配置。读取与校验全部成功后一次性替换合并结果。校验失败或多目录部署中读取失败时，保留上一版并记录日志与指标 `yanshi_release_reload_errors_total`。在目录仍可读取时，移除其中的配置文件会恢复该目录下定义的默认选择。各进程独立加载，最多约 10 秒不一致；版本在 Session 创建时固定，这种不一致只影响那 10 秒内新建的 Session 分到哪个版本。

## 3. 分流

创建 Session 时未指定版本：

1. EndUser 在 `canary.end_users` 中 → 灰度版本；
2. `fnv64a(agent 名 + EndUser) mod 10000 < percent × 100` → 灰度版本；
3. 否则 → 稳定版本。

按 EndUser 稳定分流：同一用户的新 Session 总在同一版本；比例调大时，已在灰度中的用户仍在灰度中（散列值不变，阈值变大）。散列包含 Agent 名，不同 Agent 的灰度人群互不相关。

指定版本创建（`agent_version`）仍然允许，用于业务线自己的测试；撤回的版本不能指定（400）。

## 4. 回滚：撤回版本

把版本加入 `withdrawn` 即回滚：

- 新 Session 不再分到它；
- 已固定在它上面的 Session，在**下一个 Run 开始时**切换到当前的稳定版本：Service.Submit 新建 Run 时，与 RunRequested 在同一次提交中写入 `AgentSwitched{from, to, reason: "withdrawn"}`；
- 进行中的 Run 不受影响（插话不触发切换），它的上下文、调用与审批都基于原版本，中途切换会破坏一致性。

`AgentSwitched` 是 Event：Session 当前使用的版本由日志投影得到（`State.Agent()`），日志仍是唯一事实源；Worker 与 API 都读投影中的当前版本。普通的全量发布（改 `stable`）不迁移已有 Session：它们的版本没有问题，没有必要改变正在进行的对话。

撤回版本的 AgentDef 文件要等其 Session 都已切换或结束后再删除，否则进行中的 Run 找不到定义而失败。

## 5. 比较

- 指标按版本区分：`yanshi_runs_finished_total{status, agent}`、`yanshi_run_duration_seconds{status, agent}`，`agent` 为 `name@version`（版本数量少，基数可控）。
- 用量记录增加 `agent`（`name@version`），`GET /v1/usage?group_by=agent` 按版本汇总成本（迁移 `0008_usage_agent.sql`）。沙箱执行的用量不区分版本。

## 6. 评测门槛

`yanshi eval -agent-version <v>` 评测尚未成为默认的版本（各用例的 Agent 使用该版本）。AGENTS.md 规定：进入灰度或成为稳定版本之前，该版本必须通过 `make eval`，内核与 xinli 两套评测各自与基线比较。只改 xinli 提示词时可运行 `make eval-xinli`。评审时检查。运行时不强制。

## 7. 验证

- 单元测试：分流的确定性与比例、白名单、比例调大时的单调性、配置校验、重新加载失败时保留原配置。
- 模拟测试：随机撤回与恢复版本；不变量：每个新 Run 开始时 Session 的当前版本不是撤回的版本；投影与快照覆盖 `AgentSwitched`；PostgreSQL 差分测试。

## 8. 验证结果

- 单元测试：2 万个 EndUser 在 10% 灰度中分到约 10%（1800～2200 之间），同一用户结果不变，比例调到 30% 时原灰度用户全部仍在灰度中；白名单、配置校验、撤回、重新加载失败时保留原配置。
- 模拟测试：两个版本，随机在"灰度 50%""撤回灰度版本""全量后撤回旧版本"之间切换。300 个种子中发生 110 次切换，全部不变量成立；PostgreSQL 差分测试一致。变异检验：去掉 Submit 中的切换，被"新 Run 在撤回的版本上开始"不变量发现。
- 端到端（`serve`，修改文件不重启）：白名单用户分到灰度版本 2、其他用户为 1；把 2 加入 `withdrawn` 后 10 秒内重新加载，白名单用户的下一个 Run 切到 1 并记录 `AgentSwitched{from: 2, to: 1, reason: withdrawn}`；写入非法配置时被拒绝并保留原配置。

## 9. 单次调用的发布

Oneshot 定义与同目录的 `releases.yaml` 沿用同一结构，例如 `apps/xinli/oneshots/releases.yaml`。stable、canary（version、percent、end_users）与 withdrawn 的校验、散列分流、文件热加载共用 `internal/release`。按声明名与 EndUser 稳定分流，未配置发布时使用按文件名排序最后加载的版本。声明的全部版本只在启动时加载；每 10 秒热加载发布选择，错误配置保留原值，使用既有发布错误指标。

单次调用没有 Session：每次请求独立选择一次版本，修复调用保留本次选择。撤回只影响后续请求，不中断在途调用。同名各版本 concurrency 须一致，因为它们共享声明并发名额。发布前用 `make eval-xinli EVAL_FLAGS="-case oneshot-live-brief -oneshot-version <版本>"` 与基线比较，无回归后才能进入灰度或 stable；`-agent-version` 不影响单次调用。用量按 oneshot 的 name@version 汇总，和 AgentDef 归属分开。
