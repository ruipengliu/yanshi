# M4 Memory 与 Grant

> 状态：已实现，已用真实模型验证（§10 第 6 步）· 依赖：ADR-0003、[Session 生命周期](./m2-session-lifecycle.md)（ADR-0015）、[鉴权](./auth.md) · ADR-0016

## 1. 目标

- **跨 Session 的个性化**：Agent 记住关于 EndUser 的事实与偏好（"偏好简洁的回答""同事张三的邮箱是…"），并在之后的 Session 中使用。北极星场景 A 中"结合 Memory 识别'同事'"依赖它。
- **默认隔离、授权共享**（ADR-0003）：Memory 归属产生它的 BusinessLine；其他业务线读取必须有 EndUser 的 Grant，撤销立即生效。
- **用户可控**：EndUser 能查看、搜索、删除自己的 Memory，管理 Grant，并查看"哪个业务线在什么时候用了我的哪些 Memory"。
- **可删除**：Memory 和 Session 数据一样遵循 ADR-0015，账号注销时随之删除。

不做：模型训练与微调；面向业务线的用户画像分析接口（Memory 只供 Agent 在对话中使用）。

**前提：EndUser ID 跨业务线统一**（公司统一账号）。跨业务线的 Grant 依赖"业务线 A 的 u1 与业务线 B 的 u1 是同一个人"；如果各业务线的用户 ID 不统一，需要先有账号关联，这不在本设计范围内。

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

**不存储敏感个人信息**（健康、金融账户、身份证件、精确位置、未成年人信息等）。按个人信息保护的要求，这些需要单独同意。第一阶段平台直接拒绝写入，写入指令中会明确说明这一点。以后如果需要，再连同单独同意的流程一起设计。

类别是平台固定的分类，原因有两个：一是授权可以按类别进行（§4）；二是 EndUser 在界面上能看懂"允许 B 读取你在 A 中的偏好"。

## 3. 写入与读取

### 写入：由 Agent 通过能力调用

Agent 通过进程内的能力写入和删除 Memory（能力名用单下划线：双下划线是"设备标签__能力"的分隔符）：

- `memory_save{category, content, replaces?}`：写入一条，可替换旧的一条，用于处理"搬家了"这类更新；
- `memory_forget{id}`：删除一条（只能删除本业务线的）。

Agent 能否写入由能力白名单中是否包含 `memory_save` 决定，与其他能力一致；AgentDef 的 `memory.recall` 配置召回条数。

- 写入是调用，所以天然记录在日志里（`ToolCallStarted` / `ToolResult`）：何时、因为哪句话写了什么，都可以审计。
- Memory ID 由 `call_id` 派生，因此重复执行是幂等的，不需要额外的去重。

### 读取：Run 开始时自动召回，并记录为 Event

Worker 开始每个 Run 时：

1. 以本次输入为查询，从**本业务线的 Memory**加上**经 Grant 可读的其他业务线 Memory**中检索 top-k；
2. 把结果作为 `MemoryRecalled{run_id, items: [{id, business_line, category, content}]}` 追加到日志；
3. 组装上下文时，把**当前 Run** 的召回结果放进系统指令之后的一段"关于用户的已知信息"。之前 Run 的召回不进入上下文。

为什么要记录为 Event：检索结果会随时间变化，只有记在日志里，接管、回放和评测看到的上下文才一致（ADR-0004）。它同时也是**访问记录**：EndUser 可以看到哪个业务线在什么时候用了哪些 Memory。

另外提供 `memory_search{query}` 供 Agent 主动查询，结果作为普通的调用结果记录。

召回时还写入一条**访问索引**（Memory ID、读取方业务线、Session、时间，不含内容），供 `GET /v1/memories/access` 查询。访问索引在提交召回事件**之前**写入：宁可多记一次"可能被读取"，也不漏记。访问索引随 Memory 删除。

## 4. Grant

```
Grant { id, end_user, from_business_line, to_business_line, categories[], created_at, expires_at?, revoked_at? }
```

- 授权粒度是**业务线对加类别**：EndUser 允许业务线 B 读取自己在业务线 A 中某些类别的 Memory。
- **只能由 EndUser 本人创建**：需要用户令牌，并且令牌的业务线是 A 或 B 之一。具体由哪一侧的 App 发起授权界面，属于产品层的问题。
- **撤销立即生效**：检索时实时检查 Grant，不做缓存。已经写入 B 侧日志的召回记录（`MemoryRecalled`）不会被删除，因为它们是 B 侧对话历史的一部分，随 B 侧 Session 的删除而删除。但它们不会进入之后的 Run：每个 Run 都重新召回，而之前 Run 的召回本来就不进入上下文（§3）。所以撤销最晚从下一个 Run 起生效，这一点会写进用户协议。
- 授权只能读，不能写：B 写下的 Memory 永远归属 B。

