# M7 内核补充：Capture 正文存储、擦除、交付确认、定时、出站流、单次调用

> 状态：设计（2026-10-27），经 Codex 两轮评审后修订（[评审记录](../review/2026-10-27-persona-architecture-review.md)）；交付确认（§11）已删除，EndUser 删除屏障（§12）已实现，定时已全部实现（含 xinli-assistant 版本 2 使用、评测拨钟、pgqueue 内部到期表，压测结果见 §13），单次调用、档位、重答与 `speaker_ref` 按 P3 的讨论改写（[整体架构](./persona-architecture.md) §13 的 Q77–Q90），实现中；`speaker_ref`、档位与单次调用已实现，重答未实现 · **§2–§4、§11 由 [ADR-0050](../adr/0050-capture-text-in-the-business-line.md) 取代**（旁听原文存放在业务线；§11 已删除），§5 按 P2 的讨论改写（没有窗口），§6 的擦除与修订信封不做 · ADR-0044（已取代）、ADR-0045、ADR-0046、ADR-0047、ADR-0049（已取代）、ADR-0050 · 来由：[xinli 与 persona agent 的整体架构](./persona-architecture.md) §2.3 · 依赖：[M0 核心原语](./m0-core-primitives.md)、[旁听](./captures.md)、[输入、触发与外部循环](./m5-inputs-triggers-loops.md)、[Session 生命周期](./m2-session-lifecycle.md)（ADR-0015）、[Memory 与 Grant](./m4-memory-grant.md)、[配额与用量](./m4-quota-usage.md)、[模型调用](./model-calls.md) · 术语：`CONTEXT.md` 的 Erasure、CaptureCorrection、Ack、Schedule、Outbox、Oneshot、Tier、Redo

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
| 8 | 档位（Tier） | 触发与定时的 Run 没有用户令牌，配额只能在内核执行 | P3，已实现 |
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
- **写入**：旧设计的内核采集决策中，事件与片段在同一事务中写入（同一 Session 键、同一分片），经 `lifecycle.Recheck` 先写、后查删除记录。
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

> 实现状态：定时已全部实现：事件、投影、Session 时区、HTTP、到点执行、内置能力、clock_now 默认时区、空闲关闭豁免、内部到期表与百万停放压测（Mac mini 实测结果见 §13），以及 xinli-assistant 版本 2 使用与评测拨到下次到期（[评测](./m4-eval.md) §2；新增用例与版本回归待远端验证）。

> P2 动工前按[整体架构](./persona-architecture.md) §13 的 Q55–Q76 改写：没有窗口、Session 的时区、超额不跳过、有定时的 Session 不按空闲关闭。

### 5.1 为什么在内核

按钟点做的事（用户要求的"每周五给我周报"、"明早提醒我"，之后的心力优先与画像）由业务线到点以服务令牌提交时，输入在投影中是用户原话：Agent 根据外部内容写下的定时文字，到期后会被当作用户亲口说的，绕过写入闸门（ADR-0037 拒绝"推入"的同一理由）。内核的定时让到期输入的来源为"平台定时"，并复用后台调用的停放唤醒。业务线提交的固定指令（"整理今天"、"回顾本周"）没有这个问题，仍由编排器按业务规则提交（ADR-0050，Q55）。

这修改了 ADR-0040 第 5 条：那里担心的"EndUser 的时区与一天的边界"由创建方显式给出，内核不知道"一天"；定时属于一个 Session，只在本 Session 内开启 Run。

### 5.2 声明

```
Schedule {
  schedule_id,                       // 创建方给出（能力由 call_id 派生），按它去重
  input: [ContentBlock],             // 到期时的输入文字；只允许 Text
  time_zone?: "Asia/Shanghai",       // IANA；省略时取 Session 的时区，Session 没有时区则必填
  at?: "2026-10-30T17:00",           // 一次性：当地时间
  rrule?: "FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0",   // 周期：RRULE 子集（FREQ=DAILY|WEEKLY|MONTHLY，INTERVAL=1–52，BYDAY=MO…SU〔MONTHLY 可带 1–4 或 -1 序数〕，BYMONTHDAY=1–31 或 -1〔仅 MONTHLY〕，BYHOUR、BYMINUTE、COUNT、UNTIL）
  until?: 时间戳
}
```

