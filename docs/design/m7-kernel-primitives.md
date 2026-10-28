# M7 内核补充：Capture 正文存储、擦除、交付确认、定时、出站流、单次调用

> 状态：设计（2026-10-27），经 Codex 两轮评审后修订（[评审记录](../review/2026-10-27-persona-architecture-review.md)）；交付确认（§11）与 EndUser 删除屏障（§12）已实现，其余未实现 · **§2–§4、§11 由 [ADR-0050](../adr/0050-capture-text-in-the-business-line.md) 取代**（旁听原文存放在业务线；§11 按其退役条件删除），§5 的 `window.stream`、§6 的擦除与修订信封不做 · ADR-0044（已取代）、ADR-0045、ADR-0046、ADR-0047、ADR-0049（已取代）、ADR-0050 · 来由：[xinli 与 persona agent 的整体架构](./persona-architecture.md) §2.3 · 依赖：[M0 核心原语](./m0-core-primitives.md)、[旁听](./captures.md)、[输入、触发与外部循环](./m5-inputs-triggers-loops.md)、[Session 生命周期](./m2-session-lifecycle.md)（ADR-0015）、[Memory 与 Grant](./m4-memory-grant.md)、[配额与用量](./m4-quota-usage.md)、[模型调用](./model-calls.md) · 术语：`CONTEXT.md` 的 Erasure、CaptureCorrection、Ack、Schedule、Outbox、Oneshot、Tier、Redo

xinli 落地需要内核补上的原语。每一项都按[整体架构](./persona-architecture.md) §9 的判据给出理由：第二条业务线也会需要，并且业务侧做不到或做了不安全。业务语义（交谈、承诺、项目、人）一律不进内核。

## 1. 总览

> ADR-0050 之后，下表中 §2、§3、§4、§11 的行不再实施（§11 已实现的部分按退役条件删除），原文、擦除与纠错由业务线负责；内核新增的可信结构化片段、写日志前审核、擦除即删除工作 Session 与绝对删除期限见 ADR-0050 与[整体架构](./persona-architecture.md) §11。

| § | 原语 | 判据 | 阶段 |
|---|---|---|---|
| 2 | Capture 正文存储；`captures` 结果只存引用；统一展开器；读取 API | 原文要能物理擦除且只有一份；100 万 DAU 时每天约 500 GB 原文 | P1 |
| 3 | 擦除（Erasure） | 部分删除要物理删除，业务侧碰不到日志 | P1 |
| 4 | 原文修订（CaptureCorrection） | 日志只追加，业务侧改不了原文 | P1 |
| 11 | 交付确认（Ack） | 读取位置现在随 Run 完成提交，"读过"不等于"业务已保存"；只有平台能决定位置 | P1 |
| 12 | EndUser 删除屏障 | 注销期间按 EndUser 的写入（新 Session、用量、档位）可能复活数据 | P1 |
| 6 | 出站流（Outbox） | 每个 Session 一条 SSE 撑不住；擦除要可靠地通知 Persona | P1 |
| 5 | 定时（Schedule） | 业务侧提交的输入在投影中是用户原话 | P2 |
| 7 | 单次调用（Oneshot） | 业务侧直连模型会绕过计量、内容安全与评测 | P3 |
| 8 | 档位（Tier） | 触发与定时的 Run 没有用户令牌，配额只能在内核执行 | P3 |
| 9 | 重答（Redo） | 只有平台能让 Transcript 略去被重做的 Run | P3 |
| 10 | 说话人引用（`speaker_ref`） | 契约字段 | P3 |

触发（ADR-0040）按 [m5](./m5-inputs-triggers-loops.md) §6 实现，放在 P2，本文不重复。

## 2. Capture 正文存储

### 2.1 为什么移出日志

- **擦除。** 擦除要改动原文，而事件日志只追加（ADR-0004）。原文在单独的存储中，擦除只改那里，日志依然不可变。现在 `captures` 的工具结果在日志中复制了一份原文，也要去掉。
- **容量。** 正文占旁听 Session 日志的绝大部分；与元数据分开后，日志、快照与回放都随之变小。
- **先例。** 工件就是这样做的：Event 中只放 `artifact://` 引用（[工件](./m2-artifacts.md)）。日志决定有哪些片段、谁说的、何时说的、修订到第几版、是否被擦除；存储只保存文字。"Event 日志是唯一事实源"仍然成立。

