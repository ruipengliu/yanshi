# 旁听

> 状态：已实现（2026-10-25）· 决定：ADR-0036（只收文字，信任按说话人区分）、ADR-0037（每日旁听 Session 与分级保留）、ADR-0039（流、读取位置与来源推导）· 业务线的用法：[旁听业务线 xinli](./captures-xinli.md) · 术语：`CONTEXT.md` 的 Capture、Stream、Exchange、Retention

全天佩戴的录音外设（领夹）经手机上的 HostApp 接入，把 EndUser 一天的对话以文字交给 yanshi。本文是平台这一侧：写入、投影、读取（`captures` 工具）、引用核对、信任、保留与删除。整理成摘要、归集到 Project、承诺账本与每周回顾由业务线编排，见 [captures-xinli.md](./captures-xinli.md)。流、触发与外部循环的通用设计见 [输入、触发与外部循环](./m5-inputs-triggers-loops.md)。

## 1. 目标

- HostApp 能把带说话人标签的旁听转写（Capture）批量写入 Session，可安全重试，不开启 Run。
- 整理旁听的 Agent 能按时段（Span）分页读取，长的一天不必一次放进上下文；白天可以多次整理，每次只读新的。
- 他人的话不被当作用户原话：不绕过 Memory 写入闸门（ADR-0023），受注入判断覆盖（ADR-0030）；业务线核对承诺人时依据平台给出的说话人，而不是模型自报的。
- 他人的原话只保留短期（默认 7 天），关于 EndUser 本人的 Memory 长期保留，均可按 Session 删除。
- 不新增跨 Session 写入：跨 Session 的编排由业务线服务端完成。

## 2. 架构

```
领夹 ──蓝牙──▶ HostApp：转写、声纹区分（yanshi 之外）
                 │ SubmitCaptures（HTTP 或 Connection，经 service），stream = "transcript"
                 ▼
        旁听 Session（AgentDef 声明 streams: [transcript]，每个 EndUser 当地每天一个）
                 │ 业务线提交"整理"输入；Run 用 captures 工具按时段读
                 │ ├─ memory_save：用户本人说过的偏好、人际关系（§7）
                 │ └─ mcp_<业务线>__*：交出整理结果；引用的原话由平台附上说话人（§6）
                 ▼
        业务线服务端（远程 MCP Server）：Project、承诺账本、提醒、每周回顾
```

业务线服务端以远程 MCP Server 的身份提供工具（[MCP 设计](./m5-mcp.md)）：每次调用都带 `X-Yanshi-End-User` 与 `X-Yanshi-Session`，服务端据此按用户处理数据。它不作为 Node 接入：Node 总是代表某个 EndUser（[认证](./auth.md) §3）。

## 3. 写入 Capture

### 契约

`proto/yanshi/v1/event.proto`：

```proto
// CaptureAppended 是 HostApp 提交的一批旁听转写（ADR-0036）。不开启 Run，不进入 Transcript。
message CaptureAppended {
  string capture_id = 1;               // HostApp 生成，按它去重
  repeated CaptureSegment segments = 2;
  string stream = 3;                   // 必填，平台登记的流（session.KnownStream）；旁听转写是 "transcript"
}

message CaptureSegment {
  Speaker speaker = 1;                 // 佩戴者或他人
  string speaker_key = 2;              // 区分不同的他人："other-1"、"other-2"；只在本批内有意义
  string text = 3;                     // 未通过内容安全时为空
  bool redacted = 4;                   // 未通过内容安全，只留占位；只由平台设置
  google.protobuf.Timestamp spoken_at = 5;  // 设备时钟：用于显示时刻与切分时段，不用于排序
  string time_zone = 6;                // 手机所在的 IANA 时区，如 "Asia/Shanghai"；为空时不显示时间、不按停顿切分
}
```