- **Session 的时区**：创建 Session 时可带 `time_zone`（IANA，记入 `SessionCreated`，用户令牌与服务令牌都可设），是定时与 `clock_now` 省略时区时的默认值；P2 不提供修改（Q65、Q75）。
- **时区规则**：当地时刻不存在（夏令时跳过）时顺延到之后第一个存在的时刻；重复时取第一次。每次解析出的 UTC 时间写入事件，重放不依赖升级后的时区数据库。RRULE 子集由内核自己实现，不引入第三方库（Q59）。
- **限额**：每个 Session 至多 50 个有效定时；`rrule` 的最小间隔为 1 小时；`at` 须晚于现在。委托派生的子 Session 不能创建定时（Q70）。

### 5.3 接口

| 入口 | 说明 |
|---|---|
| 内置能力 `schedule` | `{action: create \| list \| cancel, ...}`，由能力白名单授予；create 的结果写明下次执行的当地时间与星期；当前 Run 受外部内容影响时（写入闸门的判断），create 与 cancel 须用户批准（Q58） |
| HTTP | `POST /v1/sessions/{id}/schedules`、`GET`、`DELETE /v1/sessions/{id}/schedules/{schedule_id}`：用户令牌（本人的 Session）与服务令牌（本业务线）；不审批 |

内置能力由 `service.ScheduleCapability` 提供，在 `app.NewCore` 中与 Memory 能力一起追加；仍须 AgentDef 白名单授予，工具目录按名称排序。create 以 call_id 的 SHA-256 派生 sch_ 前缀的 ID，by 为 agent；同一调用重试复用声明。list 按下次到期与 ID 排序，列出文字、规则、带星期的当地到期时间；cancel 前先 list，重复取消不再写事件。一次性与 RRULE 的钟点均为当地时间，RRULE 必须带 BYHOUR 与 BYMINUTE；until 参数为带偏移的时间戳。

审批复用 `runtime.tainted`：当前上下文有不是用户原话的内容（包括平台定时）时，create 与 cancel 都须批准，不采用 Memory 的用户文字依据豁免；list 不审批。能力的 Checker 先解析参数、Compile 规则并检查子 Session 与有效数上限，失败直接返回可重试的错误说明，不请求审批。摘要注明创建或取消、定时文字（至多 200 token），创建摘要显示规则、下次当地时间与时区；批准、拒绝及超时沿用 Worker 的审批流程。执行时 service 再校验当前状态并进行内容安全检查；等待审批期间一次性时刻已过期的，返回错误而不创建。

定时的文字在创建时经内容安全检查，到期时不再检查。三个 SDK 不提供定时方法（App 在 P4 经 BFF，Q63）；控制台已有通用事件列表，显示新事件的种类、时刻与可展开的完整 JSON，不新增面板或场景。

### 5.4 语义

