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
- **按住说话**：每次语音识别与播报记一条（kind = `speech`，[按住说话](./speech.md) §4）。识别的输入记为音频秒数，播报的输出记为字数，价格表按每百万秒、每百万字配置。请求前检查配额。
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
  tiers:
    free: {end_user_daily: 2}
    pro:  {end_user_daily: 10}
```

未配置的一层不限制。周期的边界必须与整点对齐（时区偏移为整小时），因为聚合按小时进行。

### 档位（Tier）

`usage.Limits.Tiers` 以档位名映射到 `usage.TierLimits`；名称须匹配 `^[a-z0-9_-]{1,32}$`，每档的 `end_user_daily` 必须显式大于 0、有限且能换算为正的微元整数。`Limits.Prepare` 在业务线加载时校验。只配置档位也算开启配额（`Limits.Enabled`），仍须为模型配置价格。

`Quotas.Status` 统一解析 EndUser 的档位，提交、模型调用、压缩、Call 与语音沿用这一入口。已分配且配置存在时只替换 EndUser 日限额；未分配时使用默认日限额。业务线月限额、周期与已用金额保持不变，升级不清零，降级不追溯。已存档位在配置中不存在时回落默认日限额（默认为 0 即不限），每次解析增加无标签指标 `yanshi_quota_unknown_tier_total`；删除档位配置前应先迁移已分配的用户。该指标不带业务线、EndUser 或档位名。

`usage.TierCache` 缓存分配结果（包括未分配），有效期为 5 秒，与 serve 的用量缓存一致。本进程 Put、Delete 和注销清理完成后立即失效，写入报错也失效，因为报错可能只是回包丢失。缓存用全局世代号保护：读取前保存世代，回填前在同一互斥锁下比较；期间发生写入就丢弃结果并重读。世代不按 EndUser 永久保存，注销不会留下个人标识的世代记录。其他进程的变更最多 5 秒后参与新检查，已在途检查仍可能沿用旧值，遵循 ADR-0019 的软上限；已挂起 Run 按既有周期复查，不因档位变化主动唤醒。

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

### 档位存储与删除

`usage.TierStore` 提供 Get、Put、Delete；未分配时 Get 返回空字符串，Delete 幂等。内存实现 `usage.MemTiers` 与 PostgreSQL 实现 `pgusage.Tiers` 经 `app.Stores` 构造，`app.NewCore` 将同一个档位缓存交给配额与 `service.Service`。档位属于配额配置的按用户分配，放在 `internal/usage`，不引入权益领域或 Session 数据。

迁移 `internal/pg/migrations/0025_end_user_tiers.sql` 创建 `end_user_tiers(business_line, end_user, tier, updated_at)`，主键为 `(business_line, end_user)`；在 `pg.Tables` 登记 EndUser 分片键。每次查询或写入只访问一个 EndUser，过滤业务线，不跨键事务。

`Service.PutTier` 先直接检查 EndUser 删除屏障，写入后总是经 `lifecycle.RecheckEndUser` 复查，包括写入返回错误但可能已提交的情况。复查与撤回使用不随请求取消的上下文；发现屏障就删除刚写的档位，不能确认屏障时也保守撤回。屏障读取不经档位缓存。`Service.DeleteEndUser` 在固定清理路径中删除档位，失败则不封口，所有调用方均生效，无须各自传入 remover。宽限期内的写入即使在落盘后、复查前进程崩溃，也由到期清理兜底：`app.NewCore` 将共享档位缓存登记到 `Core.EndUserData`，Janitor 调用 `PruneEndUsers`，逐条复核到期屏障，先经 `EndUserHolder.Delete` 清理档位，再移除屏障。清理幂等，每个 EndUser 一条档位 DELETE；某条失败就保留该屏障、下一轮重试，并继续其他候选。

PG 在单个 EndUser 的事务中锁住并复核屏障，清理回调经 `pg.Exec` 沿用同一连接池的事务，再删除屏障；失败回滚，不将多个 EndUser 放入同一事务。内存实现用同一把屏障锁串行化清理与删除，回调不重入 Deletions。这样其他 Janitor 的旧候选不会清除已经复用 ID 的新档位。清理回调中的缓存失效可能早于 PG 提交，Janitor 在事务结束后再经 `EndUserHolder.Invalidate` 失效一次，仍按世代防止提交前读回填旧值。

屏障只有在到期清理成功后才会消失；之后允许复用同一 EndUser ID，从默认档位开始。

## 5. 接口

- `GET /v1/quota[?end_user=]`：当前周期的限额、已用与重置时间（业务线与 EndUser 两层）。EndUser 令牌只能看自己。EndUser 层的 `tier` 只在有效分配时返回，默认或未知档位回落时省略。
- `PUT /v1/end_users/{id}/tier`：JSON `{business_line?, tier}`；成功返回 `{"tier":"pro","limit_micros":10000000}`。`limit_micros` 为有效 EndUser 日限额。只允许服务令牌，用户令牌返回 403；业务线取自令牌，自报不符返回 403。开发模式须在 JSON 或查询参数中指定业务线，两处冲突返回 400。
- `DELETE /v1/end_users/{id}/tier`：恢复默认，成功返回 `{"tier":"","limit_micros":2000000}`；默认限额 0 表示不限。只允许服务令牌，开发模式经查询参数指定业务线，与注销接口相同。
- 设置未知档位返回 400；业务线未开启配额时两个档位写接口均返回 400（档位不会产生效果）。Put 遇到 EndUser 删除屏障返回既有屏障错误码 404；配置或存储故障照现有错误路径处理。接口沿用配额与注销的普通 JSON，不新增 proto 消息。
- `GET /v1/usage?from=2026-10-01&to=2026-11-01&group_by=day|end_user|model[&end_user=]`：按业务线时区的日期汇总。EndUser 令牌只能看自己，且不能按 EndUser 分组。
- 指标：`yanshi_usage_cost_micros_total{business_line,kind}`、`yanshi_quota_rejections_total{scope,where}`、`yanshi_usage_record_errors_total`。业务线名不是个人数据，可作为标签。

## 6. 验证

- 一致性套件 `usagetest`：幂等、Spent 的时间范围、合计行、匿名化后合计不变、Prune。
- 模拟测试：开启配额与计量，配额设得很紧；不变量：
  - 每个 Session 已提交的模型 token 不超过已记录的 token（不会少计已写入日志的工作）；
  - 每层的已用金额不超过"限额 + Worker 数 × 单次调用费用上限"；
  - 因配额挂起的 Run 在额度恢复后都能结束；
  - 注销的 EndUser 不在用量记录中出现。
- 档位一致性套件 `tiertest`：覆盖幂等覆盖、删除、复用与业务线/EndUser 隔离；内存与 PG 实现共用，PG 由 `YANSHI_TEST_PG` 启用。
- 档位缓存测试：5 秒边界、本进程写后立即可见、Put/Delete（含已提交但报错）与在途读取交错不回填旧值。
- 到期清理一致性测试：未到期不调用、分页、清理失败保留屏障、其他候选继续、重试成功；两种 lifecycle 实现共用。PG 回归覆盖单连接池沿用同键事务、失败回滚，以及提交前回填旧缓存后再次失效。
- 档位删除测试：注销在写前、写后、回包丢失与请求取消时交错；清理失败不封口，宽限期后复用 ID 不继承旧档位。sim 覆盖 `TiersSet`、`TiersWithdrawn`，删除不变量直接查底层存储，档位加入 PG 差分指纹。新增 `TierWriterCrashes`、`TierCrashRecoveries`，覆盖 Put 落盘后跳过复查；收敛时显式拨钟到全部宽限期结束，屏障移除后检查没有残留档位，再回到原循环处理拨钟后新到期的定时。崩溃只在故障模式注入。档位模拟由 `Options.Tiers` 独立开启，档位选择使用直接从种子派生的独立随机流；未开启时保持原有额度、六种注销写入及其抽样顺序，也不增加到期拨钟。`TestTiersWithFaults` 单独要求全部档位覆盖，原综合场景仍保留包括 `CardAnswers` 的全部既有覆盖；PG 差分显式开启档位。
- 授权矩阵覆盖用量与档位接口；真实 HTTP 测试覆盖设置、查询、超默认但未超档位额度时配额检查放行、恢复默认、用户令牌拒绝与注销清理。

档位相关测试已在 Mac mini 通过 `make check`（含 PG）与 `make sim`（2026-10-28）；下节是此前计量与配额的验证结果。

## 7. 验证结果

- **模拟测试**：配额设得很紧（EndUser 每日 6000 微元，约 1～2 个 Run），并随机注销账号。300 个种子中因配额挂起 196 次、提交被拒 1453 次、挂起后恢复并完成 95 个 Run、注销 139 次，不变量全部成立；PostgreSQL 差分测试逐字节一致。变异检验：去掉模型调用前的配额检查、去掉写入后的删除检查，分别被"超额时记录了模型调用""注销账号的用量未匿名化"两条不变量发现。
- **模拟测试发现的问题**：最初只在删除记录的原因为 `account_deletion` 时匿名化。Session 先被单独删除、之后才注销账号时，删除记录保留原来的原因，持有该 Session 的 Worker 在注销之后写入的用量带着原 EndUser 留了下来。改为 Session 已删除（不论原因）时把这条用量匿名化（`AnonymizeEntry`）。
- **端到端**（TokenHub，`-auth jwt`，EndUser 每日 20 微元）：第一个 Run 完成并计量 36 + 81 token = 360 微元（与价格表一致）；随后提交返回 429，`Retry-After` 指向北京时间零点；`/v1/quota` 与 `/v1/usage` 数值一致。

- **开销**（`scripts/bench.sh`，P=4、W=32、S=256、POOL=20，压测模型每个 Run 2 次模型调用）：只计量约 180 Run/s，计量加配额检查（`QUOTA=1`，配额不触发）约 177 Run/s；此前不计量的结果为 188.8 Run/s（m2-scale-test），即每次模型调用多一次写入与两次聚合读取，吞吐下降约 5%。业务线合计行尚未成为瓶颈。

`pricing.yaml` 中 tokenhub 的价格是占位值，上线前按合同价填写。
