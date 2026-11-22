# 建会话幂等

> 状态：设计已定稿（2026-11-22，Q1–Q4 按提议定），待实现· 立项：[架构评估评审](../review/2026-11-22-architecture-evaluation-review.md) §5 A1 · 依赖：[Session 生命周期与数据删除](./m2-session-lifecycle.md)、[多端双工通道](./m3-duplex-channel.md)、[端侧 SDK](./m3-client-sdks.md) · 相关 ADR：0015、0028、0043、0051 · 实施前写 ADR-0059

## 1. 问题

创建 Session 是内核唯一不能安全重试的写操作：

- `POST /v1/sessions`（`internal/httpapi/httpapi.go` 的 `create`）与 Connection 的 `CreateSession`（`node.proto`，经 `internal/channel` 的同一个 `service.Create`）都不接受幂等键，每次调用都用 `ids.Generator.Session` 生成新 ID。
- 现行文档把它列为例外：“重试创建 Session 会再创建一个”（[多端双工通道](./m3-duplex-channel.md) §5、[端侧 SDK](./m3-client-sdks.md) §1）。其余写操作或以 `input_id` 去重，或重复执行无副作用。
- 回包丢失时，调用方既不知道 Session 是否已创建，也无法证明它不存在。xinli 为此在编排器中维护“Session 创建意图”与空孤儿扫描（`apps/xinli/orchestrator/orphans.go`）：没有回包的意图只能按 EndUser 列出 Session、以 AgentDef 名称与创建时间猜测候选，并且“没有可证明的最终结束时刻”，意图须一直保留（`orphans.go` 的 `cleanCreation`）。

ADR-0043 的判据：这是内核既有职责的完善，不是业务能力进入内核。跨业务适用性在于任何经不可靠网络调用内核的端或服务端都会遇到；现有接口不足见上；业务侧只能启发式地发现孤儿，做不到“恰好创建一次”。

## 2. 目标与非目标

目标：

1. 同一幂等键的重复创建返回同一个 Session，并发的重复创建也只创建一个。
2. HTTP 与 Connection 语义一致；Go、网页、移动端 SDK 断线后自动以同一键重试，取消上述例外。
3. 与删除正确交错：删除之后以同一键重试不会复活 Session（ADR-0015）。
4. 不新增存放 Session 数据或个人数据的地方，不改 Event 结构与投影。

非目标：

- 按外部键查询而不创建（`GET`）、业务标签与按标签列出：没有业务线提出，需要时另行设计。
- 输入去重窗口（评审 A2）：另立。
- 业务侧“绑定竞争失败”产生的空孤儿：创建本身成功、但业务决定不用它，仍由业务线清理。

## 3. 决定：由幂等键确定性派生 Session ID

```
token = hex( HMAC-SHA256( secret, "yanshi.session-key.v1" ‖ 0x00 ‖ business_line ‖ 0x00 ‖ end_user ‖ 0x00 ‖ key )[:16] )
id    = ids.SessionID(token, end_user)
```

带键的创建走 `service.Create` 已有的“指定 ID”路径（委托派生子 Session 用的就是它，`delegation.ChildSessionID`）：日志上 `(session_id, seq)` 的唯一约束让第一条 `SessionCreated` 成为唯一的胜者，后到的得到 `eventlog.ErrConflict`，读回已有 Session 并比对参数。

这样做的结果：

- **不需要新表。** 幂等的事实就是日志本身；删除记录天然按派生 ID 生效。没有新的 Session 数据存放处，不必在 `app.SessionData` 登记，也没有新的一致性套件。
- **槽位规则不变。** `ids.SessionID` 由 token 决定自身槽位、由 EndUser 决定 EndUser 槽位，`service.Create` 现有的槽位校验照常成立（ADR-0051）。
- **业务线之间隔离。** 业务线与 EndUser 都参与派生，同一个键在不同业务线、不同 EndUser 下得到不同的 ID；读回后仍核对 `SessionCreated` 的业务线与 EndUser（ADR-0051：槽位只供定位，不证明归属）。
- **键不落盘。** 键不写入日志、索引或任何存储，日志与指标照常只记 Session ID。

### 3.1 为什么用 HMAC 而不是普通哈希