一个 Capture 是一批片段，而不是一句话：HostApp 通常每分钟或断网恢复后提交一批，一次 `Store.Update` 写一个 Event。建议 HostApp 用语音活动检测去掉静音，每 1–5 分钟或每次停顿提交一批（容量估算见 [m5](./m5-inputs-triggers-loops.md) §8）。

### 接口

| 入口 | 形式 |
|---|---|
| HTTP | `POST /v1/sessions/{id}/captures`，`{stream, capture_id, segments[]}`，返回 `{redacted, duplicate}` |
| Connection | `ClientRequest.op` 的 `SubmitCaptures{session_id, stream, capture_id, segments}`（`node.proto`） |
| service | `Service.SubmitCaptures(ctx, sessionID, stream, captureID, segments)`，返回 `CaptureResult{Duplicate, Redacted}` |

两个入口都调用 `service.SubmitCaptures`（ADR-0024），三个 SDK 都有提交方法（ADR-0026）。用户令牌与服务令牌都可提交，授权规则与输入相同（`internal/httpapi/authz_test.go` 的矩阵）。

### 决策

1. 校验：`stream` 未登记、`capture_id` 为空或超过 128 字节（`MaxInputIDLen`）、超过 200 段或单段超过 2000 字、客户端设置了 `redacted`、`time_zone` 不是 IANA 时区名（或是 `Local`）时返回 invalid。写错的时区会让整理显示错误的日期，所以拒绝而不是悄悄不显示。
2. `capture_id` 在最近的窗口中（`State.Captures`，§4）：返回 `duplicate`，不写入。决策之前与决策中各查一次，与输入去重相同。
3. AgentDef 没有声明这个流（`streams`）：返回 invalid。防止 Capture 写进 Project 或助手 Session。
4. 内容安全（下节），然后在 `session.Store.Update` 的决策中：Session 已关闭返回 conflict（已删除照常 not found）；否则写入一个 `CaptureAppended`，不唤醒 Session（Capture 不开启 Run，ADR-0036）。

不做准入与配额检查：它们只约束新 Run，Capture 不调用模型。

### 内容安全（ADR-0021）

在写日志之前检查，送检的单位是整批，与一条输入相同：

- 整批不违规：整批写入。一批至多 200 段，逐段送检的代价是每批上百次调用。
- 整批违规：逐段送检，单独违规的片段 `text` 置空、`redacted = true`，其余照常写入。时间线上可见空缺，一句话不会让整批丢失。没有一段单独违规（违规取决于上下文）时，整批都只留占位。
- 提供商不可用：整批拒绝（fail closed），返回 unavailable。HostApp 保留本地缓冲，稍后以同一 `capture_id` 重试。

### Session 索引

Capture 与输入一样更新索引中的 `LastInputAt`（受 `TouchEvery` 节流），否则只有旁听、没有输入的旁听 Session 会在白天被 `close_after_idle` 关闭。

## 4. 投影

`State.Captures`（`session.CaptureLog`）：

- 最近 512 个 `capture_id` 的窗口（`MaxRecentCaptures`），供去重；
- Capture 的数量、最后一个 Capture 的序号与写入时间（更新索引；业务线可以据数量判断有没有新的旁听）；
- 每个流已读到的位置 `Read`（§5 的接着读）。

Capture 的内容不进入 `State`，也不追加到 `History`：一天的转写可能有十几万字，放进投影会使每个快照都随之增大。`captures` 工具从日志按序号读取。关闭之后的 Capture 被 `Apply` 拒绝；`stream` 为空或未登记的同样被拒绝（不读 ADR-0039 之前写下的日志）。

## 5. `captures` 工具

内置工具 `captures`（`internal/capture`）在 `app.NewCore` 中总是登记，由能力白名单决定哪个 AgentDef 能用；只有声明了 `transcript` 流的 Session 才有内容可读。

### 时段

时段（Span）是一个旁听 Session 中前后相连的一段旁听：相邻两段的说话时间相隔不超过间隔。由平台按日志确定性地切分，是分页与判断"是否已结束"的单位，不承担语义：一个时段里可能有几段不同的谈话（相隔几分钟的两通电话、午饭转谈工作），一场会议也可能因为一次长时间的静默跨两个时段。交谈（Exchange）由整理的模型在时段内划定、交给业务线（ADR-0042，[xinli 的建模](./captures-xinli-model.md) §8）。