### 2.2 存储

```sql
-- 分区父表：按 Session 创建日期按天分区；分片键为 session_id（ADR-0028）。
CREATE TABLE capture_segments (
    session_id  text   NOT NULL,
    seq         bigint NOT NULL,   -- CaptureAppended 或 CaptureCorrected 的事件序号
    idx         int    NOT NULL,   -- 段序号，与引用编号 #seq.idx 一致
    version     int    NOT NULL,   -- 0 为原文，修订依次加一（§4）
    day         date   NOT NULL,   -- 分区键：Session 创建的日期
    text        text   NOT NULL,   -- 擦除后为空
    PRIMARY KEY (day, session_id, seq, idx, version)
) PARTITION BY RANGE (day);
```

- 契约：`CaptureSegment.text` 在日志中不再填写（字段编号保留，`buf breaking` 通过；旧日志不再读取，见 §15）；提交接口的 `text` 不变，由 service 拆出写入存储。`CaptureAppended` 只含说话人、`speaker_key`、`speaker_ref`、时间、`redacted`。
- **写入**：`service.SubmitCaptures` 的决策中，事件与片段在同一事务中写入（同一 Session 键、同一分片），经 `lifecycle.Recheck` 先写、后查删除记录。
- **读取**：capture 包中新的 `Store` 接口，内存与 PostgreSQL 两种实现，通过新的一致性套件 `capturetest`；在 `app.Stores` 中构造、在 `app.SessionData` 登记，接入 `internal/sim` 的差分测试与删除不变量。

### 2.3 删除与分区

- **删除仍按 Session 精确执行。** Retention 到期或用户删除时，Janitor 照常逐 Session `DELETE` 片段（ADR-0015 的语义不变：删除记录写入即对 API 不可见，Janitor 随后物理删除）。
- **分区只用于回收空间。** 一个分区中的 Session 全部删除后，Janitor 整片删除它，死元组不靠 vacuum 回收。"物理删除"分两层写进部署文档：行在 Janitor 完成时对任何读取都不存在；磁盘空间在分区删除时回收（MVCC 旧版本在此之前仍占空间，备份同 ADR-0015）。
- **分区寿命有上界**：声明了流的 AgentDef 加载时校验 Retention 必须有 `close_after_open`（新增：自创建起多久强制关闭，旁听 Session 设为 72h）与有限的 `delete_after_close`，所以只有短期保留的 Session 写入这张表，一个分区不会被长期保留的 Session 拖住。
- 这是缓解，不是容量问题的终点：逐行 `DELETE` 仍有 WAL 与索引成本，按 §13 的压测确定单分片容量。

### 2.4 统一展开器

`captures` 工具结果中的旁听片段内容块只带引用与元数据，不带文字。凡是需要文字的地方，都经同一个展开器（capture 包中新的 `Expander`）解析，不各自读存储：

| 使用方 | 现在 | 改为 |
|---|---|---|
| Transcript 组装请求（`model.ExpandCaptures`） | 读内容块中的文字 | 经展开器 |
| 来源推导（`session.Provenance` 的旁听片段，`provenance.go` 的 `resultPieces`） | 读内容块中的文字 | 产出引用；写入闸门与注入判断经展开器取文字 |
| 注入判断的缓存 | 按 `call_id` | 按 `call_id` 与该结果中片段的版本；擦除或修订后重新判断 |
| `cite_captures`、读取 API、单次调用 | — | 经展开器 |

- **按日志前缀解析。** 展开器的输入是"日志前缀 + 引用"：版本取该前缀中可见的最新 `CaptureCorrected`，前缀中有覆盖它的 `CaptureErased` 时一律返回占位，即使存储尚未清空。所以同一个日志前缀总是展开出同样的文字，模型请求、闸门与判断看到的一致，`Worker.Step` 的确定性不变。
- **"请求前缀稳定"的显式例外**：擦除与修订会改变已发出的内容，使该 Session 之后的请求前缀变化一次。它们都很少；评测报告的"缓存命中"列照常监控。

### 2.5 读取 API

`GET /v1/sessions/{id}/captures?after=<位置>&limit=`：用户令牌与服务令牌都可读（授权与 `events` 相同），经展开器返回片段（引用、说话人、时间、文字或占位、是否修订过）。App 展示原文、BFF 的实况预览用它。加入 `authz_test.go` 的矩阵。