Session ID 出现在日志、指标、URL 与出站流信封中。键可能是业务实体的编号（订单号、日期加任务号），取值空间小；若用不带密钥的哈希，拿到 Session ID 与 EndUser 的人可以逐个猜键来确认“某用户有某订单的会话”。加密钥之后，没有密钥无法从键推出 ID，也无法反向确认。

委托派生用不带密钥的 SHA-256，输入是父 Session ID 与 call_id，都是内核生成的随机 ID，没有这个问题，保持不变。

### 3.2 密钥

- 配置：`YANSHI_SESSION_KEY_SECRET`（至少 32 字节；也可用 `-session-key-secret-file` 从文件读取），经 `app.Options.SessionKeySecret` 进入 `NewCore`，再交给 `service.Service`。只有调用 `service.Create` 的角色（gateway；单进程模式即全部）需要它；同一单元内所有这类进程必须相同。
- 未配置时：带键的请求返回 `invalid`（`idempotency keys are not configured`），不退化为不幂等的创建。单进程开发模式使用固定的开发密钥并在启动时提示，与 S3 的开发凭据同一做法；模拟测试、评测与 e2e 经 `app.Options` 取固定值。
- 部署：K8s overlay 从 Secret 注入；`docs/launch-checklist.md` 增加一项“生产密钥已配置，且不是开发值”。
- 轮换：换密钥后旧键派生出新 ID，跨越轮换的重试会再创建一个 Session。因此不做例行轮换；泄露时轮换，接受这一代价（见 Q2）。

## 4. 契约

### 4.1 HTTP

`POST /v1/sessions` 的请求体增加可选的 `idempotency_key`：

- 格式：1～128 个字符，取自 `[A-Za-z0-9._:-]`；不合法返回 400。
- 建议每个“创建意图”生成一个随机值（UUID）并在调用之前持久保存；以业务实体编号作键也可以（§3.1），但同一实体需要第二个 Session 时（例如擦除后重建，见 §5.3）必须换键。
- 用户令牌与服务令牌都可以带键；键的作用域是令牌认定的业务线与 EndUser（`claim`），与请求体中的值一致。

响应：

| 情形 | 状态码 | 响应体 |
|---|---|---|
| 首次创建 | 201 | `{"session_id": ...}`（与现在相同） |
| 重放：已有 Session，参数一致 | 200 | 同上，同一个 ID |
| 同一键、参数不一致 | 409 | `conflict` |
| 该键对应的 Session 已删除，或 EndUser 正在注销 | 404 | `not_found` |

不带键的请求行为不变。

### 4.2 Connection

`CreateSession` 增加字段（向后兼容，`buf breaking` 通过）：

```proto
message CreateSession {
  string agent = 1;
  string agent_version = 2;
  // 非空时创建幂等：同一 EndUser 以同一键重复创建，返回同一个 Session（docs/design/session-create-idempotency.md）。
  string idempotency_key = 3;
}
```

错误映射与 HTTP 相同（`conflict`、`not_found`、`invalid`）。Connection 不区分首次与重放，`ClientResponse` 不增加字段。

### 4.3 参数一致的判定

同一个键绑定首次请求的参数。重放时与已记录的 `SessionCreated` 比较：

| 字段 | 比较方式 |
|---|---|
| `business_line`、`end_user` | 必须相等（归属核对，不相等按 409） |
| `agent` 名称 | 必须相等 |
| `agent_version` | 请求指定了版本时必须相等；未指定时不比较。发布配置可能在两次尝试之间改变分流，以首次的结果为准 |
| `delete_at` | 都为空，或相等（秒精度） |
| `time_zone` | 相等 |

只写了索引、日志尚未写入的半成品（首次尝试在 `Index.Put` 之后失败）：`Index.Put` 是“不存在才插入”（`pglifecycle.go` 的 `ON CONFLICT DO NOTHING`），若后续尝试换了参数，索引与日志会不一致。所以没有日志时还要读索引行，`agent` 与 `delete_at` 不一致同样返回 409；用原参数重试即可完成创建。

## 5. 服务端流程

`service.CreateRequest` 增加 `IdempotencyKey`，与 `SessionID`（内部指定 ID）互斥，同时给出返回 `invalid`。带键时：

