# M4 Memory 与 Grant

> 状态：已实现，已用真实模型验证（§10 第 6 步）· 依赖：ADR-0003、[Session 生命周期](./m2-session-lifecycle.md)（ADR-0015）、[鉴权](./auth.md) · ADR-0016
>
> 2026-11-14：ADR-0036、ADR-0039、ADR-0050 与 ADR-0037 的内核部分已整合进 [ADR-0052](../adr/0052-attested-results-and-provenance.md)，ADR-0040 已取消，ADR-0010 已被 [ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 取代。本文引用这些 ADR 之处，现行规则以 ADR-0052、ADR-0053 为准（[ADR 索引](../adr/README.md)）。

## 1. 目标

- **跨 Session 的个性化**：Agent 记住关于 EndUser 的事实与偏好（"偏好简洁的回答""同事张三的邮箱是…"），并在之后的 Session 中使用。北极星场景 A 中"结合 Memory 识别'同事'"依赖它。
- **默认隔离、授权共享**（ADR-0003）：Memory 归属产生它的 BusinessLine；其他业务线读取必须有 EndUser 的 Grant，撤销立即生效。
- **用户可控**：EndUser 能查看、搜索、删除自己的 Memory，管理 Grant，并查看"哪个业务线在什么时候用了我的哪些 Memory"。
- **可删除**：Memory 和 Session 数据一样遵循 ADR-0015，账号注销时随之删除。

不做：模型训练与微调；面向业务线的用户画像分析接口（Memory 只供 Agent 在对话中使用）。

**前提：同身份域的 EndUser ID 跨业务线统一**（2026-11-26，[ADR-0064](../adr/0064-identity-domains-for-cross-line-grants.md)）。跨业务线的 Grant 依赖"业务线 A 的 u1 与业务线 B 的 u1 是同一个人"；需要共享 Memory 的业务线必须在配置中声明相同的非空 `identity_domain`（并确认使用同一套账号体系），未声明的业务线默认独占，不能与其他业务线互相 Grant。如果各业务线的用户 ID 不统一，需要先有账号关联，这不在本设计范围内。

## 2. 数据模型

```
Memory {
  id, business_line (所有者), end_user,
  category,              // 见下表，平台固定
  content,               // 一句自然语言陈述，如"偏好简洁的回答"
  source_session, source_call,   // 由哪个 Session 的哪次调用写入
  created_at, updated_at,
  embedding              // 向量，供语义检索
}
```

| 类别 | 例子 | 说明 |
|---|---|---|
| `profile` | 称呼、职业、常驻城市 | 基本信息 |
| `preference` | 回答风格、饮食偏好 | 偏好 |
| `relationship` | "张三是同事，邮箱 …" | 人际关系 |
| `interest` | 关注的话题 | 兴趣 |
| `plan` | "下个月搬家" | 长期事项 |
| `health` | "对青霉素过敏" | 健康（需业务线开启、EndUser 单独同意，见下） |

**健康信息按业务线开启、按 EndUser 单独同意（ADR-0022 及其 2026-11-19 修订）**。过敏、饮食禁忌、慢性病与用药关系到安全，助手应当记住；但它们属于敏感个人信息，按个人信息保护的要求需要用户的单独同意，并允许撤回。业务线在配置中声明 `memory: {health: true}`，表示它有取得单独同意的流程；每个 EndUser 同意后，业务线再以服务令牌登记：

- `PUT /v1/end_users/{id}/consents/health` 登记同意，返回 `{kind, granted_at}`；`granted_at` 是平台侧可出示的同意记录，重复登记保留最初的时间。业务线未开启时 400，用户令牌 403。`DELETE` 撤回，`?purge=true` 一并删除该用户在本业务线的健康 Memory（连同被它们取代的、以别的类别记下的同一件事），返回删除条数。
- 存储同档位（[M7](./m7-kernel-primitives.md) §8）：表 `end_user_consents(business_line, end_user, kind, granted_at)`，主键 `(business_line, end_user, kind)`，分片键为 EndUser；`kind` 目前只有 `health`，为将来的敏感类别留位置。登记"先查、后复查"EndUser 删除屏障，注销账号时删除（`Service.DeleteEndUser` 的固定清理路径与 Janitor 的到期再清理）。实现在 `internal/consent`（一致性套件 `consenttest`）。
- 读缓存 5 秒（`consent.Cache`，由 `app.NewCore` 与管理接口、注销共用）：开启与撤回跨进程至多 5 秒生效，撤回在本进程立即失效缓存。`purge` 先撤回、再删除；其他进程在缓存期内仍可能写入，需要确保清除时可在 5 秒后再调用一次。
- 健康信息可用，当且仅当业务线开启且该 EndUser 已登记同意（`app.Core` 组合为 `memory.Service.Health`）。不可用时写入 `health` 类别被拒绝（工具返回原因，助手告诉用户无法记住；业务线未开启与该用户未同意给模型看的原因相同）；召回与检索也不包含该类别。只有健康信息的写入才读同意记录；预检查（`Check`）读不到同意记录时不下结论，由写入时再判断，读失败即写入失败。
- **按内容认定，不只看类别。** 类别是模型自报的：家人的血压写成 `relationship` 就绕过了同意（[工作日评审](../review/2026-10-25-xinli-workday.md) F3）。内容中出现健康信息用词（`internal/memory/health.go`：过敏、血压、用药、失眠、焦虑、怀孕等）时，不论关于谁，都只能写入 `health`：未开启的业务线一律拒绝，并告诉 Agent 不要换个类别或说法；开启的业务线要求改用 `health` 重写。用词只收几乎只指身心健康的（不收"医生""感冒""压力"），宁可漏掉也不误伤；换个说法的健康信息仍靠写入指令。这是只收紧的规则，判断在写入时做、不回溯已有的记忆；同一件事之前以别的类别记下的，改写为 `health` 时不按重复处理，新的照常写入、旧的失效（§11）。这些检查不依赖存储（`memory.Service.Check`），Worker 在审批与写入闸门之前先做（[M0](./m0-core-primitives.md) §4）：注定被拒绝的写入不让用户批准一次无用的调用，后台整理也不因此挂起。
- 跨业务线读取时，只有写入方业务线已开启、且已为该 EndUser 登记同意，健康信息才能经 Grant 被读取——即使用户授权了 `health` 类别。读取方业务线自己是否开启不影响。
- 关闭开关或撤回同意后，已有的健康信息不再被召回或检索，但默认不删除（用户可能暂时撤回、之后重新同意）；它们仍可经 `GET /v1/memories` 列出与删除，或撤回时带 `?purge=true` 一并删除。
- `-auth none` 的开发模式与评测视为业务线已开启、所有 EndUser 都已同意（`app.Options.HealthEveryone`）：没有业务线服务端来登记。

**其他敏感个人信息不存储**（金融账户、身份证件、精确位置、未成年人信息等）：写入指令中明确禁止，证件号与银行卡号由平台直接拒绝。

类别是平台固定的分类，原因有两个：一是授权可以按类别进行（§4）；二是 EndUser 在界面上能看懂"允许 B 读取你在 A 中的偏好"。

## 3. 写入与读取

### 写入：由 Agent 通过能力调用

Agent 通过进程内的能力写入和删除 Memory（能力名用单下划线：双下划线是"设备标签__能力"的分隔符）：

- `memory_save{category, content, replaces?, valid_until?}`：写入一条，可替换旧的一条（旧的标为失效、作为历史保留，§11），用于处理"搬家了"这类更新；带日期的计划写明有效期；
- `memory_forget{id}`：删除一条（只能删除本业务线的）。

Agent 能否写入由能力白名单中是否包含 `memory_save` 决定，与其他能力一致；AgentDef 的 `memory.recall` 配置召回条数。

- 写入是调用，所以天然记录在日志里（`ToolCallStarted` / `ToolResult`）：何时、因为哪句话写了什么，都可以审计。
- Memory ID 由真实 EndUser、业务线、来源 Session 与完整 `call_id` 的确定性摘要派生，并嵌 EndUser 槽位；无调用 ID 时随机生成。重试先按真实 EndUser 查旧 `mem_<call_id>`，再查新 ID，幂等命中须核对完整归属（ADR-0051 C3b）。原相似内容去重规则不变。
- **几乎相同的不再写入**：同一 EndUser 与业务线中已有一条文本相似度（字符二元组的 Dice 系数）不低于 0.85、且号码与邮箱等字母数字串完全相同的 Memory 时，`memory_save` 不再写入，返回已有那条的 ID 并告诉 Agent"没有重复写入"；带 `replaces` 时仍把被替换的那条标为失效。不限于同一类别：同一句话归到另一个类别（"不吃香菜"写成 `health`）也是重复。每日整理（[旁听业务线 xinli](./xinli-captures.md) §3）每次遇到用户说过的事都会想再记一次，重复的条目会占满上限、挤占召回。阈值只合并标点、空白与个别字的差别；"吃"与"不吃"、只差一位的号码都照常写入。合并后的事实只归属最早写入它的 Session：删除那个 Session 时它随之删除，即使之后的 Session 也提到过。
- **写入后列出同类别的已有记忆**：新写入一条（没有 `replaces`）后，结果中列出同类别中最相近的至多 5 条已有记忆，并提示"如果刚记住的这条取代了其中某条，再调用一次 `memory_save`，content 不变，`replaces` 填旧的 ID"。这次调用命中上一条的去重，只把旧的那条标为失效。Agent 常常不先查就写（"搬到上海了"与"常住北京"并存），这是它看到旧记忆的最后机会，做法同 mem0 写入前检索相似记忆、再决定新增或更新。排序用文本相似度，不调用嵌入模型。

### 写入闸门（ADR-0023）

同一来源判断也用于 `schedule` 的 create 与 cancel：只要上下文包含不是用户原话的内容就须审批（含平台定时），不采用 Memory 的依据豁免，list 不审批；见 [M7 §5.3](./m7-kernel-primitives.md#53-接口)。

记忆投毒是持久化的提示注入：网页、文件、调用结果中的指令一旦被写成 Memory，就会在之后的每个 Session 中被召回。写入闸门只收紧、不放宽：

- **触发条件**：当前模型上下文中有不是用户原话的内容——设备、沙箱、MCP 的调用结果（能力名含 `__`）、输入或回答附带的界面上下文（[多端双工通道](./m3-duplex-channel.md) §8）、Call 中语音模型的转述（派生的任务与转述的回答，[Call](./m3-call.md) §6），或可能概括了它们的压缩摘要——且要写入的内容在用户本人的输入中找不到依据。用户对 `ask_user` 提问的回答算作用户本人的输入（ADR-0025），经语音模型转述的回答（`ToolResult.from_call`）除外；Call 中用户说的话（转写）算作用户本人的输入。
- **依据的判定**：内容中的邮箱、链接、号码都原样出现在用户输入里，且内容的字符二元组至少一半出现在用户输入中（允许"我对花生过敏"→"用户对花生过敏"这样的改写）。投毒通常夹带用户从未说过的地址或号码。比较之前两侧都做 NFKC 规范化（全角字符不能绕过地址与号码的识别）；二元组收录任何文字的字母、附加符号与数字，号码识别任意十进制数字；内容中没有可比较的二元组（只有标点或单个字）时按无依据处理：识别不了的一律须审批（ADR-0023 的 2026-11-14 修订）。
- **结果**：这次 `memory_save` 需要用户批准，审批说明为"助手想记住：…（这条信息来自本次对话中读取的外部内容，不是你直接说的）"。用户亲口说的事实、没有外部内容的对话都不受影响。
- 判定只依据日志投影，不调用模型，因此是确定性的。已经请求过审批的调用一律走完审批；反过来，判定只在调用开始之前做：调用开始之后到达的插话可能使上下文变为含外部内容，已开始的调用不再回头请求审批（模拟测试发现）。
- `replaces` 只能替换同类别、仍有效的记忆：类别不同多半是 ID 用错，会把一条无关的信息标为失效。
- 发现投毒后，业务线可用 `DELETE /v1/sessions/{id}/memories`（服务令牌）删除该 Session 写入的全部记忆。

### 读取：Run 开始时自动召回，并记录为 Event

Worker 开始每个 Run 时：

1. 以本次输入为查询，从**本业务线的 Memory**加上**经 Grant 可读的其他业务线 Memory**中检索 top-k；
2. 把结果作为 `MemoryRecalled{run_id, items: [{id, business_line, category, content}]}` 追加到日志；
3. 组装上下文时，把**当前 Run** 的召回结果放进系统指令之后的一段"关于用户的已知信息"。之前 Run 的召回不进入上下文。

为什么要记录为 Event：检索结果会随时间变化，只有记在日志里，接管、回放和评测看到的上下文才一致（ADR-0004）。它同时也是**访问记录**：EndUser 可以看到哪个业务线在什么时候用了哪些 Memory。

另外提供 `memory_search{query, history?}` 供 Agent 主动查询，结果作为普通的调用结果记录；`history` 为 true 时包含失效与过期的（§11）。召回与检索都只含有效的 Memory，并注明记录日期。

召回时还写入一条**访问索引**（Memory ID、读取方业务线、Session、时间，不含内容），供 `GET /v1/memories/access` 查询。访问索引在提交召回事件**之前**写入：宁可多记一次"可能被读取"，也不漏记。访问索引随 Memory 删除。

## 4. Grant

```
Grant { id, end_user, from_business_line, to_business_line, categories[], created_at, expires_at?, revoked_at? }
```

- 授权粒度是**业务线对加类别**：EndUser 允许业务线 B 读取自己在业务线 A 中某些类别的 Memory。
- **只在同身份域内创建与生效（ADR-0064）**：`from` 与 `to` 必须都已在业务线配置中登记，且声明了相同的非空 `identity_domain`；未声明身份域的业务线默认独占，不与任何其他业务线同域（即便另一方把 `identity_domain` 显式写成前者的业务线名）。判定放在 `memory.Service`，所有入口统一经过它；不满足时创建返回 400（错误只含业务线名），`Scopes`（召回与 `memory_search`）与 `ListGrants` 实时跳过配置变更后不再同域或未登记的旧授权，撤销（`RevokeGrant`）不受影响。`-auth none` 开发模式、评测与模拟测试不做身份域检查。
- **只能由 EndUser 本人创建**：需要用户令牌，并且令牌的业务线是 A 或 B 之一。具体由哪一侧的 App 发起授权界面，属于产品层的问题。
- **撤销立即生效**：检索时实时检查 Grant，不做缓存。已经写入 B 侧日志的召回记录（`MemoryRecalled`）不会被删除，因为它们是 B 侧对话历史的一部分，随 B 侧 Session 的删除而删除。但它们不会进入之后的 Run：每个 Run 都重新召回，而之前 Run 的召回本来就不进入上下文（§3）。所以撤销最晚从下一个 Run 起生效，这一点会写进用户协议。
- 授权只能读，不能写：B 写下的 Memory 永远归属 B。

## 5. 检索与存储

- **存储**：PostgreSQL + pgvector（ADR-0007 的基线数据库，不新增组件；私有化部署只需安装扩展）。开发环境的 compose 改用带 pgvector 的镜像。
- **嵌入**：模型网关新增 `Embedder` 接口，第一个实现是 OpenAI 兼容的 `/embeddings`（私有化服务、TokenHub）；火山方舟的多模态嵌入模型 `doubao-embedding-vision-*` 走 `/embeddings/multimodal`，每段文本单独请求（2026-10-11 起默认使用，见 model-calls.md §4）。换嵌入模型需要重新计算向量，所以向量要记录模型名，检索时只比较同一模型的向量。
- **没有嵌入服务时**（开发、测试、模拟）：退化为字符二元组相似度（Dice 系数），在应用层计算。没有采用 `pg_trgm`，因为它对中文的切分依赖数据库的 locale；而每个 EndUser 每个业务线至多数百条，在应用层计算开销很小，内存与 PG 两种实现的结果也完全一致。
- **向量检索**：按所属方过滤后精确比较（`<=>`），不建向量索引；向量列不固定维度，只比较同一模型、同一维度的向量。
- **规模**：每个 EndUser 每个业务线最多保留 N 条有效的（默认 500）。超出时拒绝写入，并提示 Agent 先删除或合并。失效与过期的作为历史另有期限（§11）。

## 6. 用户可见的接口

| 接口 | 权限 | 说明 |
|---|---|---|
| `GET /v1/memories?category=&q=` | 用户：本业务线的自己的 Memory；服务令牌：本业务线（需指定 `end_user`） | 列表与搜索 |
| `GET /v1/memories?end_user=&source_session=…&source_session=…` | 仅服务令牌，本业务线与指定 EndUser | 完整列出这些 Session 写入的有效 Memory；最多 32 个来源参数 |
| `DELETE /v1/memories/{id}` | 同上 | 删除一条 |
| `GET /v1/grants`、`POST /v1/grants`、`DELETE /v1/grants/{id}` | 仅用户令牌（本人） | 查看、创建、撤销授权 |
| `GET /v1/memories/access?since=` | 用户：自己的 | 访问记录：哪个业务线在什么时候召回了哪些 Memory |

来源候选查询不要求 Session 仍存在：erasure 后 `SourceSession` 保持原值，业务线等删除完成后可以合并当天用过的所有工作 Session 查候选。重复来源不会重复返回结果，未知来源返回空集合。查询只含未被取代、未过期、未删除的 Memory，返回字段与普通列表相同，包含健康类别以便用户决定；它不通过 Grant 跨业务线读取。

为避免普通列表默认 100 条的截断漏掉候选，此查询返回所有匹配项，不接受 `limit`、非空 `q`、非空 `category` 或 `history=true`，组合使用返回 400；空来源或超过 32 个参数同样返回 400。使用 `memory.Store.ListBySource`，内存与 PostgreSQL 语义一致，数据库按 EndUser 与 BusinessLine 过滤，复用 `memories_owner` 索引。

## 7. 与删除的关系（ADR-0015）

- **账号注销**（`DELETE /v1/end_users/{id}`）：删除该 EndUser 在该业务线的全部 Memory，以及该业务线作为授出方或接收方的全部 Grant。
- **删除单个 Session**：由该 Session 写入的 Memory **一并删除**（§9）。Janitor 的清理步骤中增加一步，按 `source_session` 删除。
- **按保留期到期删除 Session**（删除原因 `retention`）：由该 Session 写入的 Memory **保留**（ADR-0037）。到期清理不是用户的删除意图；旁听 Session 只保留 7 天，每晚整理时写下的偏好与沟通习惯不应随原话一起消失（[旁听](./captures.md) §9）。Memory 的登记为 `KeepOnRetention`（`app.SessionData`），该字段亦用于擦除原因；Janitor 按删除记录的原因跳过它；委托派生的子 Session 随父 Session 一起按保留期删除时同样保留。用户可以经 `GET /v1/memories` 列出与删除它们。
- **擦除工作 Session**（`erasure`，ADR-0050）：同样保留既有 Memory，子 Session 同因删除；绝对期限 `delete_at` 到期则按 `retention` 删除。两种原因都不豁免迟到的新写入：`lifecycle.Recheck` 按新 Memory ID 撤回，不删除该 Session 的其他既有 Memory。
- **删除已清理 Session 的来源 Memory**：`DELETE /v1/sessions/{id}/memories` 在 Session 已不存在时，允许同业务线的服务令牌按删除记录授权并幂等删除；用户令牌、没有删除记录或业务线不符仍返回 404。unrestricted 开发模式须带 `business_line`；仍存在的 Session 保持原有权限。删除按 `source_session` 访问 Memory，不依赖 Session 投影或 EndUser。
- 访问记录保存在召回方的 Session 日志中，随该 Session 删除；另外有一张不含内容的访问索引（memory_id、业务线、时间），删除 Memory 时一起删除。

## 8. 测试

- 一致性套件 `memorytest`：内存与 PG 实现；类别过滤、上限、按 Session 删除、按 EndUser 删除、Grant 生效与撤销。
- **授权矩阵扩展**：新接口 × 调用方；另外有"跨业务线召回"矩阵：不同的 Grant 状态（无、有效、过期、已撤销、类别不符）× 召回结果。
- 模拟测试：召回事件参与确定性回放；删除 Session 或注销账号后，Memory 也必须满足"删干净、不复活"的不变量。
- 嵌入模型可用后，用真实模型评测召回质量（M4 评测体系）。

## 9. 决定（2026-10-09）

1. **Grant 粒度**：业务线对加类别。
2. **写入方式**：第一阶段只由 Agent 通过能力写入；后台自动提取等评测体系建立后再做。
3. **删除对话**：由该对话写入的 Memory 一并删除，符合"删掉就是删掉"的预期。按保留期到期删除时保留（2026-10-23 修订，ADR-0037）：这条规则的本意是尊重用户的删除意图，到期清理不是。

## 10. 实施顺序

1. `memory` 包：Store（内存实现与 PG 实现，pgvector 加 `pg_trgm`）、Grant 存储、一致性套件 `memorytest`；迁移 0005；compose 改用带 pgvector 的 PostgreSQL 镜像。
2. 模型网关新增 `Embedder`（OpenAI 兼容 `/embeddings`）；没有配置时使用确定性的文本相似度检索。
3. `MemoryRecalled` 事件；Worker 在 Run 开始时召回；上下文组装；能力 `memory__save` / `memory__forget` / `memory__search`；AgentDef `memory:` 配置段。
4. 接入删除：Janitor 按 Session 删除 Memory，注销账号时删除 Memory 与 Grant；模拟测试加入 Memory 的删除不变量与召回的确定性回放。
5. API：Memory 列表、搜索、删除，Grant 的创建与撤销，访问记录；扩展授权矩阵与跨业务线召回矩阵。
6. 用真实模型验证召回质量。**已验证（2026-10-09）**：嵌入模型改用 TokenHub `kinfra-text-embedding-0.6b`（1024 维，在相关与无关记忆之间的区分度优于 4b 版本），对话模型 `deepseek-v4.1-flash`。模型能主动把称呼、同事邮箱、回答偏好分别存为三个类别；新 Session 能通过向量召回；其他业务线在授权前读不到、只授权"人际关系"后只读到这一类、撤销后从下一个 Run 起读不到；访问记录中能看到读取方。

## 11. 时效：失效与有效期（已实现，2026-10-24）

> 依据 2026-10-24 的旁听复盘与 [ADR-0038](../adr/0038-superseded-memories-are-kept.md)；本节的规则优先于 §3、§5、§7 中关于替换与删除的旧描述。

### 问题

- **替换即删除。** `replaces` 删除旧条目，"以前住北京"这类历史随之消失，用户问"我去年住哪"时无从回答。被替换的也不一定真的过时：模型偶尔会用错 ID，删掉的是一条无关的信息（§3 只能限制为同类别）。
- **没有有效期。** "下个月搬家""周三出差"这类计划，过了日期仍一直被召回，模型把过时的计划当成现在的事。
- **召回看不出新旧。** 召回文本只有内容，没有记录日期，模型无法判断"喝咖啡"和"已经戒咖啡"哪条在后（`memory-conflict-recency` 依赖模型自己察觉）。

业界的做法：Graphiti 给每条事实记 `valid_at` / `invalid_at`，矛盾时只把旧事实标为失效、不删除；mem0 在 2026-04 改为只追加，读取时按时间消解冲突。两者都与本仓库"日志只追加"的取向一致。

### 设计

| 字段 | 含义 |
|---|---|
| `superseded_by`、`superseded_at` | 被哪条 Memory、在何时取代。非空即"已失效"。 |
| `valid_until` | 有效期的最后一天（当地日期），可空。过了这一天即"已过期"。 |

- **替换只标记失效。** `memory_save{replaces}` 写入新的一条，把旧的标为被它取代，不删除。替换仍只限同类别。
- **有效期由写入方给出。** `memory_save` 新增可选参数 `valid_until`（YYYY-MM-DD），用于带日期的计划与临时状态（"下周三出差"写到出差那天）。不写即长期有效。平台不推断有效期。
- **有效的才参与召回与检索。** 召回（§3）、`memory_search`、去重与"写入后列出同类别的已有记忆"都只看有效的（没有失效、没有过期）。Grant 授出的也只有有效的。
- **历史可以查。** `memory_search{query, history: true}` 同时返回失效与过期的，注明"（10-22 被 mem_x 取代）"或"（10-25 已过期）"，供回答"以前""上次"一类的问题。
- **召回带上日期。** 召回与检索的每条都注明记录日期，如 `- [profile] 用户搬到上海了（mem_x，10-22 记）`，模型据此判断新旧。这是给模型看的文本，须有评测覆盖。
- **上限只算有效的。** 每人每业务线 500 条（§5）只计有效的 Memory。
- **历史有期限。** 失效满 180 天、过期满 30 天的 Memory 被清除。清除在同一 EndUser、同一业务线写入时顺带进行（一条 SQL、一个键，ADR-0028），不新增后台任务。
- **删除照旧。** 用户要求忘记（`memory_forget`、`DELETE /v1/memories/{id}`）时删除这条，以及被它取代的整条链，"忘掉"包括它的历史。按 Session 删除、注销账号同样删除失效与过期的。按保留期删除 Session 时照旧保留（§7）。
- **用户能看到。** `GET /v1/memories` 默认只列有效的，`?history=true` 同时列出失效与过期的，带取代关系与日期。

### 存储

- **迁移。** 新增迁移：`memories` 加三列 `superseded_by`、`superseded_at`、`valid_until`（可空），不改已有迁移。
- **一致性套件。** `memorytest` 增加失效、过期、历史检索、链式删除、上限只计有效、顺带清除的用例，内存与 PG 两种实现都要通过。
- **模拟测试。** 删除不变量扩展到失效的 Memory：删除 Session 或注销账号后不得残留。

### 评测

| 用例 | 要点 |
|---|---|
| `memory-history` | 预置"常住北京"被"搬到上海了"取代：问"我搬家之前住哪"，答北京（`history: true`）。 |
| `memory-plan-expiry` | 预置一条已过期的计划（"10 月 20 日出差去广州"）：问"我最近有什么安排"，不提广州。 |
| `memory-plan-valid-until` | 用户说"下周三我去杭州出差"：写入时带 `valid_until` 为那天。 |
| 已有的 `memory-*`、`capture-memory-*` | 召回文本带日期之后不回归；`memory-conflict-recency` 预期改善。 |
