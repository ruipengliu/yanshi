# M7 内核补充：Capture 正文存储、擦除、交付确认、定时、出站流、单次调用

> 状态：设计（2026-10-27），经 Codex 两轮评审后修订（[评审记录](../review/2026-10-27-persona-architecture-review.md)）；交付确认（§11）已删除，EndUser 删除屏障（§12）已实现，定时已全部实现（含 xinli-assistant 版本 2 使用、评测拨钟、pgqueue 内部到期表，压测结果见 §13），P3 已全部实现并验证（单次调用、档位、重答与 `speaker_ref`；[整体架构](./persona-architecture.md) §13 的 Q77–Q90；首字延迟压测见 §13）；P4 的出站流按 Q105–Q106 改写，未实现 · **§2–§4、§11 由 [ADR-0050](../adr/0050-capture-text-in-the-business-line.md) 取代**（旁听原文存放在业务线；§11 已删除），§5 按 P2 的讨论改写（没有窗口），§6 按 P4 的 Q105、Q106、Q117、Q118 改写 · ADR-0044（已取代）、ADR-0045、ADR-0046、ADR-0047、ADR-0049（已取代）、ADR-0050 · 来由：[xinli 与 persona agent 的整体架构](./persona-architecture.md) §2.3 · 依赖：[M0 核心原语](./m0-core-primitives.md)、[旁听](./captures.md)、[输入、触发与外部循环](./m5-inputs-triggers-loops.md)、[Session 生命周期](./m2-session-lifecycle.md)（ADR-0015）、[Memory 与 Grant](./m4-memory-grant.md)、[配额与用量](./m4-quota-usage.md)、[模型调用](./model-calls.md) · 术语：`CONTEXT.md` 的 Erasure、CaptureCorrection、Ack、Schedule、Outbox、Oneshot、Tier、Redo

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
| 6 | 出站流（Outbox） | 无在线客户端时可靠发现运行事件；业务侧无法与内核日志原子提交 | P4b，未实现 |
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

> B3 的原子信封与 B4 的消费、保留协议已实现；B5 已加入稳定分页与记录型恢复器，B6 应用消费者待实现。依据[整体架构](./persona-architecture.md) §13 Q105–Q106、Q117、Q118；范围与验收见该文 §11 的 B3–B6。

### 6.1 用途与信封

出站流让业务线在没有在线客户端时可靠发现回复、提问与审批，维护跨 Session 收件箱、转发渠道消息，并在服务端停机后继续消费。业务侧无法把接收记录与内核日志原子提交，因此由内核提供通用运行事件的发现机制。Persona 是否接受领域事实另由业务线记录，不从 Run 完成推导（Q99）。

HTTP 接口为 GET /v1/outbox，接受分片、after 游标、kinds 与 limit；只允许服务令牌访问本业务线，长轮询最多 25 秒。GET /v1/outbox/shards 返回分片及状态；每个 PostgreSQL 分片独立消费、独立保存游标。响应包含信封列表与下一游标；以下是信封字段形状：

```text
普通信封：{envelope_id, business_line, end_user, session_id, seq, kind, run_id?, call_id?, agent?}
删除信封：{envelope_id, business_line, session_id, kind: session_deleted, reason, deletion_request_id?}
```

- 仅七种：run_completed、run_failed、question、approval_requested、background_settled、session_closed、session_deleted。不含正文或调用参数，内容按 Session 与 seq 读取；删除信封没有 Event seq。
- 信封由平台选择器产生，身份及 Agent 来自合法投影，不跨库查询 EndUser 的 Session 索引。它是 ID 与运行元数据，不是新的内容存储；普通信封仍受 Session 删除约束。
- 至少一次投递，消费方按稳定信封 ID 去重，游标与接收记录同事务保存。后续执行由消费方持久任务恢复。

### 6.2 原子写入与删除

普通信封在 `session.Store.commit` 完成事件校验后统一派生，经存储提交契约与事件一起提交；覆盖 `internal/eventlog/pglog` 的 Append 与 AppendAndEnqueue 两条路径。不能在 Update 返回后补写，不能由 Worker 分支各自拼装。信封按 Session 分片，以事件和种类的唯一约束防重复；乐观并发冲突或回滚均不留下信封。

session_deleted 不是已有 Event。它在 `pglifecycle.Deletions.Mark` 写删除记录的同一事务产生，表示“已逻辑删除”，不表示物理清理完成。只含业务线、Session、原因、可选删除请求 ID 与稳定独立信封 ID，不含 EndUser，不伪造 Event seq；重复 Mark 不重复产生逻辑删除信封。删除完成进度仍查询现有接口。