1. 校验格式与已有字段（业务线、EndUser、`delete_at`、时区），派生 ID。
2. **先查删除记录**：`Deletions.Get(id)` 存在即返回 `not_found`。这只是快速路径，避免对已消耗的键反复“写入—复查—撤回”，正确性仍由第 6 步的复查保证。
3. **再查日志**：已有 `SessionCreated` 时按 §4.3 比较，一致即返回该 ID，不解析 AgentDef。所以首次创建之后即使该版本被撤回，重放仍然成功。
4. 没有日志时读索引行，按 §4.3 检查半成品；然后照现有顺序解析 AgentDef、写索引、写日志（先索引后日志的理由不变）。
5. 写日志遇到 `ErrConflict`（并发的同键创建）时读回并按 §4.3 比较，与委托的冲突处理合为同一段代码。
6. 所有返回路径照常经 `defer` 做 EndUser 屏障与 Session 删除记录的复查（`lifecycle.RecheckEndUser`、`lifecycle.Recheck`）。

### 5.1 复查失败时的撤回

现有规则：无法读到屏障时撤回本次新建的 Session，指定 ID 的幂等重试不撤回已有的。带键时同样区分：

- 本次尝试写入了 `SessionCreated`：读不到屏障就撤回。安全优先：被撤回的 Session 可能属于正在注销的 EndUser。代价是这个键随之被消耗，本次返回 `unavailable`，之后的重试得到 `not_found`，业务线须换键（见 Q3）。
- 本次是重放：不撤回，返回错误，由调用方重试。

### 5.2 并发

两次同键请求并发时，两者都可能写索引（后者无操作），都尝试在 seq 1 追加 `SessionCreated`，只有一方成功，另一方走第 5 步读回同一个 ID。内存与 PG 的日志实现都已满足这一点：首事件已存在时再以 expected = 0 追加得到 `ErrConflict`，且不改动已有事件（`eventlogtest` 的 `ConflictLeavesEventsUntouched`）。

### 5.3 与删除的关系

- Session 删除记录永久保留、不含 EndUser（[生命周期](./m2-session-lifecycle.md) §3.2），所以键在 Session 删除后**永久消耗**：无论删除原因是用户删除、保留期、`delete_at` 到期、擦除还是注销，以同一键重试都得到 404。
- 擦除后重建（生命周期 §5.1）是一个新的 Session，业务线用新键创建，并照旧传回原来的期限。
- EndUser 注销后屏障在宽限期后移除，同一 EndUser ID 可以重新使用；旧键对应的 ID 仍有删除记录，依旧 404。按建议每个意图用随机键时，这不会被察觉。

## 6. SDK

- Go（`sdk/nodesdk`）：`CreateSession` 为每次调用生成键，`ErrDisconnected` 时重连后以同一键自动重试，直到得到结果或 ctx 结束，与 `Submit` 相同。新增 `CreateSessionWithKey(ctx, agent, version, key)`，供需要跨进程重启继续重试的 HostApp 自己保存键。
- 网页（`sdk/web`）：`createSession(agent, version, opts)` 同样自动生成键并在断线后重试；`opts.idempotencyKey` 由调用方给出。
- 移动端经 gomobile 绑定 `nodesdk`，随之获得；改动后运行 `make mobile`。
- 三个 SDK 的语义一致；[端侧 SDK](./m3-client-sdks.md) 与[多端双工通道](./m3-duplex-channel.md)中“创建 Session 例外”的说明随实现删除。

## 7. 对业务线的影响

xinli 的“Session 创建意图”在调用内核之前落盘，它的 ID 可以直接作为键（`apps/xinli/task/creation.go`）。内核实现后：

- 回包丢失或进程退出后，以同一键重试即可得到 Session ID，`RecordCreated` 总能完成；
- `cleanCreation` 中按 EndUser 列出、按名称与时间猜测候选的分支，以及“没有回包的意图必须永久保留”的限制，可以去掉；
- 绑定竞争失败产生的空孤儿仍由编排器清理（`apps/xinli/CONTEXT.md`“空孤儿”）。

这些改动属于 xinli，在内核发布后单独进行，不在本设计的范围内。

## 8. 验证