- **事件**：`ScheduleCreated{schedule_id, input, time_zone, at, rrule, until, start, next_due, by}`、`ScheduleCancelled`、`ScheduleFired{schedule_id, due_at, next_due, run_id}`、`ScheduleSkipped{schedule_id, first_due, last_due, count, reason, next_due}`。`next_due` 是解析好的 UTC。投影中的定时（`State.Schedules`）只从事件读取下次到期，递增 `ProjectionVersion`。Apply 只校验 ID、字段形状、UTC 顺序、有效状态与输入配对，不重新解析规则或加载时区；规则校验与到期计算仅在 service 写入决策中进行。
- **执行是原子的**：`ScheduleFired` 与带定时标记（`RunRequested.schedule {schedule_id, due_at}`）的 `RunRequested` 在同一次追加中写入；输入 ID 由 `schedule_id` 与 `due_at` 的 SHA-256 派生，保留 `schedule_` 前缀，普通输入不能占用它，两个 Worker 同时判断也只生效一次。时间取自 `clock.Clock`，判断在 `Worker.Step` 的同步路径中。
- **有活跃 Run 或排队的输入时**：等它们结束再执行，不插话。`State.Active()` 包含排队状态；多个定时按下次到期、再按 ID 排序，每步只处理一个。
- **来源是平台定时**（Q57）：对写入闸门不是用户原话（只凭它写 Memory 须审批），对注入判断不是外部内容；客户端提交的输入不能带定时标记（统一检查点拒绝，`Apply` 再校验）。模型看到的输入前有平台渲染的一行"【定时 <ID> 到点：10月30日星期五 17:00】"（Q67），在 Run 的位置上，不进系统指令。
- **已保存的到期**：Fire 以投影的 NextDue 为第一点，当前规则仅推算它之后的到期；一次性直接消费已保存的 UTC。tzdata 更新只能影响尚未写入日志的将来到期，不能改写已定的时刻。
- **错过**：一次性晚到照常执行；周期只执行最近一次到期，更早的合并为一条 `ScheduleSkipped`（first_due、last_due 两端均包含，count 不含本次执行）。关闭后到点的全部未处理到期合并为 reason=closed，并清空 next_due，后续不再执行。
- **超额不跳过**（Q69）：到点总是写入 `RunRequested`，配额与准入由 Run 自己处理（挂起或排队）；`ScheduleSkipped` 只用于错过的周期与到点时已关闭的 Session。
- **失效**：一次性的执行后失效；`until` 或 `COUNT` 用尽后失效；Session 关闭后失效。有有效定时的 Session 不按空闲关闭（Q68），`delete_at` 与用户删除照常。
- **空闲关闭索引**：`lifecycle.Session.NextDue` 缓存投影中最早到期，零表示没有有效定时。service 在 Created、Cancelled、Fired、Skipped 写日志后调用 `SyncScheduleIndex`，不受 TouchEvery 节流。`IdleBefore` 跳过非零值；Janitor 关闭前读投影并修正索引，有有效定时则跳过，`Service.Close` 的空闲关闭决策也复查，防止并发创建被误关。反向校准用 `Index.ScheduledAfter` 按 Session ID 分页，每业务线每次 Sweep 至多 100 行，游标跨轮推进、到尾回绕，清除取消或耗尽后残留的非零值。日志与索引之间崩溃或并发乱序只造成暂时滞后：漏记的豁免由关闭前检查兜住，多记的豁免由分页校准清除；索引并非到期表，不负责唤醒。
- **停放契约**：所有未来定时均由 `workqueue.Park` 保留工作项，取后台截止时间与定时到期的较小者。Run 挂起时已到期的定时等待 Run 结束，不用过去的时刻反复唤醒；未来到期仍参与上限。创建、取消与结束 Run 时以 `Decision.Wake` 原子唤醒。
- **内部到期表**（Q61，P2-③）：调用方始终调用 `Queue.Park`，Worker 不区分远近。仅 Session 的 PostgreSQL 队列将晚于 now + Horizon（默认 1 小时，装配时可设）且未被唤醒的停放移到 work_dues；按 Session 分片，到期索引独立并发创建。类别、Park 后的顺序与到期时刻一并保存；租约期间的入队仍使归还后立即可认领。内存队列无需搬移。
- **迁移与并发**：Janitor 每次 Sweep 调用可选接口 `workqueue.Deferrer`，将 Horizon 内的候选逐 Session 事务移回工作表，保留精确 parked_until 与 ord；每批 1000 项，每批重新读取当前时间并以 now + Horizon 为上界，直到不足一批或耗尽时间预算。一次 Sweep 的迁移共用 `Janitor.PromoteBudget`，默认 `SweepEvery / 2`（默认周期一分钟，对应预算 30 秒），可在 Janitor 字段中配置；预算在批次边界检查，已开始的一批允许完成，余下工作下轮继续。搬移和独立入队、日志事务内入队、Remove、Park 使用同一 Session 的事务锁；旧候选按 ord 再校验，不能搬走后来的停放。Claim SQL 不变，远期项不参与认领扫描，也不计入可认领积压；队列成员检查包含两张表。指标 yanshi_queue_promoted_total 只记迁移数量。
- **故障**：Horizon 应大于 Sweep 周期的若干倍（默认 1 小时对 1 分钟）；Janitor 停摆超过 Horizon，或到期积压长期超过时间预算内的迁移能力，会晚到但不会提前执行。部署必须运行 Janitor，恢复后按原来的错过规则处理。远期项仍以日志为事实源，到期表只负责活性。
- **删除**：`app.SessionData` 的工作队列登记包含 work_dues，由 Remove 同事务清除两表。搬移只转移已有行，和删除互斥，不产生删除后的新写入；原有外部入队入口仍按生命周期协议复查删除记录。模拟测试的删除不变量检查两表没有残留。
- **重试与有效数**：同 ID、相同规范化声明幂等返回原记录，不重新锚定；不同声明返回 409。失效声明保留以防 ID 复用，限额只数 next_due 非空的记录。HTTP 列表只含有效记录；关闭后待记录 closed 的定时仍由 Worker 停放到点处理。
- **投递**：到点 Run 的回复留在日志中，经现有事件流看到；推送在 P4（Q74）。

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