## 3. 擦除（Erasure）

**接口**：`POST /v1/sessions/{id}/captures/erase`，`{erase_id, from: "#57.0", to: "#63.4"}`（同一 Session 内的引用范围，含两端），用户令牌或服务令牌。Connection 与三个 SDK 同步提供（ADR-0024、ADR-0026）。

**顺序**（与 ADR-0015 的"先记、后清"相同）：

1. 决策：校验范围、按 `erase_id` 去重，追加 `CaptureErased{erase_id, from, to}`；**同一次决策中中断该 Session 的活跃 Run**（如果有）。Session 已关闭也可以擦除。
2. 物理清空：`capture_segments` 中范围内所有版本的 `text` 置空。失败时由 Janitor 按日志中的擦除事件重试（持有者登记"待清空的擦除"）。
3. 结果：`{erased: 段数, memories: [{id, content}]}`。`memories` 是本 Session 写入的、依据可能落在擦除范围内的有效 Memory（用写入闸门的依据判定对照被擦除的佩戴者原话），由 App 询问用户是否删除；内核不自动删除，同一条 Memory 可能还有其他依据。
4. 出站流发出 `capture_erased`，信封带 Session、范围与 `erase_id`（§6）。

**为什么中断活跃 Run，以及为什么不必重建压缩。** 声明了流的 AgentDef 加载时校验必须 `context.scope: run`（xinli-day 已是）：每个 Run 只看本轮，之前 Run 的回复、调用参数与压缩摘要都不进入之后的上下文。所以擦除之后，唯一可能还带着被擦除内容的上下文就是活跃 Run（含它的压缩摘要），中断它即可；之后的 Run 从读取位置重新读，看到占位。被中断的 Run 已经交给业务线的内容，由 Persona 按 §3 的跨层契约处理。

**跨层契约**（Persona 实现，[整体架构](./persona-architecture.md) §3.2）：

- Persona 收到 `capture_erased` 后，在同一事务中记下擦除墓碑（Session、范围）、按依据反向索引删除只依据被擦除片段的对象、把依据部分被擦除的对象标为不可读并移出检索向量。
- 变更集提交的事务内检查墓碑：引用落在墓碑范围内的操作被拒绝。迟到的提交（引用核对在擦除之前、落库在之后）因此要么在墓碑之前落库、被随后的级联删除，要么在之后、被拒绝。
- 擦除到 Persona 生效之间有一个出站流消费延迟的窗口，这期间 Persona 仍可能返回派生内容；BFF 在发起擦除后立即在 App 中隐藏相关对象。延迟上界计入出站流的监控。
- 渲染：`captures`、读取 API 显示 `[已删除]`；`cite_captures` 对它们返回 `{ref, erased: true}`，不给说话时间。

产品的隐私说明写明："删除这段谈话后，原文立即删除；基于它的整理结果随后删除。"

## 4. 原文修订（CaptureCorrection）

**接口**：`POST /v1/sessions/{id}/captures/corrections`，`{correction_id, ref: "#57.3", text}`，用户令牌（本人纠错）或服务令牌（应用层代为提交）。

- 决策：校验引用存在且未被擦除；内容安全检查新文字（ADR-0021，违规即拒绝）；追加 `CaptureCorrected{correction_id, ref, version}`，新文字写入 `capture_segments` 的新版本，同一事务。
- 说话人不变：他人的话被用户改过仍是他人的话（来源只看说话人字段，ADR-0039）。
- 渲染：展开器取前缀中最新的版本，行尾标 `（已更正）`；依赖旧版本的注入判断重新做（§2.4）。
- 已完成的整理不会自动重做：出站流发出 `capture_corrected`，由应用层决定是否提交重新整理（变更集按来源重交即整体取代，ADR-0042）。

## 5. 定时（Schedule）

### 5.1 为什么在内核

按钟点做的事（每天整理、每周回顾、心力优先、用户要求的"每周五给我周报"）由业务线到点以服务令牌提交时，输入在投影中是用户原话：Agent 根据外部内容写下的定时文字，到期后会被当作用户亲口说的，绕过写入闸门（ADR-0037 拒绝"推入"的同一理由）。窗口中的静默判断要用流的写入时间，业务侧拿不到。内核的定时让到期输入的来源为"平台"，并复用后台调用与触发的停放唤醒。