普通信封在 `app.SessionData` 登记，迟到写入遵守 `lifecycle.Recheck`；删除信封不随 Session 普通数据清除，仅按出站保留规则淘汰。两种实现均接入 `app.Stores` 与 `app.NewCore`，内核模拟不依赖 Persona 或应用模块。

### 6.3 事务水位与游标

自增行号不代表提交顺序：事务 A 先取得 101，事务 B 的 102 先提交，直接越过 102 会漏掉晚提交的 101。

- 每行显式保存写入事务的 xid8（pg_current_xact_id()）及唯一行号 id。读取在同一快照内计算 xmin，只返回 xid8 < xmin 的行，按 (xid8, id) 排序；bigserial 只作同 xid 内排序，不能独自充当提交顺序。
- 小于该快照 xmin 的事务已经提交可见或回滚，尚未结束的事务都在水位及以上，因此晚提交不会被游标越过。语义依据见 [PostgreSQL 快照函数](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-TXID-SNAPSHOT)。
- 游标版本化且不透明，逻辑字段为 {business_line, shard, stream_epoch, filter_version, xid8, id}；校验业务线、分片、流世代与过滤版本。改变过滤条件须重新建立消费位置，不能沿用旧过滤器的进度。换世代或低于保留地板须明确返回错误。
- 空结果也返回安全扫描进度，包括没有本业务线匹配行的情况；分页不能越过尚未返回的匹配行。接口提供各分片当前安全起点，供 §6.5 取得 H。
- 长轮询以 NOTIFY 提示加周期复查实现。阻塞水位的事务结束时可能没有本业务线的新通知，不能只等 NOTIFY。
- 监控水位滞后、最老事务、消费者延迟与保留空间，配置事务超时约束；长事务拖住发布时不能跳过水位。

### 6.4 保留与分片故障

保留至少 72 小时从记录可以安全发布后计算，不用插入时刻加 72 小时：长事务可能让旧记录长期不可读。

持久保存“安全水位及首次观察时间”的检查点；只有被至少 72 小时前的安全水位覆盖的记录才可淘汰。另存明确的保留地板；清理按 Session 分批执行，全部完成后才推进地板。不能用当前最小出站行推断游标是否过期，空流或 Session 删除都可能使该推断错误。普通信封仍可随 Session 删除提前清除；删除信封保留到对应地板允许淘汰为止。

每分片独立水位、检查点、地板、世代与消费进度。健康分片的独立读写、队列认领和出站消费继续；依赖失联分片的全局汇总有界失败并明确返回不可用，不能将缺失用量当零或把部分结果当完整结果。就绪与入口支持部分可服务，不把全部分片健康简单做 AND；健康分片上的 Run 若触及全局配额也遵守此规则（Q118）。

### B4 的 HTTP 与存储契约

`GET /v1/outbox?shard=0&after=<cursor>&kinds=question,run_completed&limit=100`：

- 只允许服务令牌，业务线从令牌取得。显式提供其他 `business_line` 返回 403；开发用 `-auth none` 必须提供该查询参数。用户令牌返回 403，缺失或失效的令牌返回 401。
- 单库分片为 0，省略 `shard` 等同 0；其他分片返回 404。`limit` 默认 100，显式指定时为 1–1000。省略 `kinds` 表示全部七种；逗号分隔或重复参数均可，集合排序、去重后生成过滤版本，显式空种类或未知种类返回 400。
- 响应是 `{"events":[...],"next":"..."}`，空列表始终是 `[]`。信封使用 snake_case；普通信封另有可选的 `agent_version`，删除信封省略 `seq` 与 `end_user`。`after` 为空从当前可保留位置开始，并不承诺完整历史。
- 默认最多等待 25 秒，每 1 秒复查；嵌入方可用 `httpapi.Server.OutboxWait`、`OutboxRecheck` 配置。等待中不持有数据库事务或连接，客户端取消立即结束，令牌在等待中到期返回 401。NOTIFY 按业务线提示，订阅尚未生效、通知丢失、其他事务结束均由周期复查兜底。每次空读更新下一位置，超时返回最近的安全扫描进度。
- `GET /v1/outbox/shards?kinds=...` 不挂起，返回 `{"shards":[{"shard":0,"status":"ready","next":"<H>"}]}`。H 是当前水位边界，绑定同一过滤集合，可直接作为追赶的 `after`；该接口拒绝 `after`。分片不可读时返回 503，不将失败伪装成空流。

