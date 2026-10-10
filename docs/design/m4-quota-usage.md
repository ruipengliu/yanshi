# M4 配额与用量计量

> 状态：已实现（模拟测试与 TokenHub 端到端验证，见 §7） · 依赖：章程 M4"配额计费"、ADR-0015（数据删除）· ADR-0019

## 1. 目标

为每条业务线控制成本上限，并防止单个 EndUser 滥用：记录每次模型调用与沙箱执行的用量，按价格表折算为金额，在业务线与 EndUser 两层施加配额。

不做：对外开票与支付（结算由业务线与平台线下进行，本设计只提供可核对的用量）；按 AgentDef 区分配额；预付费余额。

## 2. 计量

**Usage（用量）** 是一条记录：业务线、EndUser、类别（`model` / `sandbox`）、模型、输入与输出 token、沙箱执行毫秒数、折算金额、时间。

- **模型调用**：Run 的每次模型调用（含上下文压缩）在拿到回复后、写日志前记录。顺序如此是因为 token 在调用返回时已经消耗：之后写日志失败（Attempt 被接管、Session 被删除）也应计费。调用失败（无回复）不计费。
- **沙箱执行**：沙箱控制器每执行完一次调用，按执行时长记录；以 call_id 作为幂等键，同一调用被重新投递不重复计费。沙箱空闲保活的时间不计入（属于平台的预热成本）。
- **Call**：实时语音模型每轮交互的用量（文本与音频的输入、缓存、输出）在收到时记录（kind = `call`，[Call](./m3-call.md) §7）。价格表的条目可另配 `input_audio`、`output_audio` 单价，未配置时按文本单价计。开始 Call 时检查配额，通话中每轮之后复查，用尽时挂断。
- **不计量**：Memory 的嵌入向量（单价极低，后续需要时再加）；评测（不经业务线）。

记录失败（数据库不可用）时记日志与指标后继续：少计一次，而不是重新调用模型、多花一次钱。

用量独立于 Event 日志保存：Session 会被物理删除（ADR-0015），而结算需要在删除之后仍然可核对。为此用量记录**不含 Session ID、Run ID 与任何内容**，只有业务线、EndUser 与数量。注销 EndUser 时，其用量记录中的 EndUser 被替换为匿名值 `~`：业务线的总用量不变，也不再留存个人标识。

注销与在途计量的竞争按"先写、后查"处理：写入用量之后检查 Session 的删除记录，Session 已被删除（不论原因）时把这条用量匿名化。只看注销账号（`account_deletion`）不够：Session 可能先被单独删除、之后才注销账号，删除记录保留原来的原因，而持有它的 Worker 可能在注销之后才写入（模拟测试发现）。

### 价格表

金额的内部单位是**微元**（10⁻⁶ 元，整数），避免浮点误差。价格表是部署配置 `pricing.yaml`（`serve -pricing`）：

```yaml
currency: CNY
models:                         # 元 / 百万 token；数值等于"微元 / token"
  tokenhub/deepseek-v4.1-flash: {input: 1.0, output: 4.0}
sandbox:
  per_hour: 0.36                # 元 / 小时执行时间
```

`cached_input` 是命中提供商前缀缓存的输入单价（`Usage.cached_input_tokens`，TokenHub 在 `prompt_tokens_details.cached_tokens` 中报告）；不写时按 `input` 计，不假设折扣。用量记录与 `GET /v1/usage` 同时给出缓存命中的 token 数，指标 `yanshi_model_tokens_total{direction="cached_input"}`。

金额按记录时的价格计算，之后调价不影响已有记录。任一业务线配置了配额时，所有 AgentDef 引用的模型都必须有价格，否则启动失败：没有价格的模型等于不受配额约束。

## 3. 配额

配额写在业务线配置（`businesslines/*.yaml`）中，与保留策略并列：

```yaml
quota:
  monthly: 5000          # 元，业务线每个自然月
  end_user_daily: 2      # 元，每个 EndUser 每个自然日
  timezone: Asia/Shanghai  # 日、月的边界，默认 Asia/Shanghai
```

未配置的一层不限制。周期的边界必须与整点对齐（时区偏移为整小时），因为聚合按小时进行。

### 检查点

- **提交新 Run**：任一层已超额时拒绝，HTTP 429，`Retry-After` 为超额一层的重置时间（两层都超额时取较晚的一层：在它重置之前，另一层重置也无法恢复）。插话（Steer）不新建 Run，不检查。
- **每次模型调用之前**（含压缩）：已超额时 Run 挂起，`RunSuspended.reason` 为 `end_user_quota` 或 `business_line_quota`，`until` 为配额重置时间。GET Session 的 `waiting` 显示 `{"kind": "quota", "deadline": …}`。
- **恢复**：挂起的 Run 停放到 `min(重置时间, 当前 + 10 分钟)`。Worker 认领时若 Run 仍因配额挂起且仍超额，直接重新停放，**不写任何事件**，避免月度超额期间每 10 分钟往日志里写一对 AttemptStarted / RunSuspended。调高配额（重启生效）后 10 分钟内恢复。用户可以随时中断。

沙箱执行不单独检查：它由模型调用发起，而模型调用前已检查；一次执行的时长受调用超时限制。