这修改了 ADR-0040 第 5 条：那里担心的"EndUser 的时区与一天的边界"由创建方显式给出，内核不知道"一天"；定时属于一个 Session，只在本 Session 内开启 Run。

### 5.2 声明

```
Schedule {
  schedule_id,                       // 创建方给出，按它去重
  input: [ContentBlock],             // 到期时的输入文字；只允许 Text
  time_zone: "Asia/Shanghai",        // 必填，IANA
  at?: "2026-10-30T17:00",           // 一次性：当地时间
  rrule?: "FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0",   // 周期：RRULE 子集（DAILY、WEEKLY、MONTHLY，BYDAY、BYHOUR、BYMINUTE、COUNT、UNTIL）
  window?: {quiet: 2h, stream: "transcript", latest: "04:00", jitter: 30m},
  until?: 时间戳
}
```

- `window`：从每次的时刻起，流上静默满 `quiet` 即执行；到当地 `latest` 仍未满足时，在 `latest` 起的 `jitter` 内按 `schedule_id` 与日期派生的确定性偏移执行。参数都由创建方给出（xinli 的当天整理规则，[建模](./xinli-model.md) §12）。
- **时区规则**：当地时刻不存在（夏令时跳过）时顺延到之后第一个存在的时刻；重复时取第一次。每次解析出的 UTC 时间写入事件，重放不依赖升级后的时区数据库。
- 每个 Session 至多 50 个有效定时；`rrule` 的最小间隔为 1 小时。

### 5.3 接口

| 入口 | 说明 |
|---|---|
| 内置能力 `schedule` | `{action: create \| list \| cancel, ...}`，由能力白名单授予；结果中写明下次执行时间 |
| HTTP | `POST /v1/sessions/{id}/schedules`、`GET`、`DELETE /v1/sessions/{id}/schedules/{schedule_id}`：用户令牌与服务令牌 |

### 5.4 语义

- **事件**：`ScheduleCreated{..., next_due}`、`ScheduleCancelled`、`ScheduleFired{schedule_id, due_at, next_due}`、`ScheduleSkipped{schedule_id, due_at, reason, next_due}`。`next_due` 是解析好的 UTC。投影中的定时（拟名 `Schedules`）只从事件读取下次到期，递增 `ProjectionVersion`。
- **执行是原子的**：`ScheduleFired` 与带定时标记的 `RunRequested` 在同一次追加中写入；输入 ID 由 `schedule_id` 与 `due_at` 派生，两个 Worker 同时判断也只生效一次。时间取自 `clock.Clock`，判断在 `Worker.Step` 的同步路径中。
- **跳过也落事件**：超额（准入或配额）、窗口期间 Session 关闭时写 `ScheduleSkipped`，重启后不会重做这一次。
- **有活跃 Run 时**：等它结束再执行，不插话。
- **来源是平台**：与触发相同，写入闸门不把它当作依据；客户端提交的输入不能带定时标记（统一检查点拒绝，`Apply` 再校验）。
- **错过**：晚于 `due_at` 的，晚多少执行多少；周期定时只执行最近一次到期，跳过的写 `ScheduleSkipped`。
- **停放**：到期在数小时之内的，沿用 `workqueue.Park`；更远的写入独立的到期表（按到期时间索引，同一分片），到期前由 Janitor 在同一事务中移回工作队列。唤醒、取消、Session 删除与迁移在同一事务中处理，`workqueuetest` 覆盖这些交错。原因：现在的认领按优先级扫描、再过滤停放项（`pgqueue.Claim`），百万级的长停放会拖慢认领。
- **删除**：定时在日志中，随 Session 删除；到期表在 `app.SessionData` 登记。

## 6. 出站流（Outbox）

### 6.1 形态

业务线服务端用一个游标拉取本业务线所有 Session 的选定事件，替代每个 Session 一条 SSE：

`GET /v1/outbox?shard=<n>&after=<游标>&kinds=run_completed,question&limit=500`，服务令牌，长轮询（无新事件时最多挂起 25 秒）。

```
{events: [{cursor, session_id, end_user, agent, seq, kind, run_id?, call_id?, range?}], next: <游标>}
```