游标为 `base64url(JSON).base64url(HMAC-SHA256)`，JSON 含 `v=1`、`business_line`、`shard`、`stream_epoch`、`filter_version`、`xid8`、`id`。调用方必须将它作为不透明字符串保存，不能自行构造。过滤版本是版本前缀 `1:` 与排序去重后的种类集合的 SHA-256；密钥与 epoch 存在分片元数据中，普通进程重启保持不变。内存存储重建生成新世代。

| HTTP 状态 | error | 处理 |
|---|---|---|
| 400 | outbox_invalid_request / outbox_invalid_cursor | 修正参数或损坏的游标 |
| 403 | outbox_cursor_scope_mismatch | 游标不属于令牌业务线或请求分片 |
| 404 | outbox_unknown_shard | 修正分片 |
| 409 | outbox_filter_changed | 使用新过滤集合重新建立消费位置 |
| 410 | outbox_epoch_changed / outbox_cursor_expired | 按 §6.5 重建位置；B5 实现应用恢复 |
| 503 | outbox_unavailable | 保留已有进度，稍后重试 |

PG 每次读取使用一个短的 `REPEATABLE READ READ ONLY` 事务，先在该快照读 `outbox_stream` 及 `pg_snapshot_xmin(pg_current_snapshot())`，随后执行：

```sql
SELECT ... FROM outbox
WHERE business_line = $1
  AND xid < $2::xid8
  AND (xid,id) > ($3::xid8,$4::bigint)
  AND kind = ANY($5::text[])
ORDER BY outbox.xid,outbox.id
LIMIT $6;
```

`$2` 是同一快照的 xmin。显式限定排序列，避免投影中的 `xid::text` 被当作字符串排序。满页位置为末行 `(xid,id)`；不足一页（包括空页）为 `(xmin,0)`。真实行号从 1 起，边界 `(xmin,0)` 不会越过当前仍活跃的事务。

保留元数据由新增迁移 0028–0030 创建，每个物理分片一份，在 `pg.Tables` 登记为 Global：

- `outbox_stream(singleton, stream_epoch, cursor_key, floor xid8, prune_target xid8)`：地板与清理目标均表示 `(xid8,0)`。
- `outbox_checkpoints(safe_xmin xid8 PRIMARY KEY, observed_at timestamptz)`：水位首次观察时间，相同水位 `ON CONFLICT DO NOTHING`；列名避开 PostgreSQL 系统列 `xmin`。
- `outbox(business_line,xid,id)` 与 `outbox(xid,id) INCLUDE(session_id)` 索引均以独立的 `CREATE INDEX CONCURRENTLY` 迁移增加。

Janitor 默认每分钟调用维护，每轮至多删除 1000 行：取得元数据行锁，采样安全水位，**在水位取得后**读取 `clock.Clock` 并持久记录检查点（等待连接或锁的时间不能算成可发布时间）；选取至少 72 小时前观察到的最大水位作为 `prune_target` 并提交。随后扫描有限候选，逐 Session 小事务删除 `xid < prune_target` 的行；删除信封同样适用。目标以下没有尚未结束的写入事务，不会再有迟到插入。确认全部候选清空后，另一个短事务将 `floor` 推到目标、清零目标、移除已覆盖的检查点。

清理中途读者使用 `max(floor,prune_target)` 作为有效下界；因此崩溃或部分批次完成时，旧游标明确返回 410，不会成功读到残缺历史。完成前正式地板不推进；重试清理幂等，多 Janitor 用元数据锁串行选择/完成目标。读者的同一快照保证它要么看到清理前的元数据与旧行，要么看到清理后的有效下界。Session 删除不推进出站地板；表为空也照常保存地板。停机只延长保留。

恢复数据库或重建流、无法保证连续性时，运维须在重新开放入口前调用 `pgoutbox.Store.RotateEpoch`（或在同一事务内更新 `stream_epoch`、`cursor_key`，清零 `floor/prune_target` 并删除检查点）。普通服务启动不自动换世代；PITR 是否发生不能仅从出站表可靠推断。方法与清理共享元数据锁，旧世代的清理者不能继续删除新流。

指标均只含业务线/分片 ID 与数值：`yanshi_outbox_watermark_lag_transactions`（快照 xmax−xmin）、`yanshi_outbox_oldest_transaction_seconds`（本库最老活跃事务年龄）、`yanshi_outbox_consumer_lag_transactions`（本进程最近返回位置距水位的事务数，按业务线；不是业务处理确认）、`yanshi_outbox_retained_rows`、`yanshi_outbox_retained_bytes`。Janitor 默认每 15 秒采样；PG 字节数含索引与待回收空间，内存为信封 JSON 大小。没有消费者确认协议，不宣称能量出应用处理延迟。

