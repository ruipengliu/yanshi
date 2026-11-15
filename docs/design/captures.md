# 旁听：attested 页与渲染器

> 状态：已实现。按 [ADR-0050](../adr/0050-capture-text-in-the-business-line.md)，旁听原文在业务线，内核 Capture 提交、流、读取位置与交付确认已退役。本文只描述内核仍负责的可信页、渲染、审核、来源推导与引用。
>
> 2026-11-14：ADR-0036、ADR-0039、ADR-0050 与 ADR-0037 的内核部分已整合进 [ADR-0052](../adr/0052-attested-results-and-provenance.md)，ADR-0040 已取消，ADR-0010 已被 [ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 取代。本文引用这些 ADR 之处，现行规则以 ADR-0052、ADR-0053 为准（[ADR 索引](../adr/README.md)）。

## 1. 边界

业务线保存原文、切分时段、分页并判断是否进行中，在保存整理结果的同一事务中推进处理进度。内核经同业务线可信登记的 MCP Server 读取结构化页，审核后由平台渲染；服务返回的自由文字不能冒充用户原话。完整线上契约见 §12。

## 2. 架构

HostApp 把转写交给业务线。业务线为工作 Session 提交整理输入，Run 调用业务线的 MCP 工具取得可信页。平台只在 ToolResult 中保存经审核的原文副本，副本随工作 Session 删除；业务线负责原文保留、修订与擦除。

## 3. 内核提交（已退役）

HTTP、Connection 与三个 SDK 均不再提供旁听提交。原文提交接口属于业务线。

## 4. 内核投影（已退役）

内核不保存旁听去重窗口、流或读取位置。旧的 CaptureAppended 事件、读取位置与交付确认消息已于 2026-11-15 从契约中删除（编号与名称保留，ADR-0052）；旁听片段只能出现在 attested 结果中，旧内置 captures 工具的结果不再被 Apply 接受。退役之前的日志不能再回放，开发库与评测数据须重建。

## 5. 渲染规则

`internal/capture` 保留 `PageOptions`、`OptionsFor`、`StructuredPage`、`ParsePage`、`Render` 与纯渲染辅助函数；不再提供内置工具或从日志分页。

- 页标题为 `【时段 #57.0 起】`，续页为 `【时段 #57.0 起（续）】`。时段边界由业务线提供，不承担语义。
- 每段渲染为引用、可选当地时刻、说话人及文字，如 `#57.3 10:15 [佩戴者] 明天发报价`。他人为 `[他人 other-1]`，有说话人引用时为 `[他人 other-1 @P-0012]`（`@` 前一个空格）；没有引用时输出逐字不变。拦截段保留引用等结构元数据、显示固定占位；片段换行替换为空格，防止冒充另一行。
- 同时有说话时间与时区时，页内首段与当地日期变化处输出日期、星期及之后七天的对照。缺少时间或时区时不显示时刻。
- 页尾区分时段结束、仍有下一时段、时段续页及已读完。`Cursor` 输出末段引用；续页附 `~时段起点`，如 `57.3~50.0`。引用属于业务线，`0.0` 合法，零值游标 `0` 表示从头。
- 空页与进行中页不带片段；具体文字与字段见 §12。整页按未展开内容块估算 token，审核后的占位同样受预算限制。
- 平台默认间隔为五分钟，不再接受 AgentDef 的流或间隔配置。业务线分页与平台渲染的固定基准由 `internal/eval/testdata/capture_pages.golden.json` 验证。

## 6. 引用与说话人核对（`cite_captures`）

同业务线 MCP 登记 `cite_captures: true` 时，平台提取参数中恰为引用编号的字符串（如 `#57.3`），每次至多 200 个，去重后通过 `mcpcap.AttestedCitations` 在本 Session 已读过的可信页中解析，不把编号解释为日志位置。

`_meta["yanshi/captures"]` 保持为 `[{ref, speaker: "wearer" | "other", speaker_key, speaker_ref, redacted, spoken_at, time_zone}]`；找不到时为 `{ref, missing: true}`。`speaker_ref` 为空时省略，已有 JSON 形状不变。只传说话人与时间，不传文字；业务线据此核对说话人，不能相信模型自报。引用只在本 Session 中可见。

## 7. 信任：写入闸门与注入判断

佩戴者的话算用户原话，他人的话算外部内容（ADR-0036）。判断依据是结构化的说话人字段，由平台推导为来源（`session.Provenance`：佩戴者为 `FromUser`，他人为 `FromOther`，ADR-0039）：

- **Memory 写入闸门**（`internal/runtime/memgate.go`）：佩戴者的旁听片段可以作为 `memory_save` 的依据；调用结果中含有未拦截的他人片段时，上下文视为含外部内容。拦截的片段没有内容，不算。效果是：佩戴者说"我不吃香菜"可以直接记住；从他人发言中得出、佩戴者没有说过的内容，写入前需要用户批准。他人在话里写上"[佩戴者]"不起作用。
- **注入判断**：含有他人片段的调用结果是待判断的结果，只把他人的话交给判断（佩戴者的话与平台写的说明不是外部内容），沿用 `decision.ToolResultInjection` 这个判断种类。
- **健康类**：情绪、压力、心理状态只能写入 `health`，业务线未开启 `memory: {health: true}` 时被拒绝（ADR-0022）；该类别的界定写进整理 Agent 的指令，并有评测覆盖。

说话人标签由同业务线可信登记的服务给出，yanshi 无法验证。标错时他人的话会被当作用户原话：注入判断不检查它（判断只看他人与外部来源，且目前只以影子模式记录），它还可以为 `memory_save` 提供用户原话依据。不依赖标签的兜底是白名单中没有高风险能力、高风险能力须审批（ADR-0050 对 ADR-0036 的补记）。

## 8. Retention 按 AgentDef 覆盖

业务线配置：

```yaml
retention:
  close_after_idle: 720h
  delete_after_close: 0          # Project 与助手 Session 长期保留
  agents:
    xinli-day:
      close_after_idle: 36h      # 整理后由服务端关闭；36h 只是兜底
      delete_after_close: 168h   # 工作 Session 副本保留 7 天
```

- `lifecycle.Retention` 的 `Agents map[string]Retention`。覆盖按 AgentDef 名称（不按版本），索引中的 `agent` 只存名称。覆盖整条替换，不与业务线的值合并；嵌套的覆盖在加载配置时拒绝（`lifecycle.Retention.Validate`）。
- `lifecycle.Index` 的 `IdleBefore` 与 `ClosedBefore` 带 AgentDef 过滤（`lifecycle.AgentFilter`）：Janitor 按 `lifecycle.Retention.Rules` 先按每个覆盖查询该业务线中这个 AgentDef 的 Session，再以业务线的值查询其余 AgentDef（排除有覆盖的名称），一个 Session 恰好落在一条规则中。
- PostgreSQL：迁移 `0016_sessions_agent_idle.sql` 与 `0017_sessions_agent_closed.sql` 分别建 `(business_line, agent, last_input_at) WHERE closed_at IS NULL` 与 `(business_line, agent, closed_at) WHERE closed_at IS NOT NULL`。排除若干 AgentDef 的查询仍走原有的业务线索引。
- `lifecycletest` 覆盖覆盖与排除，内存与 PostgreSQL 两种实现都要通过。

## 9. 删除（ADR-0015、ADR-0050）

原文由业务线按保留期删除，擦除时在业务库删除原文并按依据级联派生对象，再删除当天的工作 Session。内核中已读页、回复与调用参数的副本随 Session 删除。擦除与绝对删除期限使用 erasure、retention 原因，保留既有 Memory；清理完成后由用户决定是否删除依据可能落在擦除范围内的 Memory。流程见 [xinli 原文擦除](./xinli-captures.md) §13。

按 AgentDef 覆盖的 Retention 仍作工作 Session 的兜底；它不能代替业务线原文保留策略。注销账号照常删除全部所属数据。

## 10. 验证

可信页的严格解析、预算、审核失败、来源推导、Memory 闸门、注入判断与引用解析由单元测试覆盖；模拟保留 attested 结果、拦截与错误覆盖。Persona 的分页、说明和 schema 与冻结 golden 比较，不再运行内核分页生成器。

## 11. 内核采集测试（已退役）

原文提交、读取位置与交付确认的测试随入口删除；可信页验证见 §10。

## 12. 可信登记的旁听页

MCP Server 登记 `attests_speakers: true`（默认 false），且登记的 `business_line` 与 Session 相同，平台才采信结果 `_meta["yanshi/page"]`。未登记或跨业务线时忽略页元数据，按普通外部 MCP 结果处理。有可信页时先丢弃业务线的全部 `content` 与 `structuredContent`，不上传其中的媒体、不回显错误文字；模型只看平台渲染的结果。MCP 自报错误时也只写固定的平台错误。

Persona 是第一个实现者：引用命名空间与处理进度归业务线，按业务日保存。xinli-day 版本 2 使用可信 MCP 页，内核 Capture 路径已删除。见 [xinli 的旁听原文](./xinli-captures.md#13-persona-中的旁听原文)。

开启登记后，请求 `_meta["yanshi/page_options"]` 为：

```json
{"limit":500,"max_chars":6000,"gap_ms":300000,"max_tool_result":0}
```

段数与字符上限来自 `capture.MaxLimit`、`capture.MaxChars`。这里的 limit 是平台接受的上限（500），实际页段数取模型参数（未给时 `capture.DefaultLimit`，即 200）、500 与平台上限的最小值；间隔固定为 `capture.DefaultExchangeGap`（5 分钟），不再提供 AgentDef 的间隔配置；结果预算仍取 `context.max_tool_result`，0 表示不限。Persona 在业务日首次读取时固定该间隔。

响应页契约（`capture.StructuredPage`）：

```json
{
  "kind":"page", "span":"57.0", "cont":false, "done":false, "ended":true,
  "segments":[
    {"ref":"57.0", "speaker":"wearer", "speaker_key":"", "text":"原话",
     "spoken_at":"2026-10-27T10:00:00+08:00", "time_zone":"Asia/Shanghai"}
  ],
  "last_at":"", "time_zone":""
}
```

- `kind` 只能为 `page`、`empty`、`in_progress`。未知字段、类型错误、尾随 JSON 一律拒绝；业务线不能填 `redacted`、页头或下一页游标。
- `page` 至少一段，至多 `limit` 段。每段文字非空且不超过 `session.MaxCaptureText`（2000）个 Unicode rune，超出以“片段文字”拒收。页的字符计数沿用冻结的计数口径：每段 n 是 `#ref ` 加展开后的片段文字（含当地时刻、说话人标注与换行）的 rune 数；第一段不按 max_chars 拒收，之后各段在加入前检查之前累计的字符数加 n，超限即拒收，新日期头在检查之后才计入累计值。max_chars 是页大小约束，单段长度有独立上限，真正的结果预算由 Render 之后的 max_tool_result 检查兜底。
- `ref` 与 `span` 是两个非负整数组成的 `N.M`；解析后按两个整数比较，片段严格递增，`span` 不大于首段，`cont` 为真时严格小于首段。平台渲染为规范十进制形状。引用只属于业务线的命名空间，不解释为日志序号；`0.0` 也是合法引用。
- `speaker` 只能为 `wearer` 或 `other`；佩戴者的键为空，他人的键匹配 `[A-Za-z0-9_-]{1,32}`。非空 `spoken_at` 必须为 RFC3339，非空 `time_zone` 必须为有效 IANA 时区（拒绝 `Local`）。缺少任一个时不显示该段时间。
- `speaker_ref` 是业务线比对声纹后填入的可选不透明实体 ID；非空必须匹配 `[A-Za-z0-9_-]{1,64}`，且只能由 `other` 片段携带，否则拒收整页。内核不校验实体是否存在，来源推导仍只看 `speaker`。页的字符与 token 预算均包含标注中新增的空格、`@` 和引用。
- `span` 是时段起点；`cont` 表示续页；`done` 表示已读完全部旁听；`ended` 表示本时段已结束但后面还有时段。页尾与下一页游标均由平台按 `pageTail` 生成。标题、日期头、逐段编号、正文与占位也由同一纯渲染器生成。
- `empty` 与 `in_progress` 不得带片段。空页输出“（已读完：之后没有更多旁听）”。进行中页必须有有效 `span` 与 `last_at`，输出“（时段 #span 还在进行：最后一段在 HH:MM，稍后再整理。之前的时段已读完）”。因为没有末段对象，可选页级 `time_zone` 指定末段时区；省略时 `last_at` 的 RFC3339 偏移即当地时间，业务线不得用 UTC 时间代替当地偏移后期待平台猜测时区。
- 渲染结果（含占位）必须满足 `max_tool_result`，超限使用平台固定预算错误；任何格式错误只输出平台固定原因类别，不包含业务线提供的内容。

**写日志前内容安全。** Worker 在执行 MCP 调用的同步路径内，先校验整页，再以 Input 阶段将全部片段文字换行相连送检。整页被拦截时逐段复核，只把违规段的文字清空并置 `redacted`（保留说话人、`speaker_ref` 与时间等结构元数据）；若逐段全部通过，说明违规依赖组合，整页改为占位。提供商故障时整页写为错误“旁听页的内容安全检查暂不可用，稍后再读”。审核请求带 Input、BusinessLine、SessionID 与换行拼接的 Text，不写 `ContentModerated`。不同调用重读重审，不缓存审核结论；本次已执行结果进入 Worker 的 `executed` 缓存，追加重试不再次送检。

**来源与日志校验。** 成功结果由 Worker 写 `ToolResult.attested=true`，capture 块带业务线 `ref`；模型看不到标记。Apply 拒绝非 MCP、外部 attempt、路由到 Node 或错误结果的 attested 标记；capture 块只允许出现在平台写入的 attested 结果中。attested 与旧内置结果共用片段校验：说话人、键、说话人引用、文字长度与 redacted 占位关系都须合法。没有 `speaker_ref` 的旧日志仍可回放。旧日志中的读取位置与交付确认字段被忽略。投影版本递增，旧快照作废。attested 页头是平台文字，不产生 FromExternal；佩戴者片段为 FromUser，他人原话合并成一个 FromOther Piece，Result 指向这次调用结果；拦截片段不产生依据。注入判断与 Memory 写入闸门继续按结构化说话人字段判断。

**引用解析。** 开启 `cite_captures` 的同业务线 Server 使用 `AttestedCitations`，扫描调用投影序号以内的日志，在 attested 片段中查找同编号最近一次出现。返回说话人、键、可选说话人引用、时间与是否拦截，查不到为 missing，不回退到旧 Capture 事件。投影为空时序号为 0，表示不限。扫描日志而非 History，避免压缩裁剪造成引用丢失。线上元数据见 §6。