- **只含 ID 与范围**：不含回复文字与调用参数。需要内容时按 `session_id` 与 `seq` 读事件。
- **种类**：`run_completed`、`run_failed`、`question`、`approval_requested`、`background_settled`、`capture_erased`（带范围）、`capture_corrected`、`schedule_fired`、`session_closed`、`session_deleted`（带原因）。由平台登记，不暴露内部事件。
- **分片**：每个 PostgreSQL 分片一条流（ADR-0028），业务线按分片并行拉取；`GET /v1/outbox/shards` 列出分片。
- **至少一次**：消费方按 (`session_id`, `seq`) 去重；游标由消费方保存。
- **提示**：可登记 webhook，有新事件时收到不含内容的"分片 n 有新数据"。

### 6.2 不漏：按事务可见性水位发布

`bigserial` 的顺序不是提交顺序：事务 A 取得 101 未提交、B 的 102 先提交，消费者推进到 102 后，A 的 101 就被跳过。所以：

- 每行记写入事务的 `pg_current_xact_id()`（xid8），按 (xid8, id) 排序，游标是 (xid8, id)。
- 读取只返回 `xid8 < pg_snapshot_xmin(pg_current_snapshot())` 的行：水位之下的事务都已结束，之后提交的行的 xid 必然不低于水位，游标因此单调、不漏（[PostgreSQL 快照函数](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-TXID-SNAPSHOT)）。
- 代价是长事务会拖住水位，延迟随之增加；内核事务都短，另设"水位滞后"的指标与告警。
- 出站记录在追加事件的同一事务中写入（`session.Store.Update` 的决策产生登记的种类时附带一行）。

### 6.3 保留、删除与对账

- 一般的出站行在水位之下停留满 72 小时后删除；Session 删除时随之删除（在 `app.SessionData` 登记）。
- **删除信封不随 Session 删除**：`session_deleted` 与 `capture_erased` 的出站行只含 ID 与范围，照常保留 72 小时。Janitor 删除日志与索引之后（`internal/janitor`），它们是消费方得知删除的唯一途径。
- **游标过期**（消费方离线超过 72 小时）：返回明确的错误。对账：删除记录本来就持久保存（ADR-0015），新增 `GET /v1/deletions?since=` 按业务线列出；仍存在的 Session 由 `GET /v1/sessions?updated_since=` 列出后重读事件。擦除对仍存在的 Session 可从其日志中的 `CaptureErased` 重新得到；对已删除的 Session，删除本身就覆盖了擦除。
- 一致性套件 `outboxtest`；`internal/sim` 检查：日志中每个登记种类的事件恰有一行出站记录；乱序提交不漏；删除后只剩删除信封。

### 6.4 SSE 仍保留

App 内的实时体验仍走 Connection 与 SSE。出站流服务于服务端的编排与渠道转发，是"每个事件至少一次"，不是实时增量。

## 7. 单次调用（Oneshot）

### 7.1 为什么

实况预览的简述、标题、纠错建议、Persona 的嵌入都是"模型函数"：一次输入、一次输出、没有工具与状态。用 Run 做，每次都要建 Session、写十几条事件；业务侧直连提供商，又绕过了计量、配额、内容安全与换模型的评测（章程 G9、原则 10）。

### 7.2 声明

`oneshots/*.yaml`，与 AgentDef 同样版本化、经发布配置灰度（ADR-0020）：

```yaml
name: xinli-live-brief
version: "1"
kind: generate                 # generate | embedding
model: ark/doubao-seed-2-1-lite-260915:minimal
instructions: |
  用两三句话概括这段旁听正在谈什么……
input: {max_chars: 8000}
output:                        # 可选：JSON Schema，平台校验，不合格时重试一次后失败
  type: object
  properties: {brief: {type: string}}
max_output_tokens: 300
work_class: interactive        # interactive | background
```

### 7.3 接口与语义

`POST /v1/oneshots/{name}`，`{end_user, input: [ContentBlock] | captures: {session, from, to}}`，服务令牌或用户令牌：