长事务（包括其他业务线、只读长快照及准备事务）会拖住数据库级水位；不能绕过它发布。内核事务内不得等待模型、外部 I/O 或长轮询。生产建议为在线角色设置 `statement_timeout=5s`、`idle_in_transaction_session_timeout=5s`，并以应用 context 限制整个事务时长（例如 10s）；PG 16 的语句超时不能限制多语句事务总时长。迁移、备份、报表使用独立角色和超时，监控其水位影响；不使用两阶段提交时保持 `max_prepared_transactions=0`。这些是部署约束建议，本改动不重写全库参数。

### 6.5 游标过期恢复

不新增内核目录或删除记录枚举接口。应用拥有账号清单，恢复复用按 EndUser 列 Session 的现有接口，但须增加按不可变 session_id 的稳定分页模式，memory 与 PG 契约一致；默认产品排序仍按最近输入。必要时在现有索引表增加 (business_line, end_user, session_id) 索引。用可变 LastInputAt 翻页会使未扫描项因 Touch 移到游标前，不能用于完整对账（Q117）。

1. 取得各分片当前安全起点组成的 H。
2. 从应用账号表稳定分页枚举 owner，包括必要的注销中恢复状态。
3. 每个 owner 用稳定 Session ID 顺序列出 Session（包括已关闭的），从各自保存的 seq 补读事件。
4. 列出后读取 404 的项按已不可访问处理；它可能已逻辑删除，也可能是创建失败留下的空索引。
5. 已知 Session 未出现在本轮列表中只能视为待确认，必须以删除信封或逐 ID 读取的 404 确认，不能直接判删。
6. 从 H 追赶出站流并去重；若 H 已过期，重新开始恢复。

新账号、Session 的创建继续由 BFF/编排持久流程记录。恢复只保证选定运行事件与当前可访问状态收敛，不承诺补发所有历史通知，也不是全部历史输入的完整变更流。读取内容得到 404 时取消相关发送，不无限重试；已过时的提醒与审批不再推送。

#### B5 的应用实现与 B6 接入点

`apps/xinli/recovery.Recoverer.Step(ctx, businessLine)` 每步做有界读取，以 `heads → owners → sessions → events → confirm → catchup → follow` 推进；首次启动也先对账。`heads` 使用 B4 的 `/v1/outbox/shards`，整轮固定 H；`catchup` 与 `follow` 按分片轮换、独立保存 after。明确的 `outbox_cursor_expired` 或 `outbox_epoch_changed` 才重回 heads，其他错误保留位置、交调用方退避。H 在扫描期间过期，会在追赶时检测到并重新枚举。

应用 `account.Enumerator.AccountOwners` 按业务线、不可变 EndUser ID 翻页，包含 active、disabled、deleting、deleted。Session 列表使用 `order=session_id`；逐 ID 的投影与事件请求均可确认 404。未列出的已知 Session 由 `Consumer.Known` 按 ID 枚举，合并接收检查点、已有编排任务（含终态）和创建意图中的 Session，不以列表差集判删。HTTP 错误、账号归属不符、日志不连续均不推进进度。

`recovery.Consumer` 是 B6 的事务接入点：`Load`、`Session`、`Known` 读取进度与检查点；`Commit(expectedVersion, progress, batch)` 原子校验版本、去重接收、保存检查点和进度。`Batch` 含信封等价物、可选完整页检查点、或由 404 确认的不可访问 ID；404 不伪造删除信封。当前 `task/memstore.Store` 与 `task/pgstore.Store` 提供记录型实现和 `Recorded` 查询。普通接收记录按 `(session_id,seq)` 去重，真实信封另有唯一 envelope_id；恢复等价物没有 envelope_id，追赶遇到真实信封时合并。接收稀疏信封登记已知 Session，但不推进完整日志 seq。

应用迁移 `0005_outbox_recovery.sql` 增加恢复进度、Session 检查点和记录型接收表；`0006_accounts_stable_order.sql` 独立并发创建账号稳定枚举索引。进度保存轮次、当前 owner/Session、两个扫描位置、确认位置及各分片 H/after。检查点只存 ID、Agent 版本、首次提问派发及未了结后台调用元数据，不存正文；页提交前崩溃重读，提交后回包丢失则从持久位置继续。删除清普通接收记录和元数据，保留无 owner 的不可访问标记，阻止重读复活；账号 Cleanup 同事务清理并以账号终态挡住迟到页。

B5 不启动生产消费循环、不创建收件箱或通知任务。B6 复用上述事务边界，把接收与后续任务原子保存，接入启动/周期调度、退避以及产品侧新增的已知 Session 来源；发送前仍须读取当前可访问状态并过滤过时事项。记录型实现只验证恢复和去重，不代表通知交付。