- **边界只看时间。** 一批 Capture 可能跨过多个时段（断网后补交的一批可能长达几个小时），所以边界可以落在一批中间。
- **没有时间时每页算一个时段。** 片段缺少 `spoken_at` 或 `time_zone` 时无法按停顿切分，按段数与字数上限分页。HostApp 应当填写时间。
- **起点由位置派生。** 时段以第一段的引用编号称呼，例如 `#57.0`，同一份日志总是切出同样的时段。
- **间隔来自 AgentDef。** `streams` 中的 `exchange_gap`（字段名沿用），默认 5 分钟（`agentdef.DefaultExchangeGap`）。一个 Session 第一次读取时用的间隔随读取位置记下（`CaptureRead.exchange_gap_ms`），之后沿用，分页与"是否已结束"的判断在 Session 内前后一致；AgentDef 改了间隔，从新的 Session 起生效。间隔只影响一页的大小与"是否已结束"，不必按 ADR-0030 在留出集上调。

### 输入与分页

- 输入：`{after?: 位置, all?: bool, limit?: 段数，默认 200、至多 500}`。
- `after` 是位置字符串：`"0"`（或省略）从头读；`"57.3"` 表示第 57 个事件的第 3 段之后；停在一个时段中间时形如 `"57.3~50.0"`，后半是这个时段的起点，下一页据此标"（续）"。模型照抄页尾给出的值，不必理解它。`"new"` 见下节。
- 一页至多一个时段：时段结束时本页就结束，即使还没到上限；单个时段超出 `limit` 或约 6000 字符（`capture.MaxChars`）时分成几页，只有第一段就超出时也读入它。整页同时以调用方 AgentDef 的 `context.max_tool_result` 为预算：接纳下一段前，使用与 runtime 截断相同的 `model.EstimateTokens` 对未展开的内容块估算，计入标题、日期头、各行并预留最长页尾。预算充足时沿用原有页边界与文本；不足时提前分页，不切碎 Capture。连完整一段及页首尾都放不下时返回配置错误，不附 `capture_read`，读取位置不前进。声明 `captures` 能力的 AgentDef 加载时，按默认值补齐后的 `max_tool_result` 必须达到 `session.MinCaptureToolResult()`（最长 2000 字、每字至多 4 字节及页首尾的保守估算上限）。
- 页首形如 `【时段 #57.0 起】`（续页为 `【时段 #57.0 起（续）】`）；页尾注明"时段 #57.0 起到此结束，下一时段用 after=…"、"时段 #57.0 起未完，下一页用 after=…"或"已读完目前的全部旁听"。

### 给模型看的文本

每段是一个旁听片段（`ContentBlock` 的 `capture`，平台从日志读出写入），在 Transcript、估算与压缩摘要中展开为文本（`model.ExpandCaptures`、`model.CaptureText`），提供商不会见到结构；截断按片段进行，不会切开一段话。

- 每行形如 `#57.3 10:15 [佩戴者] 明天我把报价发给张三` 与 `#57.4 10:16 [他人 other-1] 好的，周五之前`。开头是引用编号（事件序号.段序号，在 Session 内唯一，§6 用它）。拦截的片段显示为 `[他人 other-1]（这段话未通过内容安全检查，已拦截）`。
- 片段中的换行替换为空格，一段话不能在文本上冒充另一行；`speaker_key` 只能是 `[A-Za-z0-9_-]{1,32}`。
- 有 `spoken_at` 与 `time_zone` 时行首是当地时刻；本页第一段与当地日期变化处另起一行 `—— 2026-10-23 星期五（当地时间）——`，并列出之后七天的日期与星期（模型自己推算星期常错一天）。模型据此把"明天""下周一"换算成日期。没有时区的片段不显示时间：`spoken_at` 是绝对时间，不知道时区时显示出来反而会误导。时区数据库编进二进制（`time/tzdata`）。