> 单次调用已实现（P3-⑤，2026-10-28）；按 [整体架构](./persona-architecture.md) §13 的 Q77–Q82，只做 `generate`，输入只收文字。编译、检查与真实模型评测待远端执行。

### 7.1 为什么

实况预览的简述、标题、纠错建议都是"模型函数"：一次输入、一次输出、没有工具与状态。用 Run 做，每次都要建 Session、写十几条事件；业务侧直连提供商，又绕过了计量、配额、内容安全与换模型的评测（章程 G9、原则 10）。

### 7.2 声明

`oneshots/*.yaml`，与 AgentDef 同样版本化、经 `oneshots/releases.yaml` 灰度（ADR-0020）。声明是全局的，任何业务线都可调用，费用记在调用方（与 AgentDef 一致）：

```yaml
name: xinli-live-brief
version: "1"
model: ark/doubao-seed-2-1-lite-260915:minimal
instructions: |
  用两三句话概括这段旁听正在谈什么……
input: {max_chars: 8000}       # 按 Unicode 字符计
output:                        # 可选：JSON Schema，平台校验（不解析外部引用），不合格时修复一次后失败
  type: object
  properties: {brief: {type: string}}
  required: [brief]
  additionalProperties: false
max_output_tokens: 300
concurrency: 8                 # 本声明（各版本共享）的并发上限
work_class: interactive        # interactive | background
```

声明不需要 `kind`；显式写 `kind: generate` 兼容接受，其他值拒绝。`kind: embedding` 推迟到 Persona 的向量检索（Q77）。名字与版本沿用 AgentDef 格式；输入、输出 token 与声明并发上限必须为正；模型引用须为 provider/model。缺省 work_class 为 interactive。同名所有版本的 concurrency 必须一致。Schema 在加载时由 google/jsonschema-go 编译，内部引用可用，禁止外部引用与加载器。

### 7.3 接口与语义

`POST /v1/oneshots/{name}`，`{end_user, input}`。服务令牌须带 `end_user`；用户令牌只能是本人。返回 `{name, version, model, output}`，output 为字符串，声明了 Schema 时为 JSON 值。开发模式沿用创建 Session 的方式在请求体给出 business_line。目录不存在或没有声明时接口返回 404，不装配单次调用服务，也不校验单次调用限额。