### 6.6 验收与实时事件流

出站流的一致性套件同时覆盖内存与 PG；PG 另测乱序提交、回滚、同 xid 多行、长事务卡水位、空流、过滤变更、保留地板与明确过期错误。`internal/sim` 检查登记事件与普通信封对应、失败提交无信封、删除收敛后只剩允许保留的删除信封，以及消费者崩溃、重读与按 envelope_id 去重后恰好一次；过期对账由 B5 覆盖。以不使用 Persona 的第二条业务线契约测试验证通用性。

当前线程的实时体验继续由 `feed.Source.Stream` 提供日志续传与实时草稿；P4 产品经 BFF 代理 SSE。出站流不转发 token 增量，也不进入聊天首字路径。

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

> 档位已实现（Q83），契约与实现见[配额与用量](./m4-quota-usage.md) §3–§6。

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

> 重答已实现（Q84、Q86、Q87；2026-10-28）。

- **接口**：`POST /v1/sessions/{id}/inputs`，`{redo: true, input_id, expected_run_id}`，不带内容；`expected_run_id` 必填，不是本 Session 最后一个 Run 时 conflict（多端旧页面不会重答错对象）。重复的 `input_id` 返回第一次的结果。不加 Connection 操作与 SDK 方法：App 在 P4 经 BFF。
- **可重答**（否则 conflict）：最后一个 Run 是 completed；它的调用全部有只读证据（`ToolCallStarted.read_only`，见下）或是 `show_card`；没有审批请求、`ask_user`、插话、委托、后台结果；它的输入是普通输入（不含 CardAction，不来自定时、Call 或委托）；它的输入之后没有发生上下文压缩（按事件序号，含 `scope: run`）；Session 不是委托子 Session，也不是三方入站的（[Agent 生态](./m7-agent-ecosystem.md) §3.2）。
- **只读证据**（已接入资格判断）：`capability.Spec.ReadOnly` 表示不改变业务或平台状态，读取的元数据更新不算写，false 表示写入或未知。内置只有 `clock_now`、`memory_search` 为 true；MCP 只从 `readOnlyHint` 映射，不从风险、幂等或 `no_approval` 推导；Node 与沙箱一律视为未知，`show_card` 也不标只读。Worker 在两处 Started 写入点把当时解析的判定写入 `ToolCallStarted.read_only`，旧日志缺字段为 false。投影与快照保留各 Attempt 的判定；`session.Run.CallsReadOnly` 对每个调用的所有 Started 取与，没有 Started（如参数错误、能力不可用）的调用没有证据，无调用时为 true；该方法不包含 `show_card` 的重答豁免或其他资格条件。实际执行前若本 Attempt 已记只读而当前声明不再只读，拒绝执行、不写新 Started，返回固定错误“能力的声明在调用开始后发生了变化，没有执行；请重新发起调用”；已记 false、当前 true 则照常执行且证据不变。已执行但结果提交失败时，保留真实结果并重试提交。
- **写入**：`RunRequested{redo_of: <run_id>}`，输入内容块由平台从被重做的 Run 复制，来源字段不复制（特殊来源已被排除）。`redo_of` 只能由平台写入（统一检查点拒绝客户端提交，`Apply` 再校验目标、输入一致与资格）。接受即替代：新 Run 失败或被中断也不回退。新 Run 按普通权限执行；它若写入，它自己就不可再重答。
- **Transcript**：被替代的 Run 的输入、召回、回复与调用不进入之后的上下文，连续重答只保留最后一次。投影保存被替代的 Run 集合（不依赖 Run 明细的保留数）；Transcript、压缩选段、token 估算（不拿被替代回答的用量校准）与摘要渲染共用同一过滤，先由完整投影确定替代关系再取前缀。日志保留全部，App 可以展示"第 2/3 个回答"。
- **安全判断不过滤**：来源推导、写入闸门与注入判断仍看完整历史；模型没看到旧的工具结果，不代表 Session 没接触过外部内容。
- **卡片**：被替代 Run 中卡片的动作按 `call_id` 失效（新回答可能复用同一个 `card_id`）。service 在去重之后、`Store.Update` 的每次决策中拒绝它们；`Apply` 对 `RunRequested`、`Steered` 与回答中的 CardAction 再校验。
- 召回在新 Run 中照常重新做一次（记为 Event）。

### 9.1 实现与并发