- **输入**：文字，或一段旁听的引用范围；后者经展开器读出并按说话人标注（调用方须有该 Session 的权限）。
- **不写日志、不存结果**：日志与指标只记 ID、名称、版本与 token 数（ADR-0015）。结果交给调用方后，调用方不得保存他人原话的派生内容超过原文的保留期（接入文档写明）。
- **计量与配额**：照常计量到业务线与 EndUser（ADR-0019），超额拒绝；写入受 EndUser 删除屏障约束（§12）。
- **内容安全**：输入与输出都在返回之前检查，失败即拒绝（ADR-0021）。
- **准入**：与 Run 的模型调用共用提供商的并发与限流（[模型调用](./model-calls.md)）。为了不挤占问答：每个单次调用声明各自的并发子限额，业务线的单次调用合计不超过提供商预算的配置比例；后台工作保有最低份额。
- **嵌入**：`kind: embedding` 返回向量与模型名，复用 `Embedder`。
- **评测**：`evals/` 增加单次调用的用例形态，改指令与模型须与基线比较。

## 8. 档位（Tier）

- 业务线配置：

```yaml
quota:
  monthly: 50000
  end_user_daily: 2        # 未设档位的 EndUser
  tiers:
    free: {end_user_daily: 2}
    pro:  {end_user_daily: 10}
```

- 档位按 EndUser 保存在内核：`PUT /v1/end_users/{id}/tier`，`{tier}`，只允许服务令牌。新表 `end_user_tiers(business_line, end_user, tier, updated_at)`，分片键为 EndUser（`pg.Tables` 登记），注销账号时删除，写入受 §12 的屏障约束。
- 准入时按 EndUser 的档位取日配额，档位读缓存（与配额缓存同一节奏）。
- 不用令牌声明：触发、定时、委托开启的 Run 没有用户令牌，准入时拿不到档位。

## 9. 重答（Redo）

- 输入：`POST /v1/sessions/{id}/inputs`，`{redo: true}`，不带内容。只针对本 Session 中最后一个已完成的 Run。
- 拒绝（conflict）的情形：最后一个 Run 不是 completed；它执行过非只读的调用（需要审批、未声明只读的能力，以及 `memory_save`、`schedule` 等内置写入）；它开始之后发生过上下文压缩（摘要中有旧回答，不做摘要重建）；Session 是三方入站的（[Agent 生态](./m7-agent-ecosystem.md) §3.2）。
- 写入：`RunRequested{redo_of: <run_id>}`，输入与来源字段（`from_call` 等）由平台从被重做的 Run 复制。`redo_of` 只能由平台写入（统一检查点拒绝客户端提交，`Apply` 再校验）。
- Transcript：被重做的 Run 的回复与调用不进入之后的上下文；连续重答时只保留最后一次。日志保留全部，App 可以展示"第 2/3 个回答"。
- 召回照常重新做一次（记为 Event）。

## 10. 说话人引用（`speaker_ref`）

`CaptureSegment` 新增 `string speaker_ref = 7`：采集服务比对声纹后填入的不透明 ID（业务线的实体 ID），`[A-Za-z0-9_-]{1,64}`。

- 内核不验证、不解析它。来源推导只看 `speaker`，被认出的他人仍是他人（ADR-0039 不变）。
- 给模型看的文本：`#57.4 10:16 [他人 other-1 @P-0012]`；`cite_captures` 的 `_meta` 附上它。
- 只有佩戴者以外的片段可以带。

## 11. 交付确认（Ack）

> xinli 已在 P1 第 2 步退役交付确认：xinli-day 版本 2 不声明流，Persona 关闭 `acks`、不再输出回执，改在接受范围变更集的同一事务中推进业务日处理进度（ADR-0050、Q39）。以下保留内核旧路径的说明，等 P1 第 5 步删除。

### 11.1 问题

读取位置在 `RunCompleted` 时提交本 Run 读到的最后一页（[旁听](./captures.md) §5）。工具调用失败只记为一条错误结果（`Worker` 的调用执行），模型仍可正常结束 Run；业务线拒收时，模拟服务甚至返回成功的 MCP 响应、只在文字中写明拒绝。于是"读页成功 → 交出失败 → 模型回复'稍后再试' → Run 完成"会让读取位置越过没有保存的时段，之后的 `after: "new"` 再也读不到它。读取位置的契约本身没有错，错在把"读过"当成"业务已保存"。

### 11.2 设计