## 5. 检索与存储

- **存储**：PostgreSQL + pgvector（ADR-0007 的基线数据库，不新增组件；私有化部署只需安装扩展）。开发环境的 compose 改用带 pgvector 的镜像。
- **嵌入**：模型网关新增 `Embedder` 接口，第一个实现是 OpenAI 兼容的 `/embeddings`，对应火山方舟的 `doubao-embedding-vision` 和私有化服务。换嵌入模型需要重新计算向量，所以向量要记录模型名，检索时只比较同一模型的向量。
- **没有嵌入服务时**（开发、测试、模拟）：退化为字符二元组相似度（Dice 系数），在应用层计算。没有采用 `pg_trgm`，因为它对中文的切分依赖数据库的 locale；而每个 EndUser 每个业务线至多数百条，在应用层计算开销很小，内存与 PG 两种实现的结果也完全一致。
- **向量检索**：按所属方过滤后精确比较（`<=>`），不建向量索引；向量列不固定维度，只比较同一模型、同一维度的向量。
- **规模**：每个 EndUser 每个业务线最多保留 N 条（默认 500）。超出时拒绝写入，并提示 Agent 先删除或合并。

## 6. 用户可见的接口

| 接口 | 权限 | 说明 |
|---|---|---|
| `GET /v1/memories?category=&q=` | 用户：本业务线的自己的 Memory；服务令牌：本业务线（需指定 `end_user`） | 列表与搜索 |
| `DELETE /v1/memories/{id}` | 同上 | 删除一条 |
| `GET /v1/grants`、`POST /v1/grants`、`DELETE /v1/grants/{id}` | 仅用户令牌（本人） | 查看、创建、撤销授权 |
| `GET /v1/memories/access?since=` | 用户：自己的 | 访问记录：哪个业务线在什么时候召回了哪些 Memory |

## 7. 与删除的关系（ADR-0015）

- **账号注销**（`DELETE /v1/end_users/{id}`）：删除该 EndUser 在该业务线的全部 Memory，以及该业务线作为授出方或接收方的全部 Grant。
- **删除单个 Session**：由该 Session 写入的 Memory **一并删除**（§9）。Janitor 的清理步骤中增加一步，按 `source_session` 删除。
- 访问记录保存在召回方的 Session 日志中，随该 Session 删除；另外有一张不含内容的访问索引（memory_id、业务线、时间），删除 Memory 时一起删除。

## 8. 测试

- 一致性套件 `memorytest`：内存与 PG 实现；类别过滤、上限、按 Session 删除、按 EndUser 删除、Grant 生效与撤销。
- **授权矩阵扩展**：新接口 × 调用方；另外有"跨业务线召回"矩阵：不同的 Grant 状态（无、有效、过期、已撤销、类别不符）× 召回结果。
- 模拟测试：召回事件参与确定性回放；删除 Session 或注销账号后，Memory 也必须满足"删干净、不复活"的不变量。
- 嵌入模型可用后，用真实模型评测召回质量（M4 评测体系）。

## 9. 决定（2026-10-09）

1. **Grant 粒度**：业务线对加类别。
2. **写入方式**：第一阶段只由 Agent 通过能力写入；后台自动提取等评测体系建立后再做。
3. **删除对话**：由该对话写入的 Memory 一并删除，符合"删掉就是删掉"的预期。

## 10. 实施顺序

1. `memory` 包：Store（内存实现与 PG 实现，pgvector 加 `pg_trgm`）、Grant 存储、一致性套件 `memorytest`；迁移 0005；compose 改用带 pgvector 的 PostgreSQL 镜像。
2. 模型网关新增 `Embedder`（OpenAI 兼容 `/embeddings`）；没有配置时使用确定性的文本相似度检索。
3. `MemoryRecalled` 事件；Worker 在 Run 开始时召回；上下文组装；能力 `memory__save` / `memory__forget` / `memory__search`；AgentDef `memory:` 配置段。
4. 接入删除：Janitor 按 Session 删除 Memory，注销账号时删除 Memory 与 Grant；模拟测试加入 Memory 的删除不变量与召回的确定性回放。
5. API：Memory 列表、搜索、删除，Grant 的创建与撤销，访问记录；扩展授权矩阵与跨业务线召回矩阵。
6. 用真实模型验证召回质量。**已验证（2026-10-09）**：嵌入模型改用 TokenHub `kinfra-text-embedding-0.6b`（1024 维，在相关与无关记忆之间的区分度优于 4b 版本），对话模型 `deepseek-v4.1-flash`。模型能主动把称呼、同事邮箱、回答偏好分别存为三个类别；新 Session 能通过向量召回；其他业务线在授权前读不到、只授权"人际关系"后只读到这一类、撤销后从下一个 Run 起读不到；访问记录中能看到读取方。