- `session.State.RedoBlocker` 是纯资格函数，返回不含内容的原因类别。当前平台来源为 `RunRequested.from_call`、`RunRequested.schedule`、后台结果内容块，以及 `SessionCreated.parent` 对应的委托输入；普通界面上下文不因此被禁止，其外部来源仍参与安全判断。三方入站 Session 尚无入口，接入时须按结构化来源补上拒绝（代码有 TODO）。
- `service.Service.Redo` 在每次 `Store.Update` 决策中先按输入 ID 去重，再检查关闭、末 Run 与全部资格；复制原内容块，不重写设备或媒体。输入 ID 沿用普通输入的长度、保留前缀与最近 64 条去重窗口；空 ID 仍表示不去重，客户端应生成稳定 ID。`expected_run_id` 必填。
- 新 Run 共用 `checkSubmitInput` 的审核、准入与配额，`newRunEvents` 的撤回版本切换，以及 `finishSubmit` 的 touch。普通输入保留原检查时机，重答在每次决策中检查。审核实际在 service 中，不在 Apply；重答再次调用输入审核，多一次审核不改变复制的内容。事件与 `Decision.Wake` 同事务提交。
- 两个设备竞争同一目标：同 ID 返回第一次结果；不同 ID 只有一个接受，另一个重算发现目标不是最后 Run 后返回 409。普通输入先提交时重答冲突；重答先提交时普通输入照原规则并入新 Run。定时先开启 Run 时重答冲突；重答先开启 Run 时到点处理等待它结束。关闭先提交时新重答冲突；重试已接受的 ID 仍返回 duplicate。
- `RunRequested.redo_of = 6`；快照新增被替代 Run 与卡片调用集合，以及 Run 的原请求、输入序号、插话事实，投影版本为 24。集合以写时复制维护，不因 Run 明细淘汰消失，也不会污染 `Check` 的原投影。审批历史、提问、委托与后台标记沿用 Call 明细。
- `internal/runtime/redo.go` 的 `filterModelHistory` 共用于 Transcript、前缀截取、压缩选段、估算和文本摘要。先取完整替代集合，再截取摘要前缀；被替代回答的 Usage 不用于校准。原 Run 在尾部，重答相当于从它之前继续，更早的系统指令、工具定义和对话前缀保持稳定；评测的缓存命中（17%）与同等长度的短用例相当。
- 卡片的两条查找路径共用失效检查，并在每次决策中、去重之后核验。Apply 对新 Run 输入、插话、回答的独立动作字段与内容块中的动作一并拒绝。新卡片复用同一 card_id 不影响旧 call_id 的失效。

### 9.2 验证

单元测试覆盖资格、非法事件、连续重答与快照、丢响应与并发重算、卡片三种载体、HTTP 校验与授权、模型视图及安全来源。`TestRedoWithFaults` 在每步原动作完成后，以独立随机流抽样追加重答，不占用普通输入的动作槽；附加动作中的提交、审核、存储故障与 Worker 交错也使用该流。覆盖普通输入、旧卡片、定时、删除、崩溃与接管；Run 计数从已验证日志的 RunRequested 累计，不依赖客户端成功响应，删除后仍保留已见覆盖。要求 `Redos`、`RedoConflicts`、`RedoDuplicates`、`RedoCardRejects` 均有覆盖。脚本模型在每次实际请求（含摘要）检查被替代回复、调用与结果，日志前缀复核资格和卡片失效；快照与全回放继续逐字节比较，部分 PG 差分种子开启重答。

`evals/redo-different-answer.yaml` 用普通助手回答亲子室内活动建议，再以 redo 回合独立评分，不以文字不同为条件。评测当前不记录完整模型请求，旧答排除由 runtime 单元测试和 sim 请求检查覆盖；Mac mini 实测 3/3 通过、评分 4.67（2026-10-29），已写入基线。


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

P4 容量按[整体架构](./persona-architecture.md) §13 Q111、Q123 固定负载实测，不承诺用户规模。下面早期原文批次估算已过时；P2、P3 已完成的独立压测结果保留。

- **原文批次写入（旧内核负载估算已过时）**：早期按 10 万/100 万 DAU 估算 1700/17000 批每秒、一批事件加至多 200 行片段；ADR-0050 后原文写入属于 Persona 的 owner 事务，不再算内核负载。内核保存已读可信页副本，读页时整页审核，被拦下再逐段复核。
- **单分片容量**：`app.Postgres` 现在是单个连接池，ADR-0028 只定了分片键。超过单分片容量之前，分片路由是前置条件（P4 验收），包括故障切换。
- **P4 压测项**：Persona 原文追加、分页、重交、擦除与 WAL；内核可信页副本、逐 Session 删除及出站查询；真实双库分摊。固定一年账本与八天原文数据配方，记录 p50/p99、装载量、SQL 数与峰值内存，不设性能硬阈值；正确性及普通事务不全量装载原文仍是硬断言。

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