工具说明与输出格式是给模型看的文本，须有评测覆盖（[captures-xinli.md](./captures-xinli.md) §10）。工具定义随 AgentDef 固定，按名称排序，请求前缀不受影响。

### 接着读与进行中的时段

- **读取位置由平台记录**（ADR-0039、ADR-0049）。`captures` 读到旁听的每一页，Worker 在结果中写入读取位置（`ToolResult.capture_read`：流名、下一页的位置、时段间隔、实际开始游标 `start`、本页第一段 `first` 与流的 `ack` 声明），模型看不到；Run 完成（`RunCompleted`）时，未声明确认的流沿用本 Run 最后一页的位置；声明 `ack: true`（`agentdef.Stream.Ack`）的流从已提交游标起，沿本 Run 中被回执完整覆盖的页拼链，同一起点取最远的被覆盖页，只提交连续前缀的真实页尾（包括 `~` 续页位置）与该页的时段间隔。页与回执分别保存在 `session.Run.CapturePages`、`session.Run.CaptureAcks`，回执随 `ToolResult.CaptureAck` 写入；规则见 [内核补充](./m7-kernel-primitives.md) §11。投影提交到 `State.Captures.Read`。没有读到任何旁听的页（"已读完"、"还在进行"）不带位置。`capture_read` 只能出现在 `captures` 的结果中，`Apply` 校验。
- **`after: "new"`** 从投影中已提交的位置接着读，不扫描日志、不解析给模型看的文字，模型也不必记住位置。只认已完成的 Run：Run 读了一个时段、还没交出就失败了，下一次会重新读到它；业务线按交出的来源去重，重复交出时取代之前的结果（[xinli 的建模](./captures-xinli-model.md) §5）。
- **进行中的时段先不读。** 读到日志末尾、而最后一段距现在不到间隔的时段可能还在继续：不返回它，只注明"时段 #… 还在进行，稍后再整理"，读取位置也就不前进。跨页的时段在第一页就先看到它的末尾：否则第一页的位置随 Run 提交，之后接着读从续页开始，第一页的原话再也读不到。这一条对任何 `after` 都生效（模型可能照抄页尾的位置接着读）；`all: true` 时照常返回（深夜的整理、调试控制台读刚写下的旁听）。"现在"取自平台的时钟（`clock.Clock`），结果写入日志后重放时使用记录下来的结果。

白天多次整理的 Agent 同时用 `context.scope: run`（[长 Run](./m2-long-runs.md) §2）：每个 Run 只看本轮的上下文，不背着上午读过的全部旁听；Run 之间靠读取位置与业务线保存的结果衔接。

### 平台专属

旁听片段只能出现在 `captures` 的结果中，且只由 Worker 写入：用户输入、其他调用的结果、设备的外部结果中出现时 service 拒绝、`Apply` 再校验一次（与 `show_card` 的卡片相同，ADR-0032、ADR-0039）。工具名不含 `__`，闸门与注入判断按片段的说话人字段判断（§7），而不是解析文本中的前缀。结果写入 `ToolResult` 后，重放时使用记录下来的结果，与其他调用一样。

## 6. 引用与说话人核对（`cite_captures`）

业务线要核对整理结果（例如承诺是谁做出的），但不能信模型自报的说话人。MCP 登记的 `cite_captures: true`（`mcpcap.Server.CiteCaptures`，[MCP 设计](./m5-mcp.md)）让平台在调用这个 Server 的工具时：

1. **解析引用。** 在调用参数中找出恰为引用编号的字符串（如 `"#57.3"`），按发起调用的 Session 的日志解析（`mcpcap.Citations`，一次至多 200 个）。
2. **附上说话人与说话时间。** 放进 `_meta["yanshi/captures"]`：`[{ref, speaker: "wearer" | "other", speaker_key, redacted, spoken_at, time_zone}]`，无法解析的为 `{ref, missing: true}`。说话时间不是对话内容，业务线据此按说话那天换算期限（[xinli 的建模](./captures-xinli-model.md) §7）；片段没有时间时省略。