- **输入只收文字**（Q78）：ADR-0050 后原文在业务线，业务线自己读出并按说话人格式化。单次调用没有工具、Memory 与日志，不涉及来源推导，其中的说话人标注不会被当作可信来源。
- **不写日志、不存结果**：日志与指标只记 ID、名称、版本与 token 数（ADR-0015）。调用方不得保存他人原话的派生内容超过原文的保留期，原文擦除后要丢弃在途结果（接入文档写明）。不承诺跨 HTTP 重试的幂等：没有结果存储，重试就是再调一次、再计一次费。
- **顺序**：鉴权与声明、输入校验 → EndUser 删除屏障（§12）→ 输入内容安全 → 配额 → 准入 → 模型调用 → 计量 → 输出内容安全 → Schema 校验（不合格修复一次，两次都计量；内容安全拒绝不重试）→ 返回前复查屏障。总时限从声明与输入校验之前开始，默认 60 秒，覆盖审核、排队和两次调用。一次请求只选一次版本；修复仍用同一声明、原始输入及固定错误类别 invalid_json 或 schema_mismatch，不回送失败输出或校验库原文。客户端取消不撤销已消耗 token 的计量，但计量仍受原总时限约束；存储失败或时限已到时沿用 ADR-0019 的尽力计量。
- **计量与配额**：照常计量到业务线与 EndUser（ADR-0019），用量记 `oneshot = name@version`（不冒充 Agent），超额拒绝；价格表的启动校验覆盖所有单次调用的模型。
- **准入**（Q80）：与 Run 的模型调用共用 `model.Guard`。优先级：交互 Run > 交互单次调用 > 后台 Run > 后台单次调用（`work_class: background`）。并发受限时，名额释放先回填后台 Run 的最低在途份额，达到份额后按上述顺序交付；后台单次调用不计入份额。最低份额默认并发上限的 1/8 向上取整，须严格小于上限（并发为 1 时显式设 0）；不抢占、不限制后台上限，空闲时后台可占满。四类准入与按键上限工具位于 `internal/model`，`oneshot.Service` 经 `app.NewCore` 装配并接入。单次调用另有三层并发上限：全部单次调用合计、每业务线、每个声明；进入 Guard 之前按固定顺序有界等待，超时返回 429，持有到逻辑调用结束（含重试退避）。合计上限必须小于提供商的总并发（启动校验）：这部分容量单次调用永远占不到，问答在单次调用饱和时仍有名额。RPS 令牌桶共享，RPS 成为瓶颈时单次调用仍可能挤占问答的发起速率（遗留）。
- **三层配置**：合计用 `app.Options.OneshotMax`（默认 8），业务线用配置 oneshots.max_concurrency（默认 0，只受其他层约束），声明用 concurrency。按合计、业务线、声明名依次获取；声明键不含版本或业务线。`app.Options.OneshotWait` 为每层最长等待（默认 2 秒），失败时逆序释放已获取的名额；成功时持有到函数返回。`app.Options.OneshotDeadline` 为整个请求的时限。
- **错误**：未知声明或删除屏障 404；空输入、缺 EndUser、Unicode 字符超限 400；冒充其他用户 403；内容安全拒绝 422、不可用 503；配额 429（携带 quota 与 Retry-After）；并发等待超时 429；总时限或模型超时 504；两次 Schema 都不合格或模型失败 502。错误不携带输入输出。
- **指标**：`yanshi_oneshot_calls_total{name, version, outcome}` 每个逻辑调用一次；未知名称使用空标签，防止用户任意名称成为指标维度。outcome 仅使用固定结果类别；模型 token 同时计入既有 token 指标。
- **评测**：用例写 oneshot 而非 agent，每轮直接经同一个 Service 调用。`evals/oneshot-live-brief.yaml` 的三个独立输入每个 trial 都运行，三次 trial 检查波动；JSON 按完整 JSON 文本做 reply 与 judge 断言，声明有 Schema 默认检查 output_schema_valid。`-oneshot-version` 指定发布前版本，`-agent-version` 不影响它；改指令与模型须与基线比较。
- **删除交错验证**：`internal/oneshot/service_test.go` 覆盖模型返回、计量写入、输出审核时注销，复用无 Session 的 Meter 屏障复查，最终不返回内容且用量匿名化；没有为单次调用新增 Session 或 sim 步进状态。

## 8. 档位（Tier）

> 档位已实现（Q83）；新增测试待远端验证，契约与实现见[配额与用量](./m4-quota-usage.md) §3–§6。

- 业务线配置：

```yaml
quota:
  monthly: 50000
  end_user_daily: 2        # 未设档位的 EndUser
  tiers:                   # 每档的额度必须显式 > 0
    free: {end_user_daily: 2}
    pro:  {end_user_daily: 10}
```

- 档位只替换 EndUser 日限额，不改变业务线月限额、周期与已用金额。升级不清零、降级不追溯。
- `PUT /v1/end_users/{id}/tier` `{business_line?, tier}` 与 `DELETE`（恢复默认），只允许服务令牌，用户令牌 403；未知档位或业务线未开启配额时 400。业务线取自令牌；开发模式 Put 可用 JSON 或查询参数，Delete 用查询参数。成功返回有效 `tier` 与 `limit_micros`；默认档位为空字符串。`/v1/quota` 的 EndUser 层有有效分配时返回 `tier`。
- 新表 `end_user_tiers(business_line, end_user, tier, updated_at)`，分片键为 EndUser（`pg.Tables` 登记），注销账号时删除；`app.Stores` 构造，`app.NewCore` 装配共享缓存，`Service.DeleteEndUser` 固定清理路径在封口前删除档位。写入"先查、后复查"删除屏障（§12），回包丢失或请求取消也复查，发现屏障撤回写入，沿用屏障错误码 404。
- `Quotas.Status` 统一按档位解析日限额，Submit、模型调用、压缩、Call、语音与单次调用都经它。档位读缓存 5 秒，本进程写后立即失效，按世代防止失效前发起的读取回填旧值；跨进程最多 5 秒生效（软上限）。已挂起的 Run 照旧按现有周期复查配额。
- 已存的档位在配置中被删除时回落到默认 `end_user_daily`（为 0 即不限）并以无标签指标 `yanshi_quota_unknown_tier_total` 计数：删档位配置之前先迁移已分配的用户。
- 不用令牌声明：触发、定时、委托开启的 Run 没有用户令牌，准入时拿不到档位。