### 单次调用子限额下的首字延迟（Q81）

独立运行，不进 `make check`，不需要 PostgreSQL、Docker 或真实模型：

```bash
make build
YANSHI_BENCH_TTFT=1 scripts/bench-ttft.sh
# 可调整机器负载与采样时长：
YANSHI_BENCH_TTFT=1 C=32 O=8 B=4 CLIENTS=64 WORKERS=64 \
  RATE=16 TTFT=50ms LATENCY=1s WARMUP=20s DURATION=60s \
  OUT=bench-results/ttft-local scripts/bench-ttft.sh
```

脚本依次启动全新的内存服务，用同一开放式问答计划（预热 20s、采样 60s、16 次/秒，每个 Run 一轮模型、无工具）运行以下配置。模型总耗时 1s、首字 50ms，平均约 16 条问答在途，小于 C−O=24。每组预先建立所有 Session/SSE，采样时不建连；进程文件描述符软限制尝试升到 8192。使用已有 `serve -oneshots` 指向临时目录，由脚本生成 `bench-brief` 声明，不进入全局声明目录；声明不带输出 Schema，避免假模型纯文本触发修复。

| 配置 | model-concurrency | model-min-background | serve oneshot-concurrency / 声明 concurrency | 单次调用循环客户端 | 特殊参数 |
|---|---:|---:|---:|---:|---|
| baseline | 32 | 4 | 8 / 8 | 0 | 无 |
| mixed | 32 | 4 | 8 / 8 | 64 | 无 |
| control | 32 | 4 | 64 / 64 | 64 | -bench-oneshot-unreserved |

各组均 `model-rps=0`、`oneshot-wait=200ms`、64 个通用 Worker；客户端与单次调用总时限默认 30s。可用 C/O/B/CLIENTS/WORKERS/RATE/TTFT/LATENCY/WARMUP/DURATION/RUN_TIMEOUT/PORT/BIN/OUT 环境变量调整，调整后须仍让问答在 C−O 的容量内且单次调用饱和。参数命令行、AgentDef、单次调用声明、JSON 与比较表保存在 OUT。

control 只放开 serve 启动处“合计上限必须严格小于模型总并发”的校验；需 `YANSHI_BENCH_TTFT=1`、memory 存储且全部加载的单次调用声明使用 `bench/`。仍验证正限额、等待与时限。Service 的三层上限、`WithOneshot` 标记、Guard 优先级均不改；control 的合计与声明上限设为客户端数，不再阻止单次调用占满模型名额。Guard 不抢占，因此问答会等待已经开始的假模型释放槽位。这是检验子限额作用所需的最小对照，不给真实提供商增加绕过路径。

判定：`mixed_p99 − baseline_p99 ≤ max(0.10 × baseline_p99, 20ms)` 且问答成功率不降。基线必须全成功；三组问答数相同、没有成功问答丢首字、客户端到达延误 p99 ≤ 20ms；mixed/control 必须有成功的单次调用，单次调用仅允许成功与 429。control 的 p99 必须比 baseline 高出同一容差，才算“明显退化”；没有退化表示实验未触发瓶颈。脚本打印表格，任一条件失败退出非零。

**结论范围**：只覆盖同进程的并发瓶颈、RPS 不受限的负载；不代表真实模型或共享 RPS 被耗尽时的保证，不验证后台 Run 保底份额或多进程的全局上限。首字从实际提交计时，计划延误另外检验，避免客户端调度落后掩盖首字尾延迟。

结果（2026-10-28，Mac mini，`scripts/bench-ttft.sh` 默认配置：C=32、O=8、B=4、64 个 Worker、假模型首字 50ms / 总耗时 1s、问答 16 次/秒开放式到达、预热 20s、采样 60s，每组 960 条问答）：

| 配置 | 问答首字 p50/p95/p99（ms） | 端到端 p50/p95/p99（ms） | 问答成功率 | 单次调用成功/s | 单次调用 429 比例 |
|---|---|---|---|---|---|
| baseline | 52.41 / 52.91 / 53.19 | 1001.00 / 1002.04 / 1002.77 | 100% | — | — |
| mixed | 51.95 / 52.78 / 53.08 | 1000.99 / 1001.94 / 1002.69 | 100% | 8.00 | 96.98% |
| control | 550.77 / 992.16 / 1043.16 | 1500.41 / 1942.25 / 1991.47 | 100% | 15.98 | 0% |