- **说话人以平台为准。** 与 Memory 写入闸门的判断依据相同（ADR-0036）。
- **只给说话人与时间，不给文字。** 他人的原话只保留 7 天（ADR-0037），不经这条路径流到业务线长期保存。业务线需要展示原话时，在保留期内用服务令牌从旁听 Session 读取。
- **只解析本 Session 的旁听。** 引用别的 Session 一律为 missing。
- **确定性。** 结果只取决于日志，而日志只追加，重放时得到同样的结果，不另记 Event。

不核对"引用的原话是否真的说了这件事"：这需要比较文字，留作后续（§12）。

## 7. 信任：写入闸门与注入判断

佩戴者的话算用户原话，他人的话算外部内容（ADR-0036）。判断依据是结构化的说话人字段，由平台推导为来源（`session.Provenance`：佩戴者为 `FromUser`，他人为 `FromOther`，ADR-0039）：

- **Memory 写入闸门**（`internal/runtime/memgate.go`）：佩戴者的旁听片段可以作为 `memory_save` 的依据；调用结果中含有未拦截的他人片段时，上下文视为含外部内容。拦截的片段没有内容，不算。效果是：佩戴者说"我不吃香菜"可以直接记住；从他人发言中得出、佩戴者没有说过的内容，写入前需要用户批准。他人在话里写上"[佩戴者]"不起作用。
- **注入判断**：含有他人片段的调用结果是待判断的结果，只把他人的话交给判断（佩戴者的话与平台写的说明不是外部内容），沿用 `decision.ToolResultInjection` 这个判断种类。
- **健康类**：情绪、压力、心理状态只能写入 `health`，业务线未开启 `memory: {health: true}` 时被拒绝（ADR-0022）；该类别的界定写进整理 Agent 的指令，并有评测覆盖。

说话人标签由 HostApp 给出，yanshi 无法验证。标错时他人的话会被当作用户原话，兜底是注入判断与白名单中没有高风险能力。

## 8. Retention 按 AgentDef 覆盖

业务线配置：

```yaml
retention:
  close_after_idle: 720h
  delete_after_close: 0          # Project 与助手 Session 长期保留
  agents:
    xinli-day:
      close_after_idle: 36h      # 整理后由服务端关闭；36h 只是兜底
      delete_after_close: 168h   # 原话保留 7 天
```

- `lifecycle.Retention` 的 `Agents map[string]Retention`。覆盖按 AgentDef 名称（不按版本），索引中的 `agent` 只存名称。覆盖整条替换，不与业务线的值合并；嵌套的覆盖在加载配置时拒绝（`lifecycle.Retention.Validate`）。
- `lifecycle.Index` 的 `IdleBefore` 与 `ClosedBefore` 带 AgentDef 过滤（`lifecycle.AgentFilter`）：Janitor 按 `lifecycle.Retention.Rules` 先按每个覆盖查询该业务线中这个 AgentDef 的 Session，再以业务线的值查询其余 AgentDef（排除有覆盖的名称），一个 Session 恰好落在一条规则中。
- PostgreSQL：迁移 `0016_sessions_agent_idle.sql` 与 `0017_sessions_agent_closed.sql` 分别建 `(business_line, agent, last_input_at) WHERE closed_at IS NULL` 与 `(business_line, agent, closed_at) WHERE closed_at IS NOT NULL`。排除若干 AgentDef 的查询仍走原有的业务线索引。
- `lifecycletest` 覆盖覆盖与排除，内存与 PostgreSQL 两种实现都要通过。

## 9. 删除（ADR-0015、ADR-0037）