## 9. 重答（Redo）

> 按 P3 的讨论改写（Q84–Q87）。

- **接口**：`POST /v1/sessions/{id}/inputs`，`{redo: true, input_id, expected_run_id}`，不带内容；`expected_run_id` 必填，不是本 Session 最后一个 Run 时 conflict（多端旧页面不会重答错对象）。重复的 `input_id` 返回第一次的结果。不加 Connection 操作与 SDK 方法：App 在 P4 经 BFF。
- **可重答**（否则 conflict）：最后一个 Run 是 completed；它的调用全部有只读证据（`ToolCallStarted.read_only`，见下）或是 `show_card`；没有审批请求、`ask_user`、插话、委托、后台结果；它的输入是普通输入（不含 CardAction，不来自定时、Call 或委托）；它的输入之后没有发生上下文压缩（按事件序号，含 `scope: run`）；Session 不是委托子 Session，也不是三方入站的（[Agent 生态](./m7-agent-ecosystem.md) §3.2）。
- **只读证据**：`capability.Spec` 新增只读标记（ReadOnly）；内置只有 `clock_now`、`memory_search`；MCP 只从 `readOnlyHint` 映射，不从风险、幂等或 `no_approval` 推导；Node 与沙箱一律视为未知。平台在执行前把判定写入 `ToolCallStarted.read_only`，跨 Attempt 取与；实际执行前若已记只读而当前声明不再只读，拒绝执行并返回固定错误。
- **写入**：`RunRequested{redo_of: <run_id>}`，输入内容块由平台从被重做的 Run 复制，来源字段不复制（特殊来源已被排除）。`redo_of` 只能由平台写入（统一检查点拒绝客户端提交，`Apply` 再校验目标、输入一致与资格）。接受即替代：新 Run 失败或被中断也不回退。新 Run 按普通权限执行；它若写入，它自己就不可再重答。
- **Transcript**：被替代的 Run 的输入、召回、回复与调用不进入之后的上下文，连续重答只保留最后一次。投影保存被替代的 Run 集合（不依赖 Run 明细的保留数）；Transcript、压缩选段、token 估算（不拿被替代回答的用量校准）与摘要渲染共用同一过滤，先由完整投影确定替代关系再取前缀。日志保留全部，App 可以展示"第 2/3 个回答"。
- **安全判断不过滤**：来源推导、写入闸门与注入判断仍看完整历史；模型没看到旧的工具结果，不代表 Session 没接触过外部内容。
- **卡片**：被替代 Run 中卡片的动作按 `call_id` 失效（新回答可能复用同一个 `card_id`）。service 在去重之后、`Store.Update` 的每次决策中拒绝它们；`Apply` 对 `RunRequested`、`Steered` 与回答中的 CardAction 再校验。
- 召回在新 Run 中照常重新做一次（记为 Event）。

## 10. 说话人引用（`speaker_ref`）

> 按 ADR-0050 与 P3 的讨论改写（Q88）。

`CaptureSegment` 新增 `string speaker_ref = 8`（7 已是 attested 结果中的 `ref`）：业务线比对声纹后在结构化页上填入的不透明 ID（业务线的实体 ID）。

- 内核校验语法 `[A-Za-z0-9_-]{1,64}`，且只有佩戴者以外的片段可以带（标签本身也是给模型看的文字，不校验就能夹带指令）；不校验实体是否存在、身份是否准确。来源推导只看 `speaker`，被认出的他人仍是他人（ADR-0039 不变）。`Apply` 对 attested 片段的校验与非 attested 一致。
- 给模型看的文本：`#57.4 10:16 [他人 other-1 @P-0012]`；没有 `speaker_ref` 时文本逐字不变。`cite_captures` 的 `_meta` 附上它。
- Persona 的原文、分页与字符预算同步。

## 11. 交付确认（已删除）

ADR-0050 的退役条件已满足。内核不再登记回执或在 Run 完成时推进旁听读取位置；业务线在保存整理结果的同一事务中推进处理进度。旧协议消息只为解码日志保留。

## 12. EndUser 删除屏障