- **业务线给出结构化回执。** 业务线保存一个变更集后，在 MCP 结果中附 `_meta["yanshi/ack"] = {stream, from, through}`：它已持久化了这个流上 `[from, through]` 的范围。拒收、部分保存不附回执。回执是业务线对平台的声明，不是给模型看的文字。
- **AgentDef 声明需要确认的流**：`streams[].ack: true`。声明后，读取位置只按回执推进：
  - 平台在 `ToolResult.CaptureAck` 中记下回执（契约见 `proto/yanshi/v1/event.proto` 的 CaptureAck）。只采信同一业务线、登记中声明了 `acks: true`（`mcpcap.Server.Acks`）的 MCP Server 的调用结果；`session.CheckCaptureAck` 与 `State.Apply` 校验。回执不进入模型上下文；
  - `CaptureRead.Start` 记录本页实际开始读取的游标（`after: "new"` 已解析为提交位置，从头统一为 `"0"`），`first` 记录本页第一段的 Ref；`ack` 记录这次读取使用的 `agentdef.Stream.Ack` 声明。Run 保留所有需要确认的页（`session.Run.CapturePages`）与本 Run 的回执（`session.Run.CaptureAcks`），快照一并保存；
  - `RunCompleted` 时，从已提交位置起，在本 Run 的页中找 `start` 等于当前位置、且被同一回执完整覆盖 `[first, after 的 Last]` 的页；若有多个，取页尾最远的，再从该页尾继续。找不到便停止，提交最后一页真实的完整游标及其时段间隔。以页拼链，不用 Ref 的序号相邻性推断连续：两批 Capture 之间可能夹着别的 Event。回执越过实际读取范围，也只能提交到被覆盖的已读页尾；
  - 没有新内容的时段也要交出（空变更集，xinli-day 已经每个时段一次 `file_changes`），否则前缀断开。
- **未声明 `ack` 的流**：行为不变。
- 切换前的 `xinli-day` 版本 1 声明 `ack: true`；此用法现已退役。

### 11.3 语义核对

- 平台不判断业务线是否真的保存了：回执是业务线的责任；平台保证的是"没有回执的范围不会被跳过"。
- 确定性：回执随 `ToolResult` 写入日志，投影只从日志计算前缀。
- 与触发的关系：读取位置没有前进时，触发的"同一位置只触发一次"仍按最后一个 Capture 的序号判断；未确认的时段由下一次触发（有新的 Capture 时）或当天整理补上。

## 12. EndUser 删除屏障

注销账号（`DELETE /v1/end_users/{id}`）期间，按 EndUser 写入的地方可能让数据复活：新建 Session、单次调用的用量（计量现在只在有 Session 时复查删除，`usage.Meter`）、档位。

- 注销先写 EndUser 删除记录（按业务线隔离，分片键为 EndUser），再清除；所有按 EndUser 的写入入口"先写、后查"这条记录（与 `lifecycle.Recheck` 同一写法）：新建 Session、计量、档位、单次调用。
- 屏障在删除执行期保存，全部清除完成之后再保留宽限期（默认 24 小时，经 `app.Options.EndUserGrace` 配置），由 Janitor 删除到期记录；之后同一 EndUser ID 可以重新使用。失败或尚未完成的注销不开始倒计时，重复注销复用原屏障。业务线须先使用户令牌失效，宽限期用于拦截注销前已在途、注销后才落盘的请求。Session 删除记录仍不保存 EndUser，屏障只在执行期与宽限期内保存它。
- 在 `internal/sim` 的删除不变量中检查：注销与这些写入任意交错，结束后不留实名数据。
- 随 P1 落地：它也覆盖新建 Session 这个现有入口，不等到 P3 的档位与单次调用。

## 13. 容量

在[整体架构](./persona-architecture.md) §7 的基础上，内核的验收按峰值而不是全天平均：

- **Capture 写入**：旁听集中在约 8 小时，按每分钟一批，峰值约 1700（10 万 DAU）/ 17000（100 万）批每秒；每批一次事件加至多 200 行片段。整批被内容安全拦下时逐段复核，调用数放大到段数。
- **单分片容量**：`app.Postgres` 现在是单个连接池，ADR-0028 只定了分片键。超过单分片容量之前，分片路由是前置条件（P4 验收），包括故障切换。
- **压测项**：P1 的片段写入与逐 Session 删除（含 WAL 与索引）；P2 的百万停放与到期表迁移；单次调用的子限额下问答的首字延迟。