| 操作 | 实现 | 一并删除的 Memory |
|---|---|---|
| Retention 到期 | Janitor 删除旁听 Session | **保留** |
| 删除某天 | 删除那天的旁听 Session | 那天写入的 |
| 删除某个项目 | 删除那个 Project | 它写入的 |
| 删除全部旁听数据 | 业务线列出该用户的全部 Session 并逐个删除 | 全部 |
| 注销账号 | `DELETE /v1/end_users/{id}` | 全部 |

到期删除保留 Memory（[Memory 设计](./m4-memory-grant.md) §7、§9）：删除记录带原因（`lifecycle.Tombstone`），Memory 在 `app.SessionData` 中登记为 `KeepOnRetention`，Janitor 在原因为 `lifecycle.ReasonRetention` 时跳过它。委托派生的子 Session 随父 Session 按保留期删除时，原因同样是 `retention`。

Session 已在按保留期删除（对外已不可见）时，用户再删除它得到 not found，不会改为一并删除 Memory；这些 Memory 仍可经 `GET /v1/memories` 列出、删除。

删除不级联到其他 Session：删除某天不影响业务线已保存的摘要（它们不含他人的原话，§6）。

## 10. 计量、配额与日志

- Capture 不调用模型，不计量。整理与提问的 Run 照常计量，计入 EndUser 与业务线的 Quota（ADR-0019）。
- 超出配额时 Run 由 Worker 挂起（现有行为），不为旁听放宽 Quota。
- 日志与指标只记录 Session ID、`capture_id`、片段数与拦截数，不记录转写内容与说话人（ADR-0015）。

## 11. 测试

**单元与契约**
- `session`：`CheckCapture`、未登记的流、`capture_read` 只出现在 `captures` 的结果中、`RunCompleted` 时提交读取位置、失败的 Run 不推进；来源推导（`provenance_test.go`）。
- `capture`：跨批的时段边界、没有时间时的退回、续页、`"new"` 从投影读、进行中的时段被跳过（`all` 时返回）、时段间隔沿用读取位置中的值。
- `memgate_test.go`：佩戴者原话作依据可免审批；他人片段使上下文含外部内容；已拦截的片段不计。
- `service`：去重（窗口内重复、决策中并发重复）、关闭后拒绝、未声明流的 AgentDef 拒绝、内容安全的拦截与不可用。
- `mcpcap`：`cite_captures` 解析本 Session、别的 Session、无效编号。
- `authz_test.go`、`lifecycletest`、Janitor（到期删除保留 Memory，主动删除照旧删除）。
- e2e：HTTP、Connection 与 SDK 的提交（`internal/e2e/capture_test.go`）。

**模拟测试**（`internal/sim`）
- 动作：HostApp 提交 Capture，按概率重投同一 `capture_id`、与输入和删除交错；版本 2 的 AgentDef 用 `scope: run`，同一批 Session 在两个版本之间切换。
- 故障：Capture 提交时的存储故障、内容安全不可用。
- 覆盖统计：`CapturesAppended`、`CaptureDuplicates`、`CapturesRejected`、`RetentionKeptMemories`。
- 删除不变量（`checkDeleted`）按删除原因检查 Memory 是否保留；差分测试 `TestPostgresMatchesMemory` 覆盖新事件。

**评测**：`evals/capture-*.yaml`，见 [captures-xinli.md](./captures-xinli.md) §10。

## 12. 未做

- **实时判断**：每批 Capture 判断是否需要立即提醒。等评测留出集能确定阈值后再做（ADR-0030）。
- **平台触发整理**：AgentDef 在流上声明 `trigger`，静默一段时间后由平台开启 Run（ADR-0040，[m5](./m5-inputs-triggers-loops.md) §6）。实现之前由业务线定时提交。
- **引用文字的核对**：在平台侧计算整理结果与被引用原话的字符重合度（与写入闸门的依据判定相同），再附进 `_meta`。
- **跨 Session 投递的原语**：等第二条业务线需要时再做。
- **非文字的采集**（照片、位置）：每一种另立 ADR。
- **音频、声纹、他人知情同意的提示**：在 yanshi 之外。