注销账号（`DELETE /v1/end_users/{id}`）期间，按 EndUser 写入的地方可能让数据复活：新建 Session、单次调用等不带 Session 的用量（`usage.Meter` 已复查 EndUser 屏障）、档位（`end_user_tiers`）。

- 注销先写 EndUser 删除记录（按业务线隔离，分片键为 EndUser），再清除；所有按 EndUser 的写入入口"先写、后查"这条记录（与 `lifecycle.Recheck` 同一写法）：新建 Session、计量、档位、单次调用。
- 屏障在删除执行期保存，全部清除完成之后再保留宽限期（默认 24 小时，经 `app.Options.EndUserGrace` 配置），由 Janitor 删除到期记录；之后同一 EndUser ID 可以重新使用。失败或尚未完成的注销不开始倒计时，重复注销复用原屏障。业务线须先使用户令牌失效，宽限期用于拦截注销前已在途、注销后才落盘的请求。Session 删除记录仍不保存 EndUser，屏障只在执行期与宽限期内保存它。
- 宽限期结束时，Janitor 在删除每条到期屏障之前，再清一次该 EndUser 的档位；通过 `app.NewCore` 登记的 `Core.EndUserData` / `lifecycle.EndUserHolder` 扩展到其他按 EndUser 保存的数据。`PruneEndUsers` 先逐条复核到期条件，再执行幂等清理回调；清除失败保留该条屏障并在下一轮重试，其他候选继续处理。因此宽限期内 Put 落盘后、复查前进程崩溃留下的档位，在屏障消失前仍会被清除。
- PG 的回调与屏障删除在同一 EndUser 事务中完成：锁住并复核候选，档位删除经 `pg.Exec` 沿用事务，每个 EndUser 一条档位 DELETE，失败回滚。内存实现同样串行化清理与屏障移除，防止并发清理者拿着旧候选误删复用 ID 的数据。事务结束后再次失效共享缓存，阻止提交前的读回填旧档位。
- 在 `internal/sim` 的删除不变量中检查：注销与这些写入任意交错，包含 Put 后跳过复查的进程崩溃。收敛时显式推进到宽限期结束，确认屏障已经移除且没有残留档位。
- 屏障已随 P1 落地；档位在 P3 接入，注销清理不依赖 HTTP 或 sim 调用方补传 remover，档位清理失败不能封口。删除后本进程缓存失效，在途读取按世代重读，宽限期后同一 EndUser ID 从默认档位开始。

## 13. 容量

在[整体架构](./persona-architecture.md) §7 的基础上，内核的验收按峰值而不是全天平均：

- **Capture 写入**：旁听集中在约 8 小时，按每分钟一批，峰值约 1700（10 万 DAU）/ 17000（100 万）批每秒；每批一次事件加至多 200 行片段。整批被内容安全拦下时逐段复核，调用数放大到段数。
- **单分片容量**：`app.Postgres` 现在是单个连接池，ADR-0028 只定了分片键。超过单分片容量之前，分片路由是前置条件（P4 验收），包括故障切换。
- **压测项**：P1 的片段写入与逐 Session 删除（含 WAL 与索引）；P2 的百万停放与到期表迁移；单次调用的子限额下问答的首字延迟。

### 百万停放压测（Q62）

在 Mac mini 的 PostgreSQL 单独执行（不进 make check，测试用独立 schema，结束自动清理）：

```bash
YANSHI_BENCH_PARK=1 YANSHI_TEST_PG='<DSN>' go test ./internal/workqueue/pgqueue -run '^TestMillionPark$' -count=1 -timeout=20m -v
```

测试先以真实 Park 生成远期停放模板，再通过 generate_series 批量插入同形数据（每项独立 ord），省去百万次客户端准备往返。1 万可认领项，预热 100 次后采样 1000 次 Claim + Release(done=false) 的端到端 p50/p99。三个场景分别是空表基线、加百万到期表停放、百万工作表停放对照；停放项先于活跃项排列，避免对照被顺序掩盖。另测 1 万项 Promote，最多 16 路并发，每 Session 独立事务，计时包含候选查询及全部提交。

第五场景在 1 万活跃项、无远期与有百万到期表停放两种情况下，对同一个已存在且未停放的项调用 Enqueue；各预热 100 次，再测 1000 次的端到端 p50/p99，量化包含事务咨询锁与到期表查找的入队路径开销，只报告、不设门槛。该比较反映远期数据规模的影响，不单独分离锁、查询或提交的耗时。