## 14. 不变量核对

| 不变量 | 本设计中 |
|---|---|
| Event 日志是唯一事实源 | 片段、版本、擦除、回执、定时的到期都由日志决定；存储只保存文字，展开以日志前缀为准（§2.4） |
| 改 `Apply` 或 `State` 递增 `ProjectionVersion` | 擦除范围、版本、回执、定时、`redo_of` 都改投影 |
| 契约在 `proto/`，只做向后兼容的修改 | 新增字段与事件；`CaptureSegment.text` 保留编号 |
| `Worker.Step` 确定性 | 定时到期按 `clock.Clock` 在同步路径判断，UTC 写入事件；展开只取决于日志前缀 |
| 请求前缀稳定 | 定时、重答、确认不改系统指令与工具；擦除与修订是显式例外（§2.4） |
| 内容安全在写日志之前 | 修订、单次调用的输入与输出都检查；定时的输入在创建时已检查 |
| 写入闸门只收紧 | 闸门经展开器取文字，擦除后的片段不再作为依据 |
| 新存储通过一致性套件、登记删除 | `capturetest`、`outboxtest`；`capture_segments`、`outbox`、到期表、`end_user_tiers`、EndUser 删除记录都登记，接入删除不变量与差分测试 |
| 新表登记分片键，一个事务一个键 | `capture_segments`、`outbox`、到期表按 Session；`end_user_tiers`、EndUser 删除记录按 EndUser |
| 外部写入后唤醒用 `Decision.Wake` | 定时的创建置 Wake，使 Worker 重新计算停放时间 |
| 日志与指标只记 ID | 出站流只含 ID 与范围；单次调用只记 ID 与 token 数 |

## 15. 测试、实施与兼容

- **单元与契约**：`session`（擦除与修订的校验、平台专属字段、回执拼前缀、定时的投影与 UTC、`redo_of` 的拒绝情形、流与 `scope: run` 的加载校验）；`capture`（展开以日志前缀为准、擦除先于清空时的占位）；RRULE 子集与时区（含夏令时跳过与重复）；`authz_test.go` 的新接口。
- **一致性套件**：`capturetest`、`outboxtest`，内存与 PostgreSQL 两种实现；`workqueuetest` 覆盖到期表迁移与唤醒、取消、删除的交错。
- **模拟测试**（`internal/sim`）：擦除与 Capture 提交、活跃 Run、删除交错，清空失败后由 Janitor 重试；业务线拒收与 MCP 故障下读取位置不越过未确认的时段；定时在写入前后崩溃不重复、不遗漏，跳过落事件；出站流乱序提交不漏、删除后只剩信封；注销与按 EndUser 的写入交错。覆盖统计：`CapturesErased`、`CapturesCorrected`、`AcksCommitted`、`SchedulesFired`、`SchedulesSkipped`、`OutboxRows`、`EndUserBarrierWithdrawn`。
- **评测**：`capture-*` 与基线一致（展开后模型看到的文本不变）；新增 `schedule-weekly-report`、`redo-different-answer`、`oneshot-live-brief`、`capture-erase-midrun`（整理中擦除，之后的整理与 Persona 中都没有被擦除的内容）。
- **e2e**：擦除、修订、回执、定时、出站流、单次调用各走一次真实 HTTP。
- **阶段**：按[整体架构](./persona-architecture.md) §11。P1 验收另含"擦除跨库竞态"与"出站流消费者离线超过 72 小时后对账"。
- **不保留对旧日志的兼容**（与 [m5](./m5-inputs-triggers-loops.md) §11 相同）：带 `text` 的 `CaptureAppended` 在 P1 之后不再读取，Mac mini 的开发数据库在部署 P1 之前清空（届时先确认）。

## 16. 不在这一版

- **按说话人擦除**（"删除某人说的所有话"）：需要跨批识别同一个人，等 `speaker_ref` 的覆盖率足够再做。
- **跨 Session 的定时**：定时只在本 Session 内开启 Run，跨 Session 仍由编排器。
- **出站流携带内容**：先只含 ID，看消费方的读事件开销是否成问题。
- **单次调用流式输出**：实况预览不需要。
- **Capture 正文移出 PostgreSQL**（对象存储或列式存储）：分区方案的压测不达标时再评估。