### 超出的上限

检查与扣减不是原子的：同一业务线的多个 Run 可能同时通过检查。超出量的上限是"检查时正在进行中的模型调用"的费用之和，对 C 端的单轮调用而言很小。我们接受这个软上限，不做预留：预留需要预估单次调用费用，并在调用失败时退回，复杂度不值得。

## 4. 存储

```go
type Store interface {
    Record(ctx, *Entry) error                         // 以 ID 幂等
    Spent(ctx, businessLine, endUser string, from, to time.Time) (int64, error) // endUser 为空表示整个业务线
    Report(ctx, Query) ([]Row, error)                 // 按日 / EndUser / 模型汇总
    DeleteEndUser(ctx, businessLine, endUser string) error // 匿名化
    AnonymizeEntry(ctx, id string) error              // Session 删除后才写入的一条用量
    Prune(ctx, before time.Time) error
}
```

PostgreSQL 实现（迁移 `0007_usage.sql`）：

- `usage_entries`：明细，主键为 ID；用于幂等与报表。
- `usage_hourly(business_line, end_user, hour, shard)`：按小时的金额与 token 聚合；`end_user = ''` 的行是业务线合计。一次记录在同一语句中插入明细，插入成功时累加两行聚合。配额检查只读聚合：EndUser 一天至多 24 行，业务线一个月至多 744 × 16 行。

业务线合计行是热点：同一业务线的每次模型调用都要更新它，并发记录在这一行的行锁上排队。合计行因此按用量 ID 的散列分成 16 片（迁移 `0009_usage_shards.sql`），读取时求和；EndUser 行与匿名行只用第 0 片（同一 EndUser 的并发很低）。压测对比（P=4、W=32、S=256、开启配额检查，每次 30 秒约 1.65 万次记录）：记录语句平均耗时 0.275 → 0.09 ms，配额检查 0.055 → 0.082 ms（多读分片行），两者合计的数据库时间减少约 26%；吞吐不变（约 176 → 178 Run/s，数据库不是瓶颈）。

明细与聚合按保留期清理（`serve -usage-retention`，默认 400 天，覆盖年度对账），由 Janitor 执行。

## 5. 接口

- `GET /v1/quota[?end_user=]`：当前周期的限额、已用与重置时间（业务线与 EndUser 两层）。EndUser 令牌只能看自己。
- `GET /v1/usage?from=2026-10-01&to=2026-11-01&group_by=day|end_user|model[&end_user=]`：按业务线时区的日期汇总。EndUser 令牌只能看自己，且不能按 EndUser 分组。
- 指标：`yanshi_usage_cost_micros_total{business_line,kind}`、`yanshi_quota_rejections_total{scope,where}`、`yanshi_usage_record_errors_total`。业务线名不是个人数据，可作为标签。

## 6. 验证

- 一致性套件 `usagetest`：幂等、Spent 的时间范围、合计行、匿名化后合计不变、Prune。
- 模拟测试：开启配额与计量，配额设得很紧；不变量：
  - 每个 Session 已提交的模型 token 不超过已记录的 token（不会少计已写入日志的工作）；
  - 每层的已用金额不超过"限额 + Worker 数 × 单次调用费用上限"；
  - 因配额挂起的 Run 在额度恢复后都能结束；
  - 注销的 EndUser 不在用量记录中出现。
- 授权矩阵：两个接口加入 `authz` 测试。

## 7. 验证结果

- **模拟测试**：配额设得很紧（EndUser 每日 6000 微元，约 1～2 个 Run），并随机注销账号。300 个种子中因配额挂起 196 次、提交被拒 1453 次、挂起后恢复并完成 95 个 Run、注销 139 次，不变量全部成立；PostgreSQL 差分测试逐字节一致。变异检验：去掉模型调用前的配额检查、去掉写入后的删除检查，分别被"超额时记录了模型调用""注销账号的用量未匿名化"两条不变量发现。
- **模拟测试发现的问题**：最初只在删除记录的原因为 `account_deletion` 时匿名化。Session 先被单独删除、之后才注销账号时，删除记录保留原来的原因，持有该 Session 的 Worker 在注销之后写入的用量带着原 EndUser 留了下来。改为 Session 已删除（不论原因）时把这条用量匿名化（`AnonymizeEntry`）。
- **端到端**（TokenHub，`-auth jwt`，EndUser 每日 20 微元）：第一个 Run 完成并计量 36 + 81 token = 360 微元（与价格表一致）；随后提交返回 429，`Retry-After` 指向北京时间零点；`/v1/quota` 与 `/v1/usage` 数值一致。

- **开销**（`scripts/bench.sh`，P=4、W=32、S=256、POOL=20，压测模型每个 Run 2 次模型调用）：只计量约 180 Run/s，计量加配额检查（`QUOTA=1`，配额不触发）约 177 Run/s；此前不计量的结果为 188.8 Run/s（m2-scale-test），即每次模型调用多一次写入与两次聚合读取，吞吐下降约 5%。业务线合计行尚未成为瓶颈。

`pricing.yaml` 中 tokenhub 的价格是占位值，上线前按合同价填写。
