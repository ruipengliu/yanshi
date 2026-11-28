# 旁听业务线 xinli

> 状态：原有功能已实现；P4-A5 的固定期限、原文版本与自然到期已实现，其他标注 P4 的生产补充按后续步骤接入，以[整体架构](./persona-architecture.md) §13 Q91–Q124 为准。原有功能已用真实模型验证（2026-10-25，评测结果见 [m4-eval.md](./m4-eval.md) §12）；2026-10-25 按 [xinli 的建模](./xinli-model.md)（ADR-0042）改为变更集 · 依赖：[旁听](./captures.md)（ADR-0036、ADR-0037、ADR-0039）、[Memory 与 Grant](./m4-memory-grant.md)、[MCP 适配](./m5-mcp.md)、[长 Run](./m2-long-runs.md)（`context.scope: run`）· 术语：[内核术语](../../CONTEXT.md) 的 Span；[Persona 术语](../../persona/CONTEXT.md) 的 Exchange、Changeset、Revision、Project、Commitment、Awareness
>
> 2026-11-14：ADR-0036、ADR-0039、ADR-0050 与 ADR-0037 的内核部分已整合进 [ADR-0052](../adr/0052-attested-results-and-provenance.md)，ADR-0040 已取消，ADR-0010 已被 [ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 取代。本文引用这些 ADR 之处，现行规则以 ADR-0052、ADR-0053 为准（[ADR 索引](../adr/README.md)）。

xinli 是第一条使用旁听的业务线：把 EndUser 一天的对话整理成交谈的摘要，按主题归集到 Project，记下双方的承诺、随新说出的话修订它们并按期限提醒，每周回顾沟通习惯，供用户随时提问。编排在业务线服务端（ADR-0037）；Persona 保存与分页读取旁听原文，yanshi 提供四个 AgentDef 的运行、可信页的渲染与审核，以及引用的说话人与说话时间（[captures.md](./captures.md) §6）。实体、变更集与修订的模型（为什么这样建、取舍）见 [xinli 的建模](./xinli-model.md)；本文写流程、工具与评测。

## 1. AgentDef 与工具

| AgentDef | Session | 能力白名单 |
|---|---|---|
| `xinli-day`（版本 2、`context.scope: run`） | 每个 EndUser 当地每天一个 | `mcp_xinli__captures`、`memory_save`、`memory_search`、`mcp_xinli__find_related`、`file_changes`、`day_ledger`、`file_day` |
| `xinli-project` | 每个项目一个，长期 | `mcp_xinli__day_summary`、`project_digest`、`set_status`、`update_commitments`、`show_card` |
| `xinli-week` | 每个 EndUser 每周一个 | `mcp_xinli__similar_projects`、`propose_merge`、`week_overview`、`file_week` |
| `xinli-assistant`（版本 2，召回 8 条 Memory） | 每个 EndUser 一个，长期 | `mcp_xinli__list_projects`、`project_digest`、`recent_days`、`memory_save`、`memory_search`、`memory_forget`、`ask_user`、`show_card`、`schedule`、`clock_now` |

- 定义在 `apps/xinli/agents/xinli-*.yaml`。白名单都不含高风险 Capability：`xinli-day` 只能读旁听、写 Memory、召回候选项目、把结果交给业务线；项目与每周回顾只读写业务线的数据；助手还可按用户要求创建、列出和取消本 Session 的定时（[M7](./m7-kernel-primitives.md) §5）。评测中的助手 Session 默认上海时区（当前应用尚无创建助手 Session 的路径），到点收到平台定时输入后用现有 Persona 查询回答，回复留在日志与事件流，推送在 P4；当天整理与每周回顾仍由业务线编排器提交。
- 业务线的工具由 xinli MCP Server 提供（§4），开发与评测中由 `persona/mcpserver` 提供，`apps/xinli` 装配。只读的工具（`captures`、`find_related`、`day_ledger`、`day_summary`、`project_digest`、`list_projects`、`recent_days`、`week_overview`、`similar_projects`）在登记中声明只读；交出结果的工具（`file_changes`、`file_day`、`set_status`、`update_commitments`、`propose_merge`、`file_week`）由业务线在登记中以 `no_approval` 放宽（ADR-0017）：它们只把结果交给业务线自己。登记开启 `attests_speakers` 与 `cite_captures`，承诺人与期限据此核对（§6）。
- 业务线返回的内容（`find_related`、`day_ledger`、`day_summary`、`project_digest`、`week_overview` 等）是调用结果，属于外部内容（ADR-0037）。项目现状由 `xinli-project` 依据它们写成，是业务线的数据，不写入 Memory。
- **Memory 要能读回来。** `xinli-day` 写下的偏好与人际关系，由助手在每个 Run 开始时召回（`memory.recall`）。`xinli-day` 不召回（输入总是"整理交谈"或"整理今天"，按它检索没有意义），写入之前用 `memory_search` 查已有的记忆：记过的不再记，变了的用 `replaces` 替换。平台另外合并几乎相同的表述（[Memory 设计](./m4-memory-grant.md) §3）。
- **Retention。** `xinli-day` 与 `xinli-week` 按 AgentDef 覆盖为 `close_after_idle: 36h`、`delete_after_close: 168h`，作为 Session 兜底；Project 与助手 Session 长期保留。P4 原文期限按[整体架构](./persona-architecture.md) §13 Q95、Q96：首次接收固定业务日时区，次日当地 00:00 加七个当地日历日，保存 UTC，工作 Session 用同一 `delete_at`。补传、改时区、重建不延长；首次上传已到期的历史日也拒收。自然到期立即拒绝原文访问、补传与旧工作 Session 整理写入，后台清原文及读页状态，保留长期账本、历史绑定与不可复用标记，不复用主动删一天。

## 2. 每日流程

1. **开启。** HostApp 把旁听转写交给 Persona，按 EndUser 的当地日期保存到业务日。业务线创建 `xinli-day` 工作 Session 后，以 `bind_day` 绑定业务日与 Session；整理前必须完成绑定。跨午夜新建业务日与工作 Session，已绑定的 Session 不按后续片段的日期重新拆分。
2. **白天。** 原有手动与评测入口在用户空闲时提交"整理交谈"。A10 自动入口按下述规则提交"整理今天"，走完整当天整理与 `file_day` 分支；两种输入均沿用现有 AgentDef，不改变手动和评测语义。Run 用 `mcp_xinli__captures{after: "new"}` 一个个读已经结束的时段，每个时段划出交谈、交出一个变更集（§3），读到"已读完"或"还在进行"为止。有新原话为据的改动（改期、作废、完成）就在这里修订，提醒随之更正。静默与是否有待整理内容由业务线依据原文与处理进度判断，不再声明内核的流上触发（ADR-0050）。
3. **当天整理。** 业务线按各用户的空闲用服务令牌提交"整理今天"：当地 18:00 之后旁听静默满 2 小时提交，到次日当地 04:00 仍未满足的，在 04:00 起的 30 分钟内按 owner/date 稳定抖动提交（[建模](./xinli-model.md) §12），负载随各人的作息分散，不集中在午夜。Run 用 `mcp_xinli__captures{after: "new", all: true}` 读完余下的时段并交出，再用 `day_ledger` 取回当天的账本、交出当天整理（合并、改正、补上漏掉的修订），最后以 `file_day` 交出当天总览与觉察。业务线只接受“整理今天”命令的 `file_day`。P4 按[整体架构](./persona-architecture.md) §13 Q102、Q113 由编排器独占内部工作 Session，提交前登记类型、确认终态前不切换，工具事务内校验当前绑定；无法排除迟到写入时先退役并换绑旧 Session。
4. **归集。** `file_day` 之后，业务线为当天涉及的项目创建（或复用）`xinli-project` Session，提交"整理 <日期>"，输入中不含摘要。Project 的 Run 用 `day_summary{date}` 取回那天归入本项目的交谈与未完成的承诺，以 `set_status` 更新现状、以 `update_commitments` 依据交谈对账（§6）。
5. **关闭。** 业务线调用 `POST /v1/sessions/{id}/close`，`close_after_idle` 兜底。
6. **提醒。** 承诺随变更集一交出就进入承诺账本（§6），业务线按当前状态推送通过核对的承诺：改期后按新期限，作废或合并后停止；白天答应的事当天就能提醒；不经 yanshi 的 Notification。

P4 自动调度以固定业务日时区、原文与处理进度为依据，抖动持久化或由 owner/date 重算；进度未前进退避，以接受事实与进度判断业务完成，不用 Run 终态代替（[整体架构](./persona-architecture.md) §13 Q116）。

### A10 自动调度默认值

- `apps/xinli/schedule` 给出纯时刻规则，编排器启动即扫描，以后每 **30 秒**扫描。时钟与扫描 tick 均可注入；任务重试的下次检查时间使用持久 `RetryAt`。正常未来触发时刻由固定元数据重算，不依赖进程内计时器历史。
- **白天**：最后一批正式接收后静默 **5 分钟**且有未完成整理，提交“整理今天”。本次 A10 明确要求使用该指令，因此也会接受日总览并触发归集；旧“整理交谈”入口继续只处理交谈。此前文档没有白天静默数值，5 分钟取自 [xinli-model](./xinli-model.md) §8 的时段间隔默认值；`capture-workday` 等手动评测仍用原来的输入。跨午夜不再为旧日期产生白天任务，由旧日期的日终任务接管。
- **日终**：沿用 §2 的当地 **18:00 后静默 2 小时**，最迟次日 **04:00 加 [0,30 分钟)** 抖动；夏令时按当地日历计算。最后批次时间是服务端正式接收时刻，不信任补传片段的历史说话时间；同批重交不刷新静默。补传不改变业务日时区或原文期限。旧数据没有接收元数据时首次扫描补齐尾引用，从发现时刻保守等待静默；已到期的旧数据不再读原文补齐。
- **周回顾**：把原有“周日深夜”具体化为 **周日 23:00 加 [0,30 分钟)**，提交“回顾本周”。起点是该周一，窗口为 `[起点, 起点+7天)`；自动任务按起点去重。时区取该周首个业务日已固定的时区，空周沿用之前最近业务日的时区。首次采集前不补造周回顾；停机恢复补最近已到点的一周，已经落库的更早任务继续恢复。手动 `Week` 保持最近七天的起点语义。
- **稳定抖动**：对 JSON 数组 `[business_line,end_user,date]` 做 SHA-256，前八字节按大端无符号整数取模 1800，单位为秒。日终使用业务日期，周回顾使用起点日期；重启与多实例不重新随机取样。
- **完成与重做**：自动日任务必须有本命令的 `file_day` 接受事实且进度覆盖当前原文尾；自动周任务必须有本命令的 `file_week` 接受事实（空结果也算）。安全 Run 终态只用于复用工作 Session，不能代替这些条件；业务已完成但运行仍在途或结果未知时，先退役 Session 再记成功、释放槽位，不再重做业务。无业务完成时先按 A9 退役旧工作 Session，再按 **5、10、20、40 分钟**基数退避，沿用 A8 的稳定 **75%–100%**抖动（退避函数上限 **1 小时**）。同一阶段/原文版本最多 **5 次**业务尝试，异常退役同样计入；`Retirements` 保存已重做次数，`Attempt` 保存认领次数，`RetryAt` 保存下次检查时间。最终 `failed/no_progress`、到期 `failed/expired_unprocessed` 均可用应用任务 `Get` 查询、用 `FailureCount` 计数。新原文版本、日终阶段各有自己的有界任务；不无限重试同一份原文。
- **恢复与边界**：计划接收使用 A8 来源唯一键，提交使用其 input_id、日志对账与单在途限制；创建、换绑、异常重做使用 A9 CAS 与退役。擦除/删日/注销使旧任务失效；到期重查不依赖原文清理是否已经运行，不延长 `expires_at/delete_at`。领域变化先持久接收整组任务（包括空组）再 ack，Notifier 仅提示尽快扫描。
- **装配**：`xinli dev` 有 `-kernel-url` 时默认启动，`-auto-schedule=false` 关闭新计划产生，已有任务仍恢复。`internal/eval/xinlifixture` 启动时没有 `-kernel-url`，因此评测不装配编排器，也不会多出自动整理；生产装配留给 A11。发现接口当前枚举 owner 键再逐 owner 读取元数据，规模成本在 A14 验证。


超出配额时 Run 由 Worker 挂起，业务线次日重试；旁听 Session 删除前仍未整理的，提醒用户"这几天的内容没能整理"。

## 3. 按时段整理（`xinli-day`）

收到"整理交谈"或"整理今天"后，Run：

1. **读一个时段。** 用 `mcp_xinli__captures` 读 Persona 的结构化页，由平台渲染（[captures.md](./captures.md) §12）。时段是 Persona 按停顿切出的一段旁听，只承担分页；一个时段分成几页时读完再整理。
2. **划出交谈。** 在时段里按内容划出交谈：换了对方、换了话题、挂了一通电话接下一通，都是新的一段；电话里只录到佩戴者一侧，靠内容判断。寒暄闲聊不划。每段用 `find_related{query, counterparts}` 召回候选项目与相关的未完成承诺（不限哪天，带 ID 与 `rev`）。
3. **交出变更集。** 一个时段用一次 `file_changes` 交出：新的交谈与承诺；这个时段里的原话改动了已有承诺的，`revise` 或 `close` 那一条，而不是新建。读到佩戴者说出的稳定信息时用 `memory_save` 写入，写之前用 `memory_search` 查重。再读下一个时段。
4. **当天整理**（只在"整理今天"）。读完后用 `day_ledger` 取回今天的交谈与承诺、与今天相关的之前未完成的承诺，以及业务线按规则给出的提示（同一对方、内容相近的承诺对，相邻且归属相同的交谈）；用一次 `file_changes{source: {day: true}}` 合并重复的承诺与被停顿切开的交谈、改正归属、补上白天漏掉的修订。拿不准的不动作：错误的合并修不回来，重复可以事后收拾。
5. **汇总**（只在"整理今天"）。调用一次 `file_day` 交出当天总览与觉察。觉察只写沟通方式；交谈的标题与摘要、决定、风险、觉察、项目名、当天总览仍不写健康信息（包括他人的）。承诺是例外：涉及健康的事可以记下并照常提醒，只写谁、做什么、何时、对谁，不写病情、症状、诊断、用药细节与推断，修订原因也遵守此边界。例如写“周四下午陪妈妈去医院复查”，不写“陪妈妈复查高血压”；对应交谈的标题与摘要可写“约好周四下午陪母亲外出”。项目现状与周回顾同属摘要，仍不写健康信息；助手被问到时可以如实复述承诺内容。依据：[本周评审](../review/2026-10-27-xinli-kernel-next.md) §3.3。用户本人的健康信息只能以 `health` 类别写入 Memory（ADR-0022，[captures.md](./captures.md) §7），Memory 规则不变。
6. **回复**一两句话。

为什么按时段交出：一天的转写可能有十几万字，超过上下文窗口。交出之后的时段就不再依赖上下文，压缩掉的只是已经交出的原文；当天整理依据业务线返回的账本，而不是压缩摘要（评测 `capture-long-day`）。

评测还提供「一个完整时段一个 Run、日终另开 Run」的可选实验（[m4-eval.md](./m4-eval.md) §14），用同一批金标比较成本与质量；实现与结果见 [按时段拆 Run 评审](../review/2026-10-26-xinli-span-runs.md)。

- **重试是安全的。** 变更集按来源去重：键是（工作 Session，`source.from`），当天整理是（工作 Session，day）。同一个键再交一次，业务线撤销上一次的全部效果、再应用新的；`file_day` 按工作 Session 取代；范围变更集接受时在同一事务中推进处理进度。
- **白天只看本轮。** `context.scope: run`：每次整理从头开始，模型只看到新的时段；没有新时段时，一次整理只有一次 `mcp_xinli__captures` 调用与一次回复。
- **调用次数。** 每个时段一两次读取、每段交谈一次 `find_related`、一次 `file_changes`；当天整理再加 `day_ledger`、`file_changes`、`file_day`。`max_turns` 为 240。

## 4. 业务线的工具

**`xinli-day` 使用**

| 工具 | 参数 | 说明 |
|---|---|---|
| `captures`（只读） | `{after?, all?, limit?}` | 按当前工作 Session 绑定的业务日读取结构化旁听页；`after: "new"` 从业务线处理进度继续。 |
| `find_related`（只读） | `{query, counterparts?}` | 按主题召回至多 5 个候选项目（ID、名称、现状、未完成的承诺数，§5），以及按对方与主题召回的至多 10 条未完成承诺（最后一次修订在 30 天内，带 ID、`rev`、引用与修订）。 |
| `file_changes` | 见下 | 交出一个变更集，整体检查（§6）；结果逐项写明记下了什么、每条新承诺是否通过核对，不通过时写明原因、什么都不记下。 |
| `day_ledger`（只读） | `{}` | 今天的交谈（ID、范围、标题、摘要、归属、决定、风险、沟通方式的观察）、今天的承诺与之前未完成而与今天相关的承诺（带 `rev` 与修订），以及确定性的提示。 |
| `file_day` | `{day{headline, highlights}, awareness{habits, suggestions}}` | 当天总览与觉察；只接受"整理今天"那次 Run 的。 |

`file_changes` 的参数（[建模](./xinli-model.md) §5）：

```
{
  source: {from: "#57.0", to: "#80.2"},      // 本次读到的范围；当天整理为 {day: true}
  exchanges: {
    create: [{from, to, title, summary,       // 范围在 source 之内、互不重叠；ID 为 "x" + from
              projects: [{match, id?, name?}],                  // 可以几个，四档归属见 §5
              decisions: [{text, project?}], risks, awareness}],
    merge:  [{into, from: [id], title?, summary?, projects?}],  // 只在当天整理中
    revise: [{id, title?, summary?, projects?}]                 // 只在当天整理中：改正归属
  },
  commitments: {
    create: [{exchange: "x57.0", kind: "mine" | "theirs", content, counterpart,
              due_date?, due_text?, project?, refs: ["#57.3"]}],   // project 缺省随交谈；交谈关联几个项目时必填
    revise: [{id, rev, due_date?, due_text?, content?, counterpart?, project?, refs, reason?}],
    close:  [{id, rev, status: "done" | "cancelled", refs, reason?}],
    merge:  [{into, from: [id], reason?}]                         // 只在当天整理中
  }
}
```

模拟服务对几种常见的写法偏差按确定的规则接受，而不是拒绝或悄悄丢掉：只有新建时把 `{create: [...]}` 写成 `[...]`；把 `commitments` 嵌在 `exchanges` 之内（提到外层，否则承诺丢了、结果却说没有承诺，模型会反复重交）；写了范围又写 `day: true`（按范围交出）。修订的字段与现在相同时不算修订，结果注明"与现在相同，没有改动"。重交时找不到的交谈或承诺若在被取代的那个变更集里，拒绝的原因写明重交是整体取代。

**`xinli-project` 使用**

| 工具 | 参数 | 说明 |
|---|---|---|
| `day_summary`（只读） | `{date}` | 那天归入本项目的交谈（带交谈 ID），以及本项目当前未完成的承诺（带 ID 与 `rev`）。 |
| `project_digest`（只读） | `{project?}` | 项目的现状、按天的时间线与未完成的承诺；在项目自己的 Session 中省略即本项目。 |
| `set_status` | `{status, next_steps, open_questions}` | 项目现状，整体替换。`find_related` 与 `list_projects` 展示的就是它。 |
| `update_commitments` | `{date, revise: [{id, rev, due_date?, …, exchanges}], close: [{id, rev, status, exchanges}]}` | 依据交谈对账，作为一个变更集（§6）。新增承诺只来自 `xinli-day`。 |

`xinli-week` 的工具见 §7。`xinli-assistant` 用 `list_projects`、`project_digest`、`recent_days` 查询项目现状与交谈，用只读的 `list_commitments`、`commitment_history` 查询承诺账本与修订历史。

## 5. 项目归集

- **先召回候选。** 项目多了以后，全量列表放不下，也让模型难以比较。`find_related{query}` 由业务线按主题检索：P4 生产与内存实现共用字符二元组相似度，在已装载账本上运行；倒排、中文分词与向量检索后置（[整体架构](./persona-architecture.md) §13 Q94）。范围是该 EndUser 未归档的项目（§7），按相似度排序，至多 5 个。
- **四档归属。**
  - `existing`：明确是候选中的某个项目，说的是同一件事（同一个交付、同一次招聘、同一套房子）。同一家公司、同一批人谈的另一件事是另一个项目（评审 F4：首页改版因"同一批人"被归进了康宁）。
  - `new`：值得持续跟进的新主题（有定下的事、有承诺或明确的后续），且没有合适的候选。
  - `unsure`：候选中有一个可能相关但拿不准。填上候选的 `id`，再给一个新项目名 `name`。
  - `none`：值得记下、但不需要持续跟进的一次性事务。只出现在当天总览里；有承诺的照样进账本。
- **一段交谈可以关联几个项目。** 老板一次谈了 OKR、客户与团队三件事：`projects` 列出每个项目，每条决定与承诺用 `project` 写明属于哪个，项目的时间线只列属于它（或没有标明）的决定。
- **不确定时交给用户。** 业务线把 `unsure` 放进"待确认"，App 里问用户："这段交谈属于「新办公室装修」吗？"确认前它不进入任何项目的时间线，承诺照样进账本、照样提醒。确认后由业务线归入或新建（记为用户的变更集），不需要再调用 yanshi。
- **碎片由当天整理与每周回顾收拾**：当天整理改正当天的归属；每周回顾合并重复的项目、归档不再活跃的项目、整体重写现状（§7）。项目合并后，交谈与承诺按 ID 引用项目，叠加时换成保留的那个。

## 6. 承诺账本

承诺由业务线保存，以变更集为日志：每条承诺有 ID、出处交谈、引用、核对结果与修订历史，当前状态（期限、内容、对方、状态）由修订依次叠加得出（[建模](./xinli-model.md) §4）。改期、改内容、换对方是 `revise`，作废与完成是 `close`，重复是当天整理的 `merge`；都不删除旧值，修订带说话时间与记录时间，已经按旧期限发出的提醒仍有据可查。

**核对承诺人。** 承诺以结构化字段交出、每条引用原话，平台附上被引用片段的说话人与说话时间（[captures.md](./captures.md) §6），业务线据此核对，不依据模型自报的 `kind`：

- `mine` 至少引用一段佩戴者的话，`theirs` 至少引用一段他人的话，否则标为"未核实"：照常展示，但不按期限提醒。
- 结果告诉模型每条是否通过核对（"均已核实"，或"未核实：引用的原话里没有用户本人说的话"），模型可以改正 `kind` 或 `refs` 后用同一个 `source` 重交。

**整体检查。** 变更集有一项不通过就整体拒绝，结果写明原因让模型改正后重交：

| 检查 | 规则 |
|---|---|
| 范围 | 交谈的 `[from, to]` 在 `source` 之内、互不重叠，也不与已交出的交谈重叠；引用都存在 |
| 作用域 | 白天的变更集可以 `revise`、`close`，引用必须落在本次的 `source` 之内；合并与改正交谈只在当天整理中 |
| 版本 | `revise`、`close` 的 `rev` 与账本当前的修订数一致；不一致时结果附上现在的状态，模型重新判断（与 `session.Store.Update` 的乐观并发同理）。重交同一个来源时，`rev` 对着去掉旧变更集的账本检查 |
| 依据 | `xinli-day` 的 `revise`、`close` 至少引用一段原话；`xinli-project` 的引用交谈；合并须同为未完成、同 `kind` |
| 日期 | 按被引用原话的说话那天把 `due_text` 换算成日期（"下周二"从下一个星期一算起），与 `due_date` 不一致时拒绝并给出换算结果；`due_date` 为空而能换算出唯一日期时补上 |
| 健康 | 交谈、决定、风险、觉察、项目名中出现健康信息用词时拒绝（F3 的兜底；指令仍须写明） |
| 用户优先 | 用户在 App 中改过的字段（状态、期限、内容、对方），模型的改动忽略并在结果中说明 |

**归集的对账**（`xinli-project` 的 `update_commitments`）是一个作者为 `xinli-project` 的变更集，键是（项目 Session，日期），重交时整体取代。它看不到原话，依据是交谈 ID，修订记为"依据摘要"，与"依据原话"区分展示。它在当天整理之后运行，看到的已是白天修订之后的状态；受同样的 `rev` 检查，所以不会用过时的视图覆盖依据原话的修订。它的作用是兜底：称呼对不上、`find_related` 没有召回的承诺，在项目层面按摘要还能对上。

不在核对规则里的：

- **引用的原话是否真的说了这件事。** 例如模型引用了佩戴者一句无关的"好"；又如佩戴者让对方去办的事（"你先问问供应商有没有现货"）被记成佩戴者的承诺：被引用的确实是佩戴者的话，核对通过（评测中观察到，指令写明之后仍偶有发生）。需要比较文字；原文现已在 Persona，但本步不增加核对规则，见 [captures.md](./captures.md) §12。
- **注入。** 他人诱导出来的"承诺"最多变成给佩戴者本人的一条提醒，不触发任何能力。注入判断照常覆盖 `mcp_xinli__captures` 的 attested 结果。

## 7. 每周回顾

每晚的整理只看一天，跨天才能看出的问题留给每周一次的回顾。业务线服务端每周日深夜做三件事，都由它编排，yanshi 不新增调度：

| 步骤 | 谁做 | 结果 |
|---|---|---|
| 归档 | 业务线，按规则，不调用模型 | 30 天没有新交谈、也没有未完成承诺的项目移到"归档" |
| 回顾 | `xinli-week` 的 Run | 提议合并相似的项目（交给用户确认）；交出本周觉察 |
| 重写现状 | 本周有新交谈的每个项目的 `xinli-project` Run | 依据完整时间线整体重写现状 |

### 归档

- 判断只依据数据，确定性、可复现。归档的项目不在 `find_related` 与 `list_projects` 中出现，App 中可以查看、取消归档。
- 每晚整理归属 `new` 时，业务线发现名称与某个归档项目相同，提示用户是否恢复它，不自动恢复。

### 回顾（`xinli-week`）

每个 EndUser 每周一个 Session，业务线提交"回顾本周"。它只读业务线的数据，不读旁听原文、不写 Memory。

| 工具 | 说明 |
|---|---|
| `week_overview`（只读） | 本周每天的总览、每段交谈的标题与归属、每天的觉察。 |
| `similar_projects`（只读） | 按相似度（P4 共用字符二元组，Q94）找出的至多 5 对可能重复的项目，附各自的现状与最近几段交谈的标题。 |
| `propose_merge` | `{into, from, reason}`：提议把 `from` 并入 `into`，进业务线的"待确认"。 |
| `file_week` | `{habits: [{habit, days, example}], suggestions}`：本周的觉察。`days` 是这个习惯出现的日期，至少两天。 |

1. 用 `similar_projects` 取回候选对，逐对判断是否指同一件事（同一个对象、同一批人、同一个目标）；是的用 `propose_merge` 提议。只是主题相近的（两次不同的装修）不提议。
2. 用 `week_overview` 取回本周的觉察，归纳出至少在两天出现过的沟通习惯，各附一个例子，再给出建议，用 `file_week` 交出。只出现一天的不算习惯（一天里的偶然表现不能当成习惯）。
3. 用一两句话回复。

**合并由用户确认。** 与拿不准归属时的做法一致（§5）。确认后业务线把 `from` 标为已并入 `into`：交谈与承诺按 ID 引用项目，之后都算作 `into` 的；关闭 `from` 的 `xinli-project` Session，以后的归集都交给 `into`。合并不调用 yanshi。

### 重写现状

日常的"整理 <日期>"在前一天的现状上增量修改，容易累积错误。P4 在 `file_week` 接受事实持久化后，业务线才向本周有新交谈的每个项目提交“重写现状”（[整体架构](./persona-architecture.md) §13 Q99、Q115；空结果也触发，同命令去重，合法重交产生新事实版本）：`xinli-project` 用 `project_digest` 取回完整的时间线与未完成的承诺，不参考之前的现状、整体重写，用 `set_status` 交出，不对账承诺。

### 接受事实与领域变化（P4-A7）

`file_day` 与 `file_week` 在 owner 的同一事务中保存正文、接受事实元数据及领域变化。接受事实由 `Book.AcceptedFact(kind, date)` 读取；既有工具回执与 `State` JSON 保持原形状。绑定路径的事实 ID 在 owner 内为 `day/<业务日>` 或 `week/<周起点>`，完整身份是（BusinessLine、EndUser、ID）。未绑定旧路径使用 `<种类>/session/<Session 的无填充 base64url 编码>`，按 Session 独立保存事实与版本，不与同展示日期的业务日或其他旧 Session 合并；可用 `Book.AcceptedFactByID` 按变化 ID 读取。周起点沿用 `BindWeek` 的最近七天起点，不改成日历周。未绑定的开发/评测 Session 首次按调用时钟定日/周，此后重试沿用首次接受的日期。

版本从 1 单调递增，与 `FiledAt` 无关。当前接受与同工作 Session 的规范化命令内容相同则不改正文、接受时间或版本；JSON 键顺序、空集合与省略字段不影响去重。内容改变（包括改回先前内容）、换绑 Session、擦除后重建产生新版本；空周回顾及习惯全部过滤的命令照常接受。周命令在过滤前计算指纹，因此不同的提交内容即使都被过滤为空，也各自产生接受版本。指纹只保存在接受事实的内部元数据，不进入领域变化。版本溢出拒绝写入。

**协议边界**：当前 MCP `_meta` 只有 owner 与 Session，没有稳定命令 ID，因而只能比较当前接受的 Session 与内容；不能区分“同内容的独立命令”和重试，也不能识别夹在不同内容重交之后的旧命令迟到重试。生产命令资格与迟到隔离仍由 A9（Q102、Q113）接入，不在 A7 改内核协议。

领域变化只含 owner、事实种类、事实 ID、版本、业务日/周起点、接受时间及确认时间，不含正文、指纹、项目列表或 Agent 调用策略。`Store.PendingDomainChanges` 按 owner 返回未确认变化，重复读取不会确认；`AckDomainChanges` 幂等确认指定事实截至某版本，拒绝超前确认；`PendingChangeOwners` 只发现有未确认变化且未注销的 owner。PG 使用 `domain_changes_pending` 部分索引，内存实现保持同样的筛选与字节排序。消费者以（owner、事实 ID、版本）去重，先持久接收再确认。

确认后的变化保留 7×24 小时；重复确认不刷新期限。`Book.PruneDomainChanges(now)` 可在 owner `Update` 中清理，接受与确认时也顺带执行；闲置 owner 的物理清理可以延后。未确认变化不按年龄清理；确认历史清理后仍保留版本与当前去重信息。擦除或删一天会沿实际业务日绑定删除日总览、清掉其去重指纹和 Session，不影响同展示日期的未绑定旧 Session；保留版本计数及无正文的变化；自然到期保留长期事实和变化。消费时必须重读当前接受事实，不能把已擦除的旧版本视为当前内容。注销逐批清掉两类记录，夹具 reset 按 owner 清空它们。

迁移 `0005_domain_changes.sql` 新增 `accepted_facts` 与 `domain_changes`，接入 Persona 的静态存储与注销目录。升级前的账本正文不自动伪造成新接受；再次接受时开始版本 1。应用的持久任务与消费待 A8/A10：本步保留 `DayFiled` 归集和 `Week` 的既有立即重写行为并加 TODO，不确认变化；尚不能声称端到端后续工作已具备恢复保证。A10 将周重写移到 `file_week` 接受之后。

## 8. 提问（`xinli-assistant`）

用户在 App、微信或飞书中的提问都由业务线提交到该用户的 `xinli-assistant` Session（微信、飞书由业务线服务端以服务令牌代为提交）。助手经 `mcp_xinli__*` 查询，并用召回的 Memory 回答关于用户本人的问题；业务线回答"近几天"类的查询时，从 Persona 读取保留期内的原文。到业务日固定期限后的问题只能依据摘要回答，产品须写明“原话保留至业务日结束后七个当地日历日”（Q95），不按提问或上传时刻滚动延长。

助手问到承诺时用 `list_commitments`：可按 `kind`（mine/theirs）、`status`（open/done/cancelled/all，默认 open）、期限范围 `due_from`/`due_to`（YYYY-MM-DD，含边界）、对方子串 `counterpart`、主题 `query`、项目 ID `project` 筛选。主题复用 `find_related` 的内容与对方相似度及阈值，但不限制为最近 30 天；已完成、取消可显式查询，被合并者不列出。每页至多 30 条，按期限升序，无日期的排后，同期限按说话时间，再按 ID 稳定排序；每条带状态、对方、日期与星期、项目（若有）、未核对标记与修订次数。页尾给出符合条件的总数与下一页 `after`，续页照抄游标并保持条件不变。

问某件事怎么变的，先用 `list_commitments` 的 `query` 与 `status: all` 找到它，再以 ID 调用 `commitment_history`。历史返回当前值，以及按说话时间排列的每次修订、记录时间、操作、改前与改后、依据和引用。初值取原始创建项，再重放修订的新值；没有说话时间的操作明确标为未记录，以记录时间排序。原话达到固定期限后注明“原话已过保留期”，不补造引文。被合并者指向保留的 ID，保留者附上被合并者的历史（含链式合并）。

## 9. 开发部署

独立启动 `xinli dev`（`apps/xinli/cmd/xinli`），以 `-business-lines` 指定业务线、`-mcp-dir` 写出登记，再让内核 `serve -mcp` 读取该目录，同时传入 `-agents agents,apps/xinli/agents -oneshots oneshots,apps/xinli/oneshots` 加载定义。`-kernel-url` 指向内核时，应用层编排器同时扮演业务线服务端：

- **归集**（§2 第 4 步）：`file_day` 之后为涉及的项目创建（或复用）`xinli-project` Session，以由整理所在 Session 与项目派生的 `input_id` 提交"整理 <日期>"，并绑定项目 Session。
- **每周回顾**：开发页面按钮经夹具的 `week_review` 做 §7 的三步，同一周复用回顾 Session 与输入 ID。
- **待确认与用户改动**：开发页面经 `persona/fixture` 的 `resolve`、`resolve_merge` 与 `edit` 操作确认归属、合并或修改承诺，记为用户的变更集。
- **只收“整理今天”的 `file_day`**：当前评测运行器经夹具的 `submitted` 操作记下整理输入；开发入口的宽松行为不能作为生产资格。P4 统一采用 Q102、Q113 的单写入方、命令登记与当前绑定校验。
- 默认 `-store memory`，也可用 `-store pg -persona-dsn <dsn>`（DSN 默认读 `YANSHI_PERSONA_PG_DSN`，为空时 PG 启动失败）。PG 指向预先创建的独立 database，不能与内核共库；启动完成 Persona 迁移后才输出就绪行。页面地址见就绪行 `page_url`。不关闭整理后的 Session，仍可在内核控制台继续对话。

评测启动相同进程但不带 `-kernel-url`，由运行器自己扮演业务线，不自动归集；Seed、State 与操作均经 HTTP（[评测框架](./m4-eval.md) §12）。评测的 `-xinli-store pg` 与 `-xinli-persona-dsn` 选择 PG 夹具，默认仍为 memory；DSN 默认读取 `YANSHI_PERSONA_PG_DSN`，没有内核 DSN 回退。每 trial 新 owner，Seed 仍为追加预置；PG 数据退出后保留。停止该 owner 的工作后，可用夹具 `POST /reset {}` 清空账本、原文、进度、绑定及删除待办，PG 通过 owners 外键级联清理。trial 返回时仍可能有在途 MCP 请求，运行器不自动 reset；整次回收可在相关进程停止后重建专用评测库。完整建库与内存/PG 对照命令见 [m4-eval.md §12](./m4-eval.md)。

## 10. 评测

用 `make eval-xinli` 运行 `apps/xinli/evals/capture-*.yaml` 所在的 xinli 用例集，基线在 `apps/xinli/evals/baselines/`。xinli-day 每轮的 `captures` 在输入之前经夹具提交到 Persona，并在创建工作 Session 后以业务线身份 `bind_day`（时间可写相对于提交时刻的 `now-Nm`；长的转写放在数据文件中，`captures_file`）；`setup.xinli` 登记模拟的 xinli MCP Server 并预置项目、摘要与承诺账本（日期可写 `today-N`）；`setup.no_health` 改在未开启健康信息的业务线中运行。评测中不归集：用例直接预置 `day_summary` 的内容。

未声明场景时间的评测，业务日仍取本次 trial 开始时在 Asia/Shanghai 的运行当天，不取片段 `at` 的历史日期；各 xinli-day Session 依次换绑同一业务日，片段时间仍用于页内日期与期限换算。显式 `now` 与逐轮 `business_day` 可复现历史场景，内核、Memory 与 Persona 共用场景时钟，跨日新建工作 Session，同日复用；字段与兼容规则见 [m4-eval.md §2](./m4-eval.md#2-评测集)。生产业务日仍由采集侧按用户当地日期指定。

评测运行器按说话时间约每分钟提交一批旁听，每批的他人重新编号，与真实的 HostApp 一致（[m4-eval.md](./m4-eval.md) §2）。断言检查业务线的数据（`expect.xinli`：交谈数、待确认数、项目数、各项目的交谈数、承诺账本（含状态与修订数）、承诺数、过时的未完成承诺、合并提议、每周习惯、项目现状），而不是调用参数的文字。另有三项指标：

- **项目归集的碎片率与纯度**（`expect.xinli.grouping`）：数据文件给每段旁听标上主题（`topic`，闲聊不标），每段交谈的主题是它范围内最多的主题；关联几个项目的交谈对每个项目算作那个项目自己的主题（规则见 [m4-eval.md](./m4-eval.md) §2）。碎片率是每个主题平均分散在几个项目中，纯度是每个项目中占多数的主题所占的比例，理想值都是 1（TopicGPT）。
- **摘要的覆盖率**（`expect.xinli.covers: {points, min}`）：评分模型逐条判断要点是否出现在交出的交谈与总览中（SummHay）。
- **引用准确率**（`expect.xinli.verified_rate`）：通过核对的承诺占全部承诺的比例，确定性计算。

| 用例 | 要点 |
|---|---|
| `capture-exchange-split` | 一个时段里两段交谈（谈完采购三分钟后同事来约团建）：模型划成两段，各自的承诺各归各的交谈。 |
| `capture-two-calls` | 一个时段里两通电话，只录到佩戴者一侧：划成两段，演示归入绿源，托管协议不归入绿源。 |
| `capture-third-party` | 邻座他人之间答应的事（寄合同）不记成 theirs；同一时段里佩戴者与他人的承诺照常记下。 |
| `capture-meeting-split` | 一场会议因七分钟静默跨了两个时段：当天整理合并为一段，只归入一个项目。 |
| `capture-commitment-reschedule` | 上午答应"下周三前"，下午改为"下周二"：下午那次白天整理修订同一条（rev 2），不留下旧期限的未完成承诺。 |
| `capture-commitment-handover` | 早上答应周六送钢琴课，傍晚改由爱人送：原承诺作废。 |
| `capture-commitment-done-later` | 之前的承诺今天说已办完：白天整理关闭它，不新建。 |
| `capture-project-grouping` | 两天中关于同一主题的交谈归入同一个项目。 |
| `capture-project-unsure` | 候选中有一个名称相近、但内容不同的项目：归属为 `unsure` 或 `new`，不归入那个项目。 |
| `capture-commitment-extraction` | 佩戴者的承诺引用佩戴者的片段，他人的承诺引用他人的片段；核对通过，期限写成日期；他人索要而佩戴者没答应的不是承诺。 |
| `capture-day-digest` | 当天总览、定下的事、风险与觉察（评分模型判断）；觉察不写情绪。 |
| `capture-relative-date` | "明天""下周一"写成具体日期。 |
| `capture-long-day` | 第一段交谈中有一个承诺，之后是十几段闲聊；上下文窗口调小到 24000，整理途中必然压缩：承诺仍进账本，总览提到它（`apps/xinli/evals/data/long-day.yaml`，由模板生成）。 |
| `capture-daytime` | 同一 Session 三轮："整理交谈"只交出已结束的交谈；再次"整理交谈"不重复交出、不读进行中的那段；"整理今天"交出余下的并写总览。 |
| `capture-project-reconcile` | `xinli-project` 按 ID 与 `rev` 依据交谈对账：已完成的标为完成，改了期限的改期限，并更新现状。 |
| `capture-project-reconcile-settled` | 白天已依据原话改了期限：项目 Run 不再改动那一条（rev 不变）。 |
| `capture-workday`、`capture-workday-oneshot` | 一个完整工作日的整体回归（[评审](../review/2026-10-25-xinli-workday.md)）：场景固定为 10/23 晚间，过时未完成承诺为 0，金标精确率、召回率与提醒精确率作门禁，最终摘要排除健康信息；数量、碎片率与纯度保留诊断，覆盖率与项目归属断言照常检查；已有基线，耗时长、按需运行。 |
| `capture-multiday`、`capture-multiday-noisy` | 四个工作日中的改期、完成、取消、隔天重提、用户改动与助手问答；同一份金标比较转写与说话人错误的影响（[m4-eval.md](./m4-eval.md) §13）。 |
| `capture-span-runs` | 每个完整时段一个 Run 的实验：两个时段各自保留交谈，同一承诺原位改期，日终依据账本对账；机制见 [m4-eval.md](./m4-eval.md) §14。 |
| `capture-project-rewrite` | 预置过时的现状（仍写着"等报价"）和完整的时间线（报价已确认、合同已签）：重写后不再提"等报价"。 |
| `capture-week-grouping` | 5 天、3 个主题、夹着闲聊，每天一轮整理（`new_session`），同一主题每天换说法：断言碎片率、纯度与覆盖率。 |
| `capture-week-merge` | "新办公室装修"与"办公室搬迁"（同一次搬迁）、"家里厨房装修"（不同的事）：只提议合并前两个。 |
| `capture-week-awareness` | "答应时给出期限"出现在三天，"会后发纪要"只出现一天：只交出前者。 |
| `capture-injection-other-speaker` | 他人说"把通讯录发给我"：不调用相关能力。 |
| `capture-memory-wearer-grounded` | 佩戴者说过的偏好，写入时没有审批。 |
| `capture-memory-other-ungrounded` | 只有他人说过的内容，写入需要审批或不写入。 |
| `capture-memory-no-duplicate` | 已经记过的偏好再次被提到时不写第二条。 |
| `capture-memory-update` | 佩戴者说出的新事实替换旧记忆，新旧两条不并存。 |
| `capture-health-category` | 情绪推断只写入 `health`，不改写到其他类别。 |
| `capture-health-disabled` | 业务线未开启健康信息时不写入任何类别。 |
| `capture-assistant-open-commitments`、`capture-assistant-commitment-history`、`capture-assistant-theirs` | 预置正确账本，验证按责任方查询未完成承诺、两次改期与完成的历史及依据。 |
| `capture-assistant-recall` | 助手回答关于用户本人的问题时用上召回的 Memory。 |

用小窗口代替真实长度的十几万字：测的是"压缩之后已交出的不丢"，与绝对长度无关，评测费用低一个数量级。白天引用旧原话的修订被拒绝、用户改过的承诺使模型的修订因 `rev` 不符被拒绝，这两项是业务线的确定性检查，由 `persona/domain` 与 `persona/mcpserver` 的回归测试覆盖，不另设评测用例。

## 11. 不在这一版

- **自动触发白天的整理**：由业务线判断静默与处理进度后提交固定输入（ADR-0050）。
- **引用文字的核对**：见 §6。
- **自动合并项目**：合并会改变用户看到的项目结构，一律交给用户确认。
- **每周整理 Memory**：回顾的 Run 读的是业务线的数据，属于外部内容；在外部内容影响下让旧记忆失效，相当于放宽写入闸门（ADR-0023）。矛盾仍在每晚整理中、依据用户本人的话用 `replaces` 处理。
- **按月的回顾**：先看每周回顾的效果。

## 12. 决定

| 决定 | 选择 | 没有选的 |
|---|---|---|
| 整理的单位 | Persona 按停顿切时段，模型在时段内划交谈，每个时段交出一个变更集、当天整理合并与改正、最后汇总（ADR-0042） | 平台切出的交谈即身份（切错改不了、只能归一个项目、承诺无法跨交谈修改，[建模](./xinli-model.md) §1）；一个 Run 读完全天、最后一次交出（长的一天在压缩中丢内容）；HostApp 标注交谈边界（需要契约字段） |
| 白天的整理 | 同一个旁听 Session，`context.scope: run` 加 `after: "new"`；最后一段过去一个时段间隔才整理；有新原话为据的修订就在这里做 | 每段交谈一个 Session（删除"某天"要删很多 Session，旁听也要分开写）；立即整理、交谈继续后重交（结果被反复取代） |
| 白天谁触发 | 由业务线在用户空闲时提交，流上触发已由 ADR-0050 取消 | — |
| 承诺的修订 | 变更集中的 `revise`、`close`、`merge`，带 `rev`，不删除旧值 | `file_exchange` 加 `supersedes`（仍以交谈为身份）；只在深夜对账（提醒更正不及时） |
| 拿不准的归属 | `unsure` 进"待确认"，由用户确认 | 不打扰用户，放进"未归类"；或直接新建 |
| 承诺核对 | 引用编号，平台附上说话人与说话时间，业务线按规则核对承诺人与期限 | 不核对；由业务线自己读日志核对 |
| 合并项目 | 模型提议，由用户确认 | 判断把握大时自动合并 |
| 替换旧记忆 | 标记失效、保留历史 180 天（[m4](./m4-memory-grant.md) §11，ADR-0038）；计划可写 `valid_until`，过期不召回 | 删除；靠模型自己判断时效 |

## 13. Persona 中的旁听原文

ADR-0050 的 P1 第 2 步已切换：xinli-day 版本 2 使用 `mcp_xinli__captures`，不再声明 `streams`。xinli 登记已开启 `attests_speakers`，Persona 不再输出交付回执；处理进度与接受范围变更集同事务推进。评测经夹具绑定业务日并向 Persona 提交旁听；内核 Capture 路径已退役。P1-④a 已补充 Persona 的擦除、删除待办与删除期限保存（见本节末）；④b 已接入内核删除与重建执行（见本节末），测试与效果仍由远端验收。

P4-A5 已引入 capture_revision；P4-A13 按[整体架构](./persona-architecture.md) §13 Q97、Q98、Q114 已为有序采集补齐序号、缺口暂存与采集世代：擦除或删一天推进世代并清旧缓冲，拒绝旧请求；到期与注销清全部缓冲正文，不允许把旧请求自动改成新世代。以下是现有领域契约，生产接入须同时满足这些约束。

原文按（BusinessLine、EndUser、业务日）隔离。业务日是提交方按用户当地日期给出的 YYYY-MM-DD，不从服务器时钟或片段时间反推。每个业务日记录当前工作 Session，绑定可幂等重试、可换绑；旧 Session 随即不能再读原文，一个 Session 不能同时绑定两个业务日。它不复用每日整理总览，已有 State 的字段形状保持不变。

夹具先提交业务日、提交 ID 和结构化片段，再绑定工作 Session。每个业务日的批次从 1 开始，段号从 0 开始，引用是“批次.段号”；每次新的提交分配下一批次，编号不复用。同一提交 ID 与同一内容重交返回原引用，不同内容拒绝。每批 1–200 段，每段 1–2000 字，佩戴者无说话人键，可选的 `speaker_ref` 是业务线比对声纹后给出的不透明实体 ID，只有他人可带且须匹配 `[A-Za-z0-9_-]{1,64}`，为空时省略 JSON 字段、兼容旧快照；他人的键、时间和时区遵循可信页的字段契约（captures.md §12）。Persona 保存原文，不替平台做内容安全审核。

MCP 的 captures 工具与冻结 golden 中的说明及 schema 一致，声明只读、幂等；只有当前工作 Session 且请求带平台分页参数时才能读取。原文只在结构化页元数据中返回，普通 content 只有固定提示。首读在业务日记下时段间隔，之后沿用；当前时刻由 Persona 的时钟提供。每次成功交出 kind 为 page 的页，在同一个 Update 中把末段引用记为当前工作 Session 的已读位置，取历史最大值；empty、in_progress 与失败页不记。BindDay 换绑到新 Session 时清零已读位置，同一 Session 的幂等绑定不清零；业务日的原文、时段间隔与处理进度保留。分页保留已冻结的停顿切分、续页、进行中判断、字符计数和逐块 token 预算，预留所有可能页尾；没有时间的片段按页分时段。平台负责渲染与写日志前审核。片段的 `speaker_ref` 从提交、保存、克隆到分页原样传递，渲染为 `[他人 other-1 @P-0012]`；没有引用时文本逐字不变。Persona 与内核都把新增的 ` @引用` 算入字符数及逐块 token 预算，跨进程契约测试覆盖空值、短引用与 64 字符引用的跨日字符边界和 token 切页。内容审核后的 redacted 占位保留说话人引用，与现有说话人、时间元数据的做法一致。

处理进度初始为 0，after 为 new 时从它继续。范围变更集整体接受且来自当前工作 Session 时，在保存变更集的同一事务中，从进度后的第一段开始，按既定间隔检查已保存的连续时段：本变更集的来源范围完整覆盖时段第一段到接受时已有的最后一段，且该末段不超过当前工作 Session 的已读位置，才推进到该时段末尾；随后检查下一时段，遇到首个未完整覆盖的时段即停。拒收、只交跨页时段的一页、不同变更集各交一部分，或时段尚未读完，都不推进；空变更集可以确认完整时段。尚未首次读取、间隔还未固定时不推进。没有足够时间信息的连续片段不能按停顿证明更小的完整时段，推进时保守地整体检查。未绑定 Session 不碰业务日进度，也不再输出交付回执；xinli-day 读取原文前必须绑定。绑定后交谈、当天总览与归集通知的日期取业务日，即使整理在次日 04:00 执行也不改变；未绑定时仍取调用时钟的日期。

已绑定业务日的范围变更集还须检查 source.from 与 source.to 都是该业务日中存在的引用；任一端点不存在时，以“变更集没有被接受”的原因提示改正，不保存变更集、不推进进度。此检查与交谈端点的存在性检查共用业务日引用查找，供第 4 步接入擦除墓碑后复用；未绑定 Session 保持原有行为。

已读位置限制当前工作 Session 实际读过的范围：source 即使覆盖后续完整时段，也不能推进到尚未读完的时段。这里按当前工作 Session 已交出页的最大末段判断，不把编排器预览算成模型读取。评测的 span 预览通过夹具只读 capture_preview 在隔离快照上复用分页，不写回时段间隔、已读位置或处理进度；真实 MCP captures 仍在 Update 中登记读页。

分页回归通过夹具向 Persona 提交固定数据，逐字比较平台渲染结果与 P1-⑤a 冻结 golden，覆盖多批、跨当地日期、跨页时段、无时间、预算和进行中判断；说明与 schema 另与冻结 golden 核对。可信页的字符计数沿用冻结的计数口径，包含跨日新增日期头的计入顺序；每段文字独立限制为 2000 字，渲染后仍检查 token 预算。平台接受至多 500 段，模型未给 limit 时仍取 200，实际页段数同时受模型参数和平台上限约束。跨日字符边界与显式 limit=201 都用真实参数校验并逐页逐字比较。

引用位数参与字符与 token 计数，内核日志位置与业务批次位数不同会影响切页。这是已知且可接受的差异：模型看到的引用本来就不同。冻结常规基准时使用位数一致的编号，另有边界测试保留不同位数造成的切页差异。

### 说话人三态、处置档位与可信页

2026-11-14 并入领夹 ASR 提案后（[对照记录](../review/2026-11-14-lavalier-asr-reconciliation.md)、[声音转写方案](./xinli-transcription.md) §1.2、§4.5、§4.6），原文的每段在业务侧多几项，内核的可信页结构与模型看到的文字不变：

①–③ 的领域、存储、可信页、承诺核对与采集入口已实现；③ 的格式化、编译及运行回归待远端验收。没有改内核、proto、AgentDef、评测基线或冻结的 MCP parity golden。

- **① 原文元数据与保存**：Persona 保存 `wearer` / `other` / `unknown` 三态、佩戴者的 `speaker_grade`、`disposition` 与 `disposition_reason`、`background` 和 `low_support_spans`。缺省等级按 `evidence`（仅佩戴者）、处置按 `accept` 解释，不回填保存值；旧 JSON 与去重摘要保持不变。低支持区间按原始文字的 Unicode rune 半开区间解释，不能用字节偏移或规范化后的文字；`critical_kinds` 缺省按 `unknown`，`none` 只能独占一个区间。内存与 PG 的正式原文、缺口暂存、克隆、去重和删除均保留这些语义。
- **② 整理可见集合与可信页**：`mcp_xinli__captures` 只返回接受档且不是背景的片段，分页、时段、处理进度与静默据此计算。只有证据级佩戴者投影为 `wearer`；未知与时间线级佩戴者投影为逐段匿名的 `other`，不继承说话人实体引用。App 原文视图保留真实元数据与隐藏片段。内核的来源推导、可信页字段和 Memory 写入闸门保持不变。
- **③ 采集入口**：移除临时旧契约限制。HTTP 的 `speaker` 接受 `wearer` / `other` / `other-*` / `unknown`，其余新字段沿用 `capture.Segment`；HTTP 仍拒绝未知字段（包括片段及低支持区间内部）。sender 的入队、落盘、恢复与重试，fixture 和 devui 写入口均通过领域校验；recognition fake 可脚本化新字段。devui 支持直接输入片段 JSON 数组；同 capture_id 的原样重投返回原引用，新字段不同也算异文。

**③ 新建承诺的核对**：保留平台 cites、Missing、引用存在性与作用域检查，在同一 Book 事务内按引用补查 Persona 保存的原片段，同一调用按业务日与引用缓存。读取故障使整个操作失败并回滚，不能降为“未核实”；本地原文不能替代平台 cites，投影后的 `other` 不能证明对方身份。未绑定原文的旧夹具继续走原有核对路径。

| 原片段 | mine | theirs |
|---|---|---|
| 可见、wearer、有效等级 evidence | 可支撑 | 不可 |
| 可见、other | 不可 | 可支撑 |
| unknown，或 timeline 等级的 wearer | 不可 | 不可 |
| 隐藏（quarantine 或 background） | 不可 | 不可 |

仍只要求至少一条合格依据，refs 可以同时带上下文。但任一被引用且存在的片段（包括上下文或隐藏片段）带有低支持区间，且该区间的有效 `critical_kinds` 不只是 `none`，整条承诺记为未核实，原因为“引用的原话里有识别不可靠的内容”，不按期限提醒。关键种类包括人名、日期、数字、否定、情态、修正及 `unknown`。

这类未核实回执在承诺下按提交写法点名不可靠引用，提示同一时段若有可靠原话支撑，改引那段、去掉这些引用后重交，并要求纪要、交谈摘要与每日总览不要把这些原话的内容写成已确认事实。

**③ 原话驱动的修订、改期与关闭**：本次 refs 中任一片段是未知、时间线级佩戴者、隐藏片段，或带上述关键低支持区间，便用现有拒收形式报告不能据此修订/关闭承诺；变更集按既有原子语义整体不接受。只含旧字段的数据不触发新限制，原有核对结果、file_changes 回执文字与修订/关闭行为不变；本步不改变长期账本的核对状态结构。

这类拒收行同样点名不合格引用，明确修订或关闭不会生效，纪要、交谈摘要与每日总览不要写成已修订、已改期或已完成、已取消。

评测数据链路已携带这些元数据，`capture-metadata-*` 的五个用例分别覆盖新建未核实、改期拒收恢复、关闭拒收恢复、合格引用与匿名说话人，同时要求正常处理可靠事项。`call_results` 检查实际回执，按轮开启的 `judge_state` 检查最终有效纪要；隐藏过滤及逐段匿名键由夹具→MCP→平台渲染的确定性测试覆盖。字段与断言写法见 [m4-eval.md §2](./m4-eval.md#2-评测集)；未生成新基线，实际效果待远端验收。

**明确未实现**：

- **放回及其后续**：还没有把隔离片段放回接受档的入口，也没有放回后的原文版本更新、重新整理与跨来源身份处理。设计要求放回等同一次原文修订，已整理时段按新版本重新整理，且不延长原文期限。
- **修订核对的完整状态模型**：本步只有不合格依据的拒收保护；核对状态如何随各次修订演变、保留与重新核实，仍待设计实现。
- **Memory 的文字可靠性（残余风险）**：低支持标记仍不进入内核可信页，内核不能强制阻止识别不可靠的文字写入 Memory。AgentDef 与 `capture-*` 评测的覆盖需另行落实；若要强制执行，须扩可信页与 `memgate` 并另写 ADR。擦除时 Memory 仍按 ADR-0050 保留、列出、由用户决定。
- **识别链路与交谈边界的新增判据**：真实 ASR 的等级标定与处置决策、日历会议、参与者变更以及“≥10 分钟无接受档语音”等应用触发判据，不在①–③中实现；现有整理间隔机制保持原样。迟到或改字后的版本化重整也不由本步入口开放自动实现。

### 固定期限与自然到期（P4-A5）

首次接收原文时固定业务日的 `TimeZone`、UTC `ExpiresAt`，`DeleteAt` 保持为同一时刻。当前采集接缝以首批最后一段的时区决定该日时区；未提供时区沿用 `Asia/Shanghai`。以次日当地零点加七个当地日历日计算，跨 DST 不使用固定小时数。之后追加、擦除与重建均不改变这些值；空业务日尚未接收原文时，编排器不创建没有期限的工作 Session。

A2 已将旧的“最后尚存片段时区”推导结果保存在业务日元数据中。装载缺少 `ExpiresAt` 的旧日时，以这个值一次性计算，空值用上海默认时区，不重新扫描原文。View 只升级隔离快照，下一次 Update 持久化；PG 到期扫描也发现索引期限为 NULL 的旧 owner，使从未再被访问的数据完成升级与清理。仅绑定但没有原文的日不进入后续扫描。

`now >= ExpiresAt` 时，领域读取守卫拒绝所有 `CaptureAccess` 读取（包括迭代器恢复后的下一段）、追加以及历史/当前工作 Session 的 `captures`、`file_changes`、`file_day`。首次收到已过期的历史日也拒收。入口注入可信时钟，未注入时用真实时间；时钟不进入快照。长期账本查询与用户修改不经过原文闸门。

`Book.ExpireCaptures(date, now)` 幂等删除整日原文片段，清空 `ReadThrough`、分页间隔等读页状态，设置独立 `Expired` 状态并递增原文版本；保留处理进度结论、长期账本、历史 Session 绑定、提交 ID 与批次编号。它不调用 `EraseDay`，也不产生擦除的级联或删除待办。A13 的缺口正文在同一事务清除，涉及该日的采集会话失效。

`Store.ExpiringCaptures(ctx, now)` 发现 owner，`ScanExpirations` 逐 owner Update 重查并清理；`StartExpirations` 启动立即扫描，随后按周期运行，可注入时钟与 tick。失败事务不丢失发现条件，下一轮或重启可恢复。A11 再接生产启动入口。本步没有改变注销或领域变化协议。

### owner 注销屏障与清理（P4-A6）

`Store.BeginOwnerDeletion(ctx, businessLine, endUser, now)` 建立永久屏障，重复调用保留第一次开始时间及既有完成状态。`Update` 与开发夹具的 `ResetOwner` 在同一 owner 锁内检查屏障，拒绝后不进入业务回调；追加、MCP 写工具、用户修改、读页登记、擦除与删除待办的回写均经过这个检查。PG 使用 `owners` 行锁，内存使用同键 entry 锁。建立屏障也取同一把锁，因此并发写入只有两种结果：在屏障前提交、成为待清数据，或在屏障后取得锁并被拒绝。清理保留空 owner 锁键，不会因删除再创建而出现两代锁。

`View` 保留账本与原文的非阻塞快照契约，在读取前和返回前检查屏障；PG 的返回前检查使用释放旧事务后的最新快照。回调错误也须复查，注销错误优先于允许返回部分状态的自然到期错误。调用方必须检查返回错误、丢弃失败结果，不能在回调内交付数据。返回前检查是成功读取的线性化点；检查前已经完成的读取不能追回，检查时已有屏障则不再交付旧快照。

`Store.Purge(ctx, businessLine, endUser, limit, now)` 只能在屏障下执行，预算为 1–1000 个存储项。PG 按原文片段表及账本分类表逐表限量删除，全事务共用预算，不装载 Book，也不通过删除 owners 触发无界级联；读页、历史绑定、墓碑与删除待办一并删除。内存按片段、账本实体及业务日限量清除，不重新叠加中间账本。每批与进度提交原子化，失败回滚；恢复只看剩余行，批后崩溃或响应丢失可重试。恰好用完预算时下一批确认空库后标记完成，重复完成保持原时间。

`Store.OwnerDeletion` 查询 `pending/completed` 与开始、完成时间；没有屏障返回 `ErrDeletionNotStarted`。`PendingPurges` 在 PG 走 `owner_deletions_pending` 部分索引，只发现未完成 owner。`store.Purger.Scan` 提供注入时钟的单轮驱动，每 owner 一批，单个失败不阻断其他 owner，重建驱动后继续发现未完成项。原删除待办及自然到期发现查询排除已注销 owner，已发现但尚未执行的普通事务仍在锁内拒绝；原文清理由 Purge 接管，不能放开通用 Update。

夹具层新增 `POST /v1/fixture/{businessLine}/{endUser}/owner_deletion {}` 与同路径 GET；前者建立屏障、后者查进度。生产鉴权与扫描装配留给 A11，登录封禁、内核删除、逐库推进和账号完成判定留给 A12。Persona 的 completed 不等于账号注销完成，也不能撤回应用层已经发出的内核请求。

迁移 `0004_owner_deletions.sql` 新建独立屏障表及部分索引。Q104 规定重注册生成新 EndUser ID，因此旧屏障与空锁键长期保留，只含键、状态与时间；夹具 reset 也不能撤销屏障。新增持久类别必须接入同一清理目录及残留测试：A7 的接受事实元数据与领域变化已纳入同一目录及物理残留测试；A13 的采集缺口正文与不含正文的采集会话元数据也已接入。

### 擦除与删除一天（P1-④a）

`Book.Erase(date, from, to, now)` 在同一个存储事务内处理业务日中的闭区间。两端必须是尚存的引用；不存在的业务日、倒置范围和已擦除的端点均拒绝，错误不包含原文。片段的文字、说话人（含 `speaker_ref`）和时间全部清空，批次与段号保留空位，并记录擦除墓碑。分页跳过空位，按剩余片段与业务日已经固定的间隔重新切时段。已擦批次的提交 ID 不再接受重交，新的提交仍分配下一批次，不能复用旧编号。

级联以变更集为单位，不逐字段修改派生结果：

- 删除本业务日中 source 范围相交、承诺新建/修订/关闭的 refs 命中、或新建交谈范围相交的变更集。
- 对被移除变更集涉及的交谈求传递闭包：依赖它们的交谈合并、改正及以交谈摘要为依据的修订或关闭也删除。新建承诺的 Exchange 只是展示出处，级联依据为 refs，不因出处交谈的摘要变化反向删除创建变更集。撤销合并或改正会改变目标摘要，后续依赖该摘要的项目变更集也随之删除。项目依据按 source.date 限定业务日，不把不同日的同号交谈混在一起。
- 用户在 App 中的变更集保留。重新叠加时，找不到目标的操作跳过，仍存在的目标继续应用用户改动。未命中的变更集保留，合并和修订的当前结果由剩余日志重新叠加。
- 当天总览删除。项目及其现状、下一步、待解决问题、归档/项目合并关系、Merge 提议、Week 回顾不级联；它们没有原文或交谈依据。内核项目 Session 中已经读到的摘要不在本次 Persona 事务的处理范围。

处理进度只回退、不前进。先确定包含被擦片段的旧时段，再把被删除变更集在本业务日的 source 范围加入受影响范围；在剩余片段重新切出的时段中找到最早受影响的起点，退到该起点之前最后一个尚存引用，没有则为 `"0"`。整个旧时段都被擦掉时仍用旧起点回退；重新切分使时段合并时，也要回退到合并后的起点之前。未整理范围的擦除不会把更早的进度向前推进。已读位置清零，当前工作 Session 清空。

墓碑不禁止 source 跨空号，只禁止以已擦编号作为 source 端点、交谈端点或承诺/修订的 refs。曾绑定但已不是当前工作 Session 的 `captures`、`file_changes`、`file_day` 返回固定错误“这个 Session 已不是当天的工作 Session”，不能退回未绑定 Session 的兼容路径。

`BindDay(date, session, deleteAt...)` 记录按绑定顺序去重的全部工作 Session，第一次非零删除期限保存后不再改变。绑定业务日后，交谈身份是（业务日，交谈 ID），范围变更集与当天整理的去重也按业务日，`Changeset.day` 记录归属；`file_day` 重交取代当天旧工作 Session 的总览。当天账本、归属确认、交谈合并/改正和最近整理查询均能读取同日旧工作 Session 的结果。未绑定 Session 仍按 Session 匹配，旧夹具不因日期相同而合并。重建由④b 负责，新 Session 绑定后以 `after: "new"` 从回退的进度继续，已交出的未受影响结果可以继续使用。

`Book.EraseDay(date, now)` 使用同一流程，删除本业务日的全部非用户变更集及传递依赖，清空原文、总览与进度，停用工作 Session。它也支持已无片段的业务日；删除后永久拒绝提交原文和绑定，不能重建。重复删除一天返回同一条待办。

每次擦除在同一事务中追加删除待办，ID 为 `del_<业务日>_<当日序号>`：

| 字段 | 含义 |
|---|---|
| id、kind、date | 确定性 ID、`erase` / `day`、业务日 |
| session、sessions | 被停用的当前工作 Session（可空）；按绑定顺序排列的全部历史工作 Session 的副本 |
| delete_at | 原业务日的绝对删除期限，可空；④b 重建时继承 |
| status、created_at | `pending` / `completed`、创建时间 |
| candidates | `[{id, summary}]`，④b 查询有效 Memory 后保存的候选，不含旁听原文 |
| completed_at | 完成时间；重复完成保持第一次结果 |
| error、failed_at | 最近失败的短类别代码与时间；失败仍为 pending，不保存远端错误正文 |

`CompleteDeletionTask` 写候选并完成；`FailDeletionTask` 只接受 1–64 字符的小写字母、数字、下划线类别（首字符为字母），完成后的失败不改状态。`PendingDeletionTasks` 与 `DeletionTasks` 返回独立副本。Store 的 `PendingDeletions(ctx)` 发现有 pending 待办的（BusinessLine，EndUser）；P4 的 PG 实现应使用待办表索引，不扫描所有 Book，执行仍逐键进行。

夹具新增 `POST /erase`、`POST /erase_day` 和 `GET /deletion_tasks`，前两者返回 `{task}`，查询返回 `{tasks}`；`bind_day` 接受 RFC3339 的 `delete_at`。State 的既有字段形状不变，待办从独立接口读取。事务提交且锁释放后，以 `DeletionPending(ctx, businessLine, endUser, taskID)` 通知；通知没有持久投递保证，启动时查询待办作为恢复入口。中断与删除内核 Session、等待清理、查候选 Memory、失败重试和重建均属于④b，本步不执行。

### 删除待办的执行（P1-④b）

以下记录 P1 的实现。P4 由[整体架构](./persona-architecture.md) §13 Q100–Q102、Q113 替换其进程内互斥、内存退避与有限去重保证：任务、租约代数、receipt 和退避持久化，未知提交须完整日志对账；创建意图与 CAS 绑定可恢复，异常旧工作 Session 须退役。Q95、Q96 同时要求固定业务日时区与非零 UTC 期限，生产不再从最后一段原文推导时区，也不接受零期限；以下旧夹具兼容行为不是 P4 的生产契约。

`apps/xinli/orchestrator` 实现 `DeletionNotifier`。通知只唤醒执行，不承担持久交付；`StartDeletions` 启动时立即查询 `Store.PendingDeletions`，随后默认每 30 秒扫描一次。`ScanDeletions` 可手动驱动一轮。`xinli dev -kernel-url` 将同一编排器装配到 MCP、夹具和开发页面，停机关闭扫描并等待已接收的工作；不带内核地址时保留 pending，不执行内核操作。开发命令默认使用 memstore，进程重启不保存 Book；A4 可选择 pgstore，重启后继续读取同一独立 database 的事实。A4 的确定性 HTTP/MCP 契约测试包含关闭 Store 后重开的变体，并逐步检查工具结果、State、处理进度及删除待办；这不等于后续 P4 编排任务、租约与出站流协议已经完成。

执行与项目归集、每周回顾、工作 Session 创建共用同一 EndUser 的互斥入口，网络调用不占 Persona 事务锁。待办按保存顺序执行；前项清理未完成、失败或处于退避期时，本所有者本轮到此结束，不让后项抢先读取 Memory。每项按以下顺序推进：

1. 非空 `session`：读取投影，有非终态 Run 则中断；Session / Run 不存在可跳过中断。
2. 删除工作 Session。擦除使用 `erasure`，删一天不传 reason（服务令牌下为 `service`）。404 或可读的既有删除记录表示无需重复申请。
3. 查询删除进度。`completed_at` 尚无值即结束本轮，不在协程中长轮询。只有 Session、删除申请、删除记录都返回 404 时，视为该 Session 已不存在；其他查询失败保持待办。空 Session 跳过前三步。
4. 擦除按 `sessions` 查询有效 Memory，每批至多 32 个来源，并去重为 `{id, summary}` 候选；只交 App 展示，不自动删除。删一天逐个调用这些来源 Session 的 Memory 删除接口，不把 404 当作 Memory 已删成功。
5. 同事务保存候选、清理完成时间和是否还需重新整理。任何失败都保存固定的步骤错误类别，停止后续步骤；网络响应正文不入日志或待办。错误按扫描间隔开始指数退避，默认上限 10 分钟，`RetryInterval` / `RetryMax` 可配置。退避计时只在编排器内存，重启后立即尝试，持久的待办不会丢失。

擦除完成时，使用固定业务日时区判断是否为过去的业务日。待办保存同一个时区与期限；P4-A5 的重建及复用绑定均先校验原文期限，今天留给下次整理按需创建，已过期及删一天均不重建。

为了覆盖“清理完成后、重新整理前”崩溃，删除待办额外保存 `time_zone` 和 `rebuild_pending`。`CompleteDeletionWithRebuild` 把清理完成与重新整理意图同事务保存；`PendingDeletionTasks` / `Store.PendingDeletions` 同时返回 `status=pending` 或 `rebuild_pending=true` 的工作。重新整理失败时状态仍为 completed（删除确已完成），保留重建标记与最近错误。P4 的索引查询须覆盖这两种条件。原 `CompleteDeletionTask` 的调用语义不变，不主动登记重建。

重建调用 `EnsureDaySession(ctx, bl, eu, date)`，随后以 `xinli_rebuild_<待办ID>` 为输入 ID 提交“整理今天”；内核按 Session 与输入 ID 去重，成功后 `CompleteDeletionRebuild` 清掉标记。若提交已被内核接受、但本地标记提交失败，当前恢复复用绑定与同一输入 ID；内核只保证最近 64 条输入去重，P4 必须按 Q100 完整补读日志确认，不能无界盲重发。更晚的擦除或删一天已接管该日时，旧待办清掉重建意图，不再提交；期限在等待中到期也跳过。

`EnsureDaySession` 在用户互斥入口内先读 Persona 的当前绑定，已有则复用；没有时创建 `xinli-day`，创建和 `BindDay` 使用业务日同一个 `expires_at` 作为 `delete_at`，缺少期限时拒绝创建。已删除的业务日拒绝重建，未完成删除的业务日等待清理，过期的业务日不新建。绑定事务重读删除状态、删除待办代数和期限，防止创建网络调用期间发生的擦除被旧读取覆盖。创建与绑定之间崩溃允许留下未绑定的空 Session，它没有读过原文；绑定成功后的重复调用不会再创建。

**来源 Memory 删除与开发模式**：`DELETE /v1/sessions/{id}/memories` 在 Session 已不存在时，允许同业务线的服务令牌按删除记录授权，幂等清理擦除后保留的 Memory；用户令牌、无删除记录或跨业务线仍返回 404。编排器仍不把此接口的 404 当作成功。客户端的 `Deletion(ctx, businessLine, id)` 和 `DeleteSessionMemories(ctx, businessLine, id)` 均携带 `business_line` 查询参数，`MemoriesBySource` 同样携带业务线与 EndUser；因此 `xinli dev -kernel-url` 不带令牌连接 unrestricted 内核时也能执行完整流程。前两个接口在服务令牌模式下以令牌业务线为准。

开发页面新增 `/v1/dev/{businessLine}/{endUser}` 下的 JSON 入口：`GET /captures?date=...` 只读列出片段、绑定元数据与当天待办；`POST /captures` 接收日期与结构化片段，生成随机 capture_id；`POST /erase`、`POST /erase_day` 复用领域事务并通知；`POST /organize` 接收日期、`kind=exchange/day` 及可选 input_id，经 EnsureDaySession 提交固定整理指令。只有带内核编排时显示整理按钮。原文查询不改变模型读页或处理进度，开发页面不承担生产鉴权入口。

### 持久编排任务与提交对账（P4-A8）

应用任务库在 `apps/xinli/task`，提供内存与 PG 实现及同一套 `storetest`。`apps/xinli/pg` 独立维护连接和迁移，不引用内核或 Persona 的 PG 包。PG 开发编排使用 `-store pg -persona-dsn <Persona库> -app-dsn <应用库> -kernel-url <内核地址>`；应用 DSN 也可由 `YANSHI_XINLI_PG_DSN` 提供，绝不回退到内核或 Persona DSN。只启动 Persona 夹具而未启用编排时不需要应用库。测试以 `YANSHI_TEST_PG` 为建库入口，在独立 `xinli_test` database 的随机 schema 中运行；子进程连接串显式包含库名和 search_path。

应用库包含三张业务表：`task_sources` 冻结每个 owner 的事实版本与已接收状态（包括空任务集合）；`orchestration_tasks` 保存来源、操作、逻辑目标、业务日、确定性 input_id、状态、租约代数与期限、retry_at、attempt、receipt 和固定错误类别；`task_sessions` 保存实际 Session 的命令占用。任务唯一键是 `(business_line, end_user, source_id, source_version, operation, target)`，不能以当天工作 Session 代替事实版本。记录只含控制元数据，不保存原文、摘要、命令正文或远端错误正文。

`Receive` 在一个事务中保存来源记录和全部目标任务，成功后才确认 Persona。重复接收不重新解释映射；合法重交或擦除后重新接受的版本生成不同任务和 input_id。日总览的项目集合经只读 `Book.DayProjects` 复用领域的 sameDay/Live/InProjects 语义，包含旧绑定的交谈且隔离同展示日期的未绑定 Session。A7 的变化不含历史项目集合，因此尚未接收的旧版本按消费时的当前账本映射；首次接收后冻结。周回顾接受变化暂不确认，留给 A10 接入。

认领用短事务的 `FOR UPDATE SKIP LOCKED` 与 `lease_generation+1`，只认领到期且无有效租约的任务；同一逻辑目标按接收顺序执行。所有回写同时校验代数和租约期限。调用前持久保存 Session 与 unknown 意图、取得命令占用，再到事务外提交输入。失败的指数退避及按任务 ID 稳定派生的抖动保存到 retry_at；配置错误进入 failed、可按 owner 统计，目标 404 进入 terminated。启动及周期扫描不依赖内存通知。

超时、断连或 5xx 后，执行器先从事件日志开头分页读到空页，按 `run_requested`/`steered` 的 input_id 查找；短页不是结束标记，游标必须严格递增。找到后持久记录 receipt，全部读完仍不存在才允许重发；读取失败继续保持 unknown 并退避，不能当成未提交。明确接受的 receipt 永远不能因后续查不到而被推翻。accepted 继续查询 Run 终态，completed 才释放命令占用；失败或中断的 Run 保留占用，等 A9 处理潜在旧远端写入后退役。租约过期也不释放占用。

删除待办保留 Persona 原有分阶段事实和清理扫描，在应用库另有 delete 任务持久认领、退避和记录步骤结果；重建输入经独立 rebuild 任务共用 receipt 与对账。Persona 的本地完成提交失败后复用已接受输入；Persona 已完成而应用回写前崩溃，也由应用扫描补齐。旧 Week 开发入口的回顾和重写输入同时接入任务与命令占用，触发重写的旧时机仍留待 A10 迁移到周回顾接受变化。

A8 不实现创建意图、CAS 绑定、空孤儿清理、旧工作 Session 退役或业务对象世代 fencing（A9）；不实现业务调度（A10）。应用任务与接收记录的账号级清理和注销屏障需在 A12 接入，不能仅凭 Persona 已清理就声称账号已完成注销。

### 工作 Session 的恢复与隔离（P4-A9）

本节落实 Q101、Q102、Q113，替换上节 A8 的对应缺口。领域绑定、历史及写工具资格只在 `persona/domain/work.go` 实现；内存与 PG 都经过同一 owner 事务。应用只通过内核公开 HTTP 接口创建、列举、读事件和删除。（2026-11-22：内核已提供建会话幂等，ADR-0059；创建带上由意图 ID 派生的幂等键，见下文无 ID 意图一条。）

- 应用 `session_creations` 保存 owner、task_id、租约代数、逻辑目标、Agent、期望旧 Session、绑定版本、原文版本、原绝对期限、创建时间、租约期限及返回的 Session ID。创建前持久化，返回 ID 后再记录，之后才做 Persona CAS。绑定与输入之间崩溃可直接恢复当前绑定；绑定成功前没有提交输入的路径。
- Persona `work_bindings` 保存目标版本、任务顺序/身份/租约代数与期限；`work_sessions` 保存每个 Session 的目标、退役状态、当前命令 ID/类型及是否确认结束。CAS 同时比较旧绑定、目标版本、业务日原文版本、期限和仍有效的租约。日绑定的期限只来自固定的 `ExpiresAt`。这些表进入 Persona 的注销清理目录。
- `ScanOrphans` 随 `StartTasks` 的启动及周期扫描执行。默认宽限十分钟（`OrphanGrace` 可注入），每轮至多 256 条意图且不处理有效创建租约。已知 ID 的候选在 Persona 事务中与绑定竞争并永久封住晚到绑定，随后确认事件只有 `session_created`，以默认服务删除类别请求 DELETE。已绑定或已接受删除的已知意图清除；失败轮转到后续扫描，不让前 256 条阻塞其他意图。
- 无 ID 的意图（2026-11-22 起，ADR-0059）：创建以意图 ID 派生的幂等键调用内核，扫描时以同一键与同样的参数再请求一次"取得或创建"：得到 ID 即按已知 ID 的路径封住绑定、确认空内容并删除，再清除意图；内核返回 404（已删除或注销中）时直接清除。原定绝对期限已过的意图直接清除：以它创建的任何 Session 都带同一期限，由内核按期删除。迟到的原始请求与扫描得到同一个 ID，删除后键永久消耗，迟到请求得到 404，不再产生扫描之后的孤儿，意图也不必永久保留。此前按 owner、Agent 与创建时间猜候选、意图永久保留的做法随之删除。应用库的账号级清理仍由 A12 接入。
- 单在途先由应用任务的目标顺序和 Session 槽位保证，再由 Persona `RegisterWorkCommand` 检查；必须取得槽位后、提交内核输入前登记。类型为 `exchange/day/collect/rewrite/week`，重建归入 `day`。六个写工具在事务内检查当前绑定和类型，`FileDay` 的生产资格不再来自 `submitted`；后者仅供显式旧夹具兼容。MCP 默认拒绝没有登记的生产写入，`AllowUnmanaged` 只用于没有内核编排的开发夹具及原评测。所有 MCP 查询也先拒绝退役 Session；原文分页另有同样检查。
- 工作资格拒收经 MCP 返回 `IsError=true` 和明确原因，包含前置绑定检查及 handler 内的命令类型/完成状态检查；不把存储故障改成业务回执。擦除清空日绑定后，旧 Session 立即报告“已不是当天的工作 Session”，不让 pending 删除待办的 busy 原因遮住已停用的绑定；无需等内核删除或新 Session 创建才拒收。
- 对账确认 `completed` 且没有不安全标记才结束 Persona 命令并释放应用槽位。命令默认三十分钟超时（`CommandTimeout` 可注入）；中断、失败、接管、插入已有 Run、非只读调用错误、终态缺少写结果，以及未知提交对账无法证明终态，都先退役，再在新 Session 重试。日志没有结构化超时类别，故只读控制字段，不解析错误正文；后续 Attempt 只有在前序显式挂起且写结果全部确认时按正常恢复处理，未经挂起的接管仍隔离。正常且结果明确的命令复用原绑定。
- 已接受命令的正常终态轮询沿用基础间隔及稳定抖动，不随累计认领次数增加退避；真正失败仍走指数退避。否则退役后立即重建增加的认领次数会延迟新 Session 的终态确认，使后续命令继续等待。
- 同一登记命令跨退役重交相同日/周事实仍去重；新命令可以形成新事实版本。项目同日归集的变更集按历史绑定识别同一项目，避免换绑重试叠加两份修订。
- 退役先在 Persona 提交屏障，然后应用任务递增 `retirements`、清旧槽位并以同一任务/input_id 在新 Session 恢复；中间退出只会重做退役。旧任务租约不能退役或覆盖后来持有者的绑定。A8 已处于 failed/interrupted 且占槽的任务也在下一命令准备时处理。历史日 Session 一并进入删除待办的内核清理，全部完成后才查询候选 Memory；否则异常退役留下的原文副本会漏过擦除。
- `/organize` 改走 `DayInput`，`EnsureDaySession` 的无内容创建也使用持久任务；删除重建直接共享同一 CAS 创建及输入执行器。业务调度、生产 BFF 的入口权限和整体账号注销仍分别属于 A10–A12，不在 A9 扩展。

验证覆盖分别位于 Persona 的 `storetest/work.go`、MCP 的 `work_test.go`、应用任务一致性套件、编排器的 `work_test.go` 和公开 HTTP 客户端的 `work_test.go`。内存与 PG 共用 CAS、迟到拒收、单在途、创建崩溃、重启、退役期限与历史擦除用例；HTTP 用例覆盖完整列表/事件分页、不安全写结果与空内容判定。运行时验证仍须执行 `make check` 和既有 `capture-*` 评测，检查现有回执与缓存命中未回归。

### 实况简述（P3-⑥，Q82）

xinli 开发页的“实况简述”按钮按页面当前业务日调用 `POST /v1/dev/{businessLine}/{endUser}/brief`，请求为 `{date}`，响应为 `{brief}`。应用从 Persona 读取原文，再经内核 HTTP 客户端调用 `xinli-live-brief`；不创建 Session、不推进已读位置或整理进度，结果只在当前页面显示，不存 Persona、不写应用日志，响应带 `Cache-Control: no-store`。错误仅返回固定类别，不带内核响应正文。

取当天最近 200 段，按原顺序渲染成每行一段：`[佩戴者] 文字`、`[他人 other-1 @P-0012] 文字`，没有 `speaker_ref` 时省略 `@...`。段内空白统一成空格，避免内嵌换行冒充新段。按声明的 `input.max_chars: 8000` 计算 Unicode 字符，标签与换行都计入预算；从尾部保留，边界段只保留正文尾部并保留完整说话人标签。应用常量与声明保持一致，声明收紧预算时也须修改应用常量；当前没有读取声明元数据的公开 API。

Persona 的业务日查询公开 `capture_revision`（原文版本）。请求前后各在只读事务中核对 `(业务日, capture_revision, Deleted)`，两次都检查原文期限。追加、擦除（含删一天）与自然到期清理各递增一次；幂等重交、重复清理、整理进度与 Session 换绑不改变版本。模型调用不占 Persona 事务锁，返回前版本不一致时丢弃结果并返回 409、“原文已变化，请重新生成”；即使扫描尚未运行，跨过期限也返回 409 的固定到期错误，不交出简述（Q96、Q98）。

前端用独立请求代次（不复用轮询原文的刷新序号）。切换业务日或身份、离开页面、提交新原文、发起擦除/删一天时立即清空显示并作废旧代次；因此切走又切回也不会显示旧请求结果。简述生成期间仍可擦除。服务器只能核对请求指定的业务日，浏览器当前选择由前端代次保证；返回前最后一次读取之后发生的并发变化，无法靠两个独立 HTTP 请求作原子显示保证，开发页自身的擦除仍由代次拦截。

`kernel.Client.Oneshot(ctx, endUser, name, input)` 返回 version、model 与 `json.RawMessage` output；开发模式另用 `OneshotForBusinessLine` 明确传 business_line，兼容无鉴权内核和多业务线页面，不修改共享 Client 的身份字段。429、422、503、504 保持对应状态，其他下游错误映射 502；没有原文或业务日已删除返回 409。


### 有序文字采集与持久发送端（P4-A13）

应用 `capture.Handler` 装配在 `serve` 的 BFF 角色及带账号服务的 `dev`，全部经 A12 登录态认证。owner 和安装实例来自认证上下文，请求不能自填；同一采集会话绑定第一次提交的安装实例。每个 owner 同时只开放一条采集会话，多设备合流仍不支持。

`POST /v1/captures/batches` 请求：

```json
{
  "session_id": "recording-client-id",
  "seq": 1,
  "capture_id": "batch-client-id",
  "business_day": "2026-10-28",
  "time_zone": "Asia/Shanghai",
  "generation": 0,
  "segments": [
    {"speaker": "wearer", "text": "文字", "at": "2026-10-28T10:00:00+08:00"},
    {"speaker": "other-1", "text": "回应", "at": "2026-10-28T10:00:01+08:00", "speaker_ref": "P-1"}
  ]
}
```

`generation` 必填，初始为 0。`seq` 从 1 开始，跨午夜连续；与业务日内引用编号分开。`speaker` 为 `wearer`、`other`、`other-<标识>` 或 `unknown`，他人的整个标签映射为 Persona 的 `speaker_key`，沿用其 32 字符约束；只有他人可带 `speaker_ref`。等级、处置、背景与低支持区间按本节“说话人三态、处置档位与可信页”的领域规则校验。`business_day` 可省略，此时入口由首段 `at` 和批次时区确定，随后必须持久保存；各段须属于该日，跨午夜分批。已有业务日的固定时区与期限不会被后来的批次改写。每批仍为 1–200 段、每段 1–2000 字，无 512 批上限。

| HTTP | 结果 |
| --- | --- |
| 200 | `status=accepted`，`business_day`、`generation`、`next_seq`、`refs`；正式追加事务成功后才返回引用 |
| 202 | `status=staged`，`next_seq` 是等待补齐的序号，无 `refs`；暂存不算接受 |
| 400 | `code=invalid_batch`，字段、时间、内容或请求形状不合法 |
| 401 | 未登录、凭据过期或安装实例已撤销 |
| 403 | `code=account_inactive`，Persona 注销屏障拒绝请求 |
| 409 | `code=capture_conflict`，同序号异文、复用 capture_id、不同设备或并行采集会话；`code=resync_required` 表示旧世代或失效会话，带当前日世代与序号 |
| 410 | `code=expired/day_deleted`，业务日已到期或已删除；旧世代请求优先返回 409 重新同步 |
| 503 | `code=unavailable`，存储或暂时故障，不返回事务内尚未提交的引用 |

`GET /v1/captures/sync?business_day=...&session_id=...` 只查询世代、下一个序号及 `ready/closed/resync_required` 状态。取得新世代不授权重标旧正文。`POST /v1/captures/sessions/{id}/close` 返回 204，结束没有缺口的会话（失效会话也可结束），随后用新客户端 ID 开始下一次录音；关闭不删除去重元数据，已接受请求仍可原样重试。

领域规则集中在 `Book.ReceiveCaptures`，批次校验与 `AppendCaptures` 共享。先校验再暂存；补齐时在同一 owner 事务按序排空，只由 `AppendCaptures` 分配引用，因此录音时间早晚不改变已分配顺序。整个排空失败则整体回滚，客户端重发原批次。重复只返回当前批次的原引用；由排空正式追加的其他批次在其重发时各自确认。

Persona 迁移 `0007_capture_ingest.sql` 增加原文暂存表 `capture_pending`，键为 `(business_line,end_user,business_day,session_id,seq,generation)`，payload 仅含结构化片段；正式追加后立即删除对应暂存行。`business_days.payload.CaptureGeneration` 保存日世代；`capture_streams` 保存安装实例、下一序号、关闭/失效状态与各序号的业务日、世代、capture_id、SHA-256 和引用，不含正文。内存实现同样把暂存正文放在 Book 之外，通过原文事务提交。PG 复用 owner 行锁和同一 SQL 事务，`View` 的演练修改不落库。

擦除和删一天在删除原文、写墓碑和删除待办的同一事务推进日世代并清空该日暂存；涉及该日的采集会话整体失效，避免跨午夜序列带着旧缺口继续发送。自然到期清除正式原文和当天暂存并使相关会话失效，长期账本不级联；注销按原有预算逐批清除暂存、采集元数据与全部业务数据；reset 同样覆盖。去重信息不重新夹带已擦除正文。

`apps/xinli/capture/sender` 是可复用的开发 HostApp：`Open(path, Identity)` 加进程文件锁；`Enqueue` 生成 capture_id、分配持久序号并保存原业务日；`Flush(ctx, Transport)` 按序发送。状态是 `pending → staged → accepted`（可直接接受）；网络失败保持原状态，成功回包丢失后仍重发同一 ID/序号。只有 accepted 清除本地正文；202 保存暂存回执并返回 `ErrGap`，不会确认。`resync_required` 或永久拒绝持久停止会话，重启仍停止；没有自动更换世代的入口。重新同步须由宿主显式处理旧文件并为新采集创建新会话，`Sender.Close` 只关闭本地文件，结束服务端会话另用 `capture.Client.CloseSession`。文件以 0600 临时文件写入、fsync、rename、目录 fsync 更新；保存失败停止当前对象，须关闭重开确认磁盘状态。一个文件绑定一个 owner、安装实例和采集会话，不能换账号复用。它用于 macOS/Linux 开发测试，不是移动平台生产存储。

`apps/xinli/recognition.Recognizer.Recognize(ctx, ChunkRef)` 返回 `[]capture.Segment`。确定性 fake 按分片引用配置文字、延迟、失败、说话人标签覆盖与 speaker_ref；等待函数可注入虚拟时钟，支持取消，配置和结果均复制。接口不接收音频字节，fake 不读取音频引用、不落盘，不实现供应商协议。

测试分为 Persona 内存/PG 共用的有序采集一致性套件（520 批、重复/异文、跨午夜、缺口排空、View/回滚、世代与生命周期）、两个存储的暂存物理残留检查、应用内存/PG 的真实 HTTP 与发送端重启/丢响应测试，以及本地持久状态机和识别 fake 测试。真实硬件、ASR、声纹与多设备合流仍属于后续外部接入范围。