判定通过：`mixed_p99 − baseline_p99 = −0.10ms`，容差 20ms；问答成功率不降。对照组（单次调用不受合计上限约束）p99 高出 989.98ms，说明实验确实触发了并发瓶颈。单次调用在 mixed 中以合计上限（8 个名额 × 1s）的吞吐运行，其余请求在 200ms 有界等待后得到 429。

## 14. 不变量核对

本节与 §15 中涉及旧 Capture 正文存储、展开器、擦除/修订事件及交付确认的条目仅为历史设计，随 §2–§4、§11 由 ADR-0050 取代；P4 出站流以 §6 为准。

| 不变量 | 本设计中 |
|---|---|
| Event 日志是唯一事实源 | 片段、版本、擦除、回执、定时的到期都由日志决定；存储只保存文字，展开以日志前缀为准（§2.4） |
| 改 `Apply` 或 `State` 递增 `ProjectionVersion` | 擦除范围、版本、回执、定时、`redo_of` 都改投影 |
| 契约在 `proto/`，只做向后兼容的修改 | 新增字段与事件；`CaptureSegment.text` 保留编号 |
| `Worker.Step` 确定性 | 定时到期按 `clock.Clock` 在同步路径判断，UTC 写入事件；展开只取决于日志前缀 |
| 请求前缀稳定 | 定时、重答、确认不改系统指令与工具；擦除与修订是显式例外（§2.4） |
| 内容安全在写日志之前 | 修订、单次调用的输入与输出都检查；定时的输入在创建时已检查 |
| 写入闸门只收紧 | 闸门经展开器取文字，擦除后的片段不再作为依据 |
| 新存储通过一致性套件、登记删除 | 到期表、`end_user_tiers`、EndUser 删除记录按所属生命周期清理；计划中的出站流通过一致性套件，普通信封登记 Session 删除，删除信封仅按保留地板淘汰；接入删除不变量与差分测试 |
| 新表登记分片键，一个事务一个键 | 出站信封、到期表按 Session；`end_user_tiers`、EndUser 删除记录按 EndUser |
| 外部写入后唤醒用 `Decision.Wake` | 定时的创建置 Wake，使 Worker 重新计算停放时间 |
| 日志与指标只记 ID | 出站流只含 ID 与运行元数据；单次调用只记 ID 与 token 数 |

## 15. 测试、实施与兼容

- **单元与契约**：`session`（擦除与修订的校验、平台专属字段、定时的投影与 UTC、`redo_of` 的拒绝情形、流与 `scope: run` 的加载校验）；`capture`（展开以日志前缀为准、擦除先于清空时的占位）；RRULE 子集与时区（含夏令时跳过与重复）；`authz_test.go` 的新接口。
- **一致性套件**：旧 Capture 一致性套件（随 ADR-0050 取消）、计划中的出站流一致性套件，内存与 PostgreSQL 两种实现；`workqueuetest` 覆盖到期表迁移与唤醒、取消、删除的交错。
- **模拟测试**（`internal/sim`）：擦除与 Capture 提交、活跃 Run、删除交错，清空失败后由 Janitor 重试；定时在写入前后崩溃不重复、不遗漏，跳过落事件；出站流乱序提交不漏、删除后只剩信封；注销与按 EndUser 的写入交错。覆盖统计：`CapturesErased`、`CapturesCorrected`、`SchedulesFired`、`SchedulesSkipped`、`OutboxRows`、`EndUserBarrierWithdrawn`。
- **评测**：`capture-*` 与基线一致（展开后模型看到的文本不变）；新增 `schedule-weekly-report`、`redo-different-answer`、`oneshot-live-brief`、`capture-erase-midrun`（整理中擦除，之后的整理与 Persona 中都没有被擦除的内容）。
- **e2e**：擦除、修订、回执、定时、出站流、单次调用各走一次真实 HTTP。
- **阶段**：按[整体架构](./persona-architecture.md) §11。P1 验收覆盖擦除跨库竞态；出站流消费者离线超过 72 小时后的对账在 P4b 验收（Q106、Q122）。
- **旧日志兼容**：CaptureAppended、CaptureRead 与 CaptureAck 的协议定义保留，退役事件和位置字段在 Apply 中忽略；投影版本升级后旧快照作废。

## 16. 不在这一版

- **按说话人擦除**（"删除某人说的所有话"）：需要跨批识别同一个人，等 `speaker_ref` 的覆盖率足够再做。
- **跨 Session 的定时**：定时只在本 Session 内开启 Run，跨 Session 仍由编排器。
- **出站流携带内容**：先只含 ID，看消费方的读事件开销是否成问题。
- **单次调用流式输出**：实况预览不需要。
- **Capture 正文移出 PostgreSQL**（对象存储或列式存储）：分区方案的压测不达标时再评估。