验收：到期表场景 p99 ≤ 基线 2 倍；迁移吞吐 ≥ 5000 项/秒（1 万项 ≤ 2 秒）。保持逐 Session 事务，计时包含提交，不改成跨键事务。

口径修订依据（Q62）：Mac mini 实测 1 万项迁移 1.56 秒，约 6400 项/秒；逐 Session 提交受落盘及连接池大小（约等于 CPU 数）限制。原先每分钟固定 10 批 × 1000 项，将实际调度吞吐压到约 167 项/秒。100 万 DAU 若大量定时集中在 08:00，要在一小时 Horizon 内移回 100 万项，至少需要 1000000 / 3600 ≈ 278 项/秒（约 280），固定批数反而成为瓶颈。因此将单次迁移验收从 1 秒改为吞吐至少 5000 项/秒，同时改用每轮时间预算；默认占半个 Sweep 周期，按该吞吐估算有约 2500 项/秒的持续迁移能力。该估算不是负载下的保证，仍需留出数据库竞争与故障恢复余量。

Mac mini 实测（2026-10-28，返工后复测；前四行的 p50/p99 是 Claim + Release，后两行是 Enqueue）：

| 场景 | p50 | p99 | 迁移总耗时 | 吞吐（项/秒） |
|---|---|---|---|---|
| 1 万活跃基线 | 1.15ms | 4.83ms | — | — |
| 加百万到期表停放 | 1.08ms | 1.74ms | — | — |
| 百万工作表停放对照 | 65.7ms | 71.0ms | — | — |
| 迁移 1 万项 | — | — | 1.59s | 约 6300 |
| Enqueue：1 万活跃、无远期 | 0.79ms | 4.42ms | — | — |
| Enqueue：1 万活跃、百万远期 | 0.94ms | 1.27ms | — | — |

认领与迁移按修订后的口径达标；百万远期停放对入队路径没有可见的影响（基线 p99 的波动来自单机噪声）。没有到期表时，百万停放使认领慢约 50 倍，到期表是必要的。

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

- **单元与契约**：`session`（擦除与修订的校验、平台专属字段、定时的投影与 UTC、`redo_of` 的拒绝情形、流与 `scope: run` 的加载校验）；`capture`（展开以日志前缀为准、擦除先于清空时的占位）；RRULE 子集与时区（含夏令时跳过与重复）；`authz_test.go` 的新接口。
- **一致性套件**：`capturetest`、`outboxtest`，内存与 PostgreSQL 两种实现；`workqueuetest` 覆盖到期表迁移与唤醒、取消、删除的交错。
- **模拟测试**（`internal/sim`）：擦除与 Capture 提交、活跃 Run、删除交错，清空失败后由 Janitor 重试；定时在写入前后崩溃不重复、不遗漏，跳过落事件；出站流乱序提交不漏、删除后只剩信封；注销与按 EndUser 的写入交错。覆盖统计：`CapturesErased`、`CapturesCorrected`、`SchedulesFired`、`SchedulesSkipped`、`OutboxRows`、`EndUserBarrierWithdrawn`。
- **评测**：`capture-*` 与基线一致（展开后模型看到的文本不变）；新增 `schedule-weekly-report`、`redo-different-answer`、`oneshot-live-brief`、`capture-erase-midrun`（整理中擦除，之后的整理与 Persona 中都没有被擦除的内容）。
- **e2e**：擦除、修订、回执、定时、出站流、单次调用各走一次真实 HTTP。
- **阶段**：按[整体架构](./persona-architecture.md) §11。P1 验收另含"擦除跨库竞态"与"出站流消费者离线超过 72 小时后对账"。
- **旧日志兼容**：CaptureAppended、CaptureRead 与 CaptureAck 的协议定义保留，退役事件和位置字段在 Apply 中忽略；投影版本升级后旧快照作废。

## 16. 不在这一版

- **按说话人擦除**（"删除某人说的所有话"）：需要跨批识别同一个人，等 `speaker_ref` 的覆盖率足够再做。
- **跨 Session 的定时**：定时只在本 Session 内开启 Run，跨 Session 仍由编排器。
- **出站流携带内容**：先只含 ID，看消费方的读事件开销是否成问题。
- **单次调用流式输出**：实况预览不需要。
- **Capture 正文移出 PostgreSQL**（对象存储或列式存储）：分区方案的压测不达标时再评估。