- `service`：重放返回同一 ID；参数不一致 409；版本撤回后重放成功；删除后 404；不同业务线、不同 EndUser 同键得到不同 ID；`IdempotencyKey` 与 `SessionID` 互斥；索引半成品；密钥未配置时拒绝。
- `httpapi` 与 `channel`：状态码与错误映射；用户令牌、服务令牌的作用域。
- 模拟测试（`internal/sim`）：增加“创建回包丢失后以同键重试”的操作，以及创建与删除、注销交错的路径；不变量为“每个 (业务线, EndUser, 键) 至多有一个 Session 写入过 `SessionCreated`”，以及删除不变量中“删除后不复活”覆盖派生 ID。回包丢失若是新的故障类型，同时加入覆盖统计。
- SDK：Go 的断线重试测试；网页 SDK 与 Go 服务端的互通测试（`sdk/web/test/interop.ts`）。
- 运行 `make check`、`make sim`、`make mobile`。不改给模型看的文本，不需要评测。

## 9. 备选方案

| 方案 | 不选的原因 |
|---|---|
| 映射表 `(business_line, end_user, key) → session_id` | 能顺带支持按键查询与键的过期，但它是新的 Session 数据存放处：要按 EndUser 分片登记 `pg.Tables`、在 `app.SessionData` 登记、写入入口先写后查、新增一致性套件并接入模拟测试的删除不变量；键本身可能是个人数据，须随 Session 删除。换来的保证与派生 ID 相同 |
| 不带密钥的哈希（与委托相同） | 不需要配置密钥，但 Session ID 可被用来确认猜测的键（§3.1）。只有在规定“键必须是随机值”时才安全，而内核无法检查这一点 |
| 客户端直接指定 Session ID | 把 ID 格式与槽位规则暴露给客户端，与 ADR-0051 的“持久 ID 由 `ids.Generator` 生成以携带槽位”相反；客户端还可以抢占 ID |
| 维持现状，由业务侧处理 | 回包丢失后业务侧无法证明 Session 不存在（xinli `orphans.go` 的注释），每条业务线都要重做一遍启发式清理 |
| 键带有效期（如 24 小时） | 派生 ID 的幂等随日志与删除记录永久成立，没有需要过期的状态；有效期只在映射表方案中才有意义 |

## 10. 已定的问题（2026-11-22）

- **Q1 重放的状态码：200。** 首次为 201，响应体相同。另一种是两者都返回 201，调用方无从区分；区分有助于排障，且不影响只看 2xx 的客户端。
- **Q2 密钥轮换：第一期不支持双密钥。** 将来若要平滑轮换，可同时配置“上一个密钥”：重放时先按旧密钥派生的 ID 查日志，命中即返回；新建一律用新密钥。代价是每次带键的创建多一次读取。
- **Q3 复查失败消耗键：接受。** §5.1 选择安全优先。发生频率是“日志写入成功后紧接着读屏障失败”，预计很低；若线上观察到，改为把撤回原因记为可识别的类别，并允许同键在该类别下重新创建（需要修订删除记录的语义，届时另议）。
- **Q4 按键查询：不做。** 带键的创建本身就是“取得或创建”。只想查询、不想创建的需求出现时再加 `GET`。

## 11. 实施步骤

1. 写 ADR-0059（派生 ID、HMAC 密钥、删除后键永久消耗），更新 ADR 索引；在 AGENTS.md 的 Connection 一行补充“创建 Session 以幂等键去重”。
2. 派生函数放在 `internal/ids`（`KeyedSessionID(secret, businessLine, endUser, key)`），与 `SessionID` 相邻并共用 token 校验。
3. `service.Create`、`app.Options`、`serve` 的配置与部署 overlay、`launch-checklist.md`。
4. `node.proto` 增加字段并 `make gen`；`httpapi`、`channel`。
5. 三个 SDK；`make mobile`。
6. 测试与模拟测试（§8）。
7. 文档：本文状态改为已实现；更新[多端双工通道](./m3-duplex-channel.md)、[端侧 SDK](./m3-client-sdks.md)、[生命周期](./m2-session-lifecycle.md) §5.1（擦除后重建须换键）、[部署](./deployment.md)（密钥）；评审 A1 标为已实现。
