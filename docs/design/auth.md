# 鉴权：业务线签发的短期令牌

> 状态：已实现 · ADR-0014 · 取代 [M1 设计](./m1-device-nodes.md) §7 与 HTTP API "无鉴权" 的开发限度

## 1. 主体与入口

| 主体 | 是谁 | 能做什么 |
|---|---|---|
| **用户令牌**（BusinessLine, EndUser） | HostApp 中的端侧 SDK，代表一个 EndUser | 只能访问该 EndUser 在该 BusinessLine 下的 Session、Node 与 Artifact |
| **服务令牌**（BusinessLine） | 业务线服务端 | 可以访问该 BusinessLine 下所有 EndUser 的资源；创建 Session 时须指明 EndUser |
| 内部令牌 | yanshi 进程之间 | 只用于内部增量接口（已实现，ADR-0013） |

需要鉴权的入口：HTTP API 的全部 `/v1/*` 接口（包括 SSE 与工件上传下载），以及 Connection 的 WebSocket 接入（`/v1/connect`，兼容路径 `/v1/nodes/connect`；Node 与会话客户端共用，ADR-0024）。`/healthz` 不需要鉴权。

## 2. 令牌

令牌采用 JWT（RFC 7519），**由业务线用自己的私钥签发**，yanshi 只保存各业务线的公钥。

```yaml
# businesslines/demo.yaml
name: demo
identity_domain: corp     # 可选；声明相同的非空身份域后才能与其他业务线互相创建与生效 Memory Grant（ADR-0064）；留空表示独占
keys:
  - kid: demo-2026-10
    alg: EdDSA            # EdDSA | ES256 | RS256
    public_key: |
      -----BEGIN PUBLIC KEY-----
      ...
max_ttl: 1h               # 拒绝有效期更长的令牌
```

`identity_domain` 声明该业务线所属的 EndUser 身份域（[ADR-0064](../adr/0064-identity-domains-for-cross-line-grants.md)）：为空表示独占，不与任何其他业务线共享 Memory；非空时不得包含空白或控制字符（在加载配置与构造 JWT 校验器时校验）。只有在配置中声明了相同非空 `identity_domain` 的两条已登记业务线才能互相创建与生效 Memory Grant。

| 声明 | 要求 |
|---|---|
| `iss` | 业务线名，必须与 `kid` 所属的业务线一致 |
| `aud` | 部署标识（`-auth-audience`，默认 `yanshi`），防止令牌被拿到别的环境使用 |
| `sub` | EndUser；服务令牌不带 `sub` |
| `scope` | `user`（默认）或 `service` |
| `exp` / `iat` | 必填，`exp - iat ≤ max_ttl`；允许 60 秒时钟偏差 |

为什么由业务线签发：

- 业务线本来就负责认证自己的用户。由它直接签发令牌，客户端启动时不需要再向 yanshi 换取令牌，yanshi 也不必理解各业务线的登录体系。
- yanshi 只持有公钥。即使 yanshi 的配置泄露，攻击者也无法伪造令牌。
- 业务线之间天然隔离：一条业务线的私钥泄露，只影响它自己的用户。
- 符合私有化部署（ADR-0001），不依赖外部身份服务。轮换密钥时，先增加新的 `kid`，再移除旧的。

验签使用每个 `kid` 配置的固定算法，拒绝令牌头中的 `alg: none` 以及与配置不一致的算法。

## 3. 授权规则

| 操作 | 用户令牌 (bl, eu) | 服务令牌 (bl) |
|---|---|---|
| 创建 Session | bl、eu 取自令牌；请求体中的值只能为空或与令牌一致 | bl 取自令牌；eu 必须在请求体中给出 |
| Session 的读写、流、中断、审批 | 仅 `SessionCreated` 的 (bl, eu) 与令牌一致的 Session | 仅 bl 一致的 Session |
| 工件 | 由所属 Session 决定 | 同左 |
| Node 列表 | 自己的 (bl, eu) | 查询参数中指定的 eu，bl 必须一致 |
| Node 接入（Hello） | 身份取自令牌；Hello 中自报的 bl、eu 只能为空或与令牌一致 | 不允许：Node 总是代表某个 EndUser |

访问不属于自己的资源时，返回 **404**，不返回 403，避免泄露资源是否存在。

## 4. 传输与长连接

- HTTP 请求（包括 SSE）使用 `Authorization: Bearer <token>`。浏览器的 EventSource 不能设置请求头，等出现 Web 端需求时再单独设计。
- Node 接入在协议内携带令牌（`Hello.token`，契约中原有的字段），不依赖 WebSocket 升级请求头，因此不能设置请求头的 WebSocket 客户端也能接入。
- SSE 在令牌到期时先发出 `event: token_expired` 再断开。
- **长连接只在建立时校验令牌，令牌到期时由服务端主动断开。** SDK 通过 HostApp 提供的 `TokenSource` 回调取得新令牌后重连。断线重连本来就是 SDK 和 SSE 支持的正常路径（Inbox 重新投递、`Last-Event-ID` 续传）。这样，泄露的令牌最多在其有效期内可用。
- 暂不做吊销列表，靠短有效期控制风险。

## 5. 开发模式

- `serve -auth jwt -businesslines <目录>`：生产方式。
- `serve -auth none`：沿用现在的行为（信任请求中自报的身份）。**只允许监听回环地址**，监听其他地址时拒绝启动，以符合"安全默认"（章程原则 6）。
- `yanshi token -key <私钥> -bl demo -user u1`：开发工具，扮演业务线服务端签发令牌。`chat` 与 `node` 命令可直接使用 `-key` 自行签发。另有 `yanshi keygen` 生成开发用的密钥对和 `businesslines/*.yaml`。

## 6. 暂不包括

- 终端用户身份联邦（OIDC 等）：由业务线自己处理，yanshi 只看业务线签发的令牌。
- 细粒度权限：哪些 Capability 可用、是否需要审批，属于 Policy（M4）。
- 配额、限流、Grant（M4）。
- 服务之间的 mTLS：由部署层（Service Mesh）提供。

## 7. 测试

- 令牌校验单元测试（`internal/auth`）：签名错误、`alg: none`、算法与配置不符、`kid` 未知、`iss` 与 `kid` 不符、`aud` 错误、过期、有效期超过 `max_ttl`、缺少 `exp`。
- **授权矩阵测试**（`internal/httpapi/authz_test.go`）：每个 HTTP 接口 × {本人、同业务线的其他用户、其他业务线、本业务线服务令牌、其他业务线服务令牌、无令牌}，断言 2xx 或 404/401。新增接口时必须加入这张表，否则测试失败（路由表与矩阵对照）。
- Node 接入：令牌身份与 Hello 自报身份不一致时拒绝；令牌到期时断开连接。
- e2e：全部场景在开启鉴权的情况下运行；另测 Node 令牌到期后断开、取新令牌重连，以及身份不符时拒绝接入。

## 8. 错误响应

HTTP API 路由返回的 JSON 错误包含 `error`（供人阅读的现有文本）、`code`（稳定的错误类别）与可选的 `reason`（该类别下的稳定子原因）。配额错误仍可带 `quota`，已有的 `Retry-After` 响应头继续有效。

`code` 和 `reason` 使用 ASCII snake_case。发布后只增不改，客户端须容许未知值与缺省的 `reason`。客户端不得解析 `error` 文本。

| code | HTTP 状态 | 来源 |
|---|---|---|
| `invalid_argument` | 400 | `service.ErrInvalid`、`oneshot.ErrInvalid`、`outbox.ErrInvalid`、`outbox.ErrCursor` |
| `memory_rejected` | 400 | `memory.ErrRejected` |
| `unauthenticated` | 401 | 出站长轮询期间令牌到期 |
| `forbidden` | 403 | `errForbidden`、`outbox.ErrScope` |
| `not_found` | 404 | `service.ErrNotFound`、`oneshot.ErrNotFound`、`shard.ErrInvalidID`、`artifact.ErrNotFound`、`memory.ErrNotFound`、`outbox.ErrShard` |
| `conflict` | 409 | `service.ErrConflict`、`outbox.ErrFilter` |
| `memory_limit` | 409 | `memory.ErrLimit` |
| `gone` | 410 | `outbox.ErrEpoch`、`outbox.ErrExpired` |
| `too_large` | 413 | `artifact.ErrTooLarge` |
| `moderation_rejected` | 422 | `moderation.ErrRejected` |
| `scan_budget_exceeded` | 422 | `service.ErrScanBudgetExceeded`，输入状态查询超出扫描预算，不可重试 |
| `rate_limited` | 429 | `model.ErrBusy` |
| `quota_exceeded` | 429 | `usage.ErrExceeded` |
| `internal` | 500 | 未归类的路由错误 |
| `not_implemented` | 501 | `speech.ErrNotConfigured` 或工件、索引、删除进度、Memory、用量接口未装配 |
| `provider_error` | 502 | `speech.ErrProvider`、`oneshot.ErrProvider`、`oneshot.ErrSchema` |
| `moderation_unavailable` | 503 | `moderation.ErrUnavailable` |
| `oneshot_unavailable` | 503 | `oneshot.ErrInternal` |
| `outbox_unavailable` | 503 | 出站流未装配或读取失败 |
| `overloaded` | 503 | `service.ErrOverloaded` |
| `shard_unavailable` | 503 | `shard.ErrUnavailable`，或分片模式下原为 500、504 的路由错误 |
| `timeout` | 504 | `context.DeadlineExceeded` |

重答（`POST /v1/sessions/{id}/inputs`）因资格检查被拒时，`code` 为 `conflict`，`reason` 取自 `session.State.RedoBlocker`。

| reason | 拒绝条件 |
|---|---|
| `session_closed` | Session 已关闭 |
| `not_last_run` | 目标已非最后一个 Run，或 Session 中没有 Run |
| `not_completed` | 最后一个 Run 的状态不是 completed |
| `delegated_session` | Session 由委托派生 |
| `platform_input` | 输入来自平台，或缺少原请求记录 |
| `card_action_input` | 输入包含 CardAction |
| `compacted` | 输入之后发生过压缩 |
| `steered` | Run 已接收插话 |
| `approval_requested` | Run 请求过审批 |
| `ask_user` | Run 调用过 `ask_user` |
| `delegation` | Run 发起过委托 |
| `background_call` | Run 有后台调用 |
| `not_read_only` | 除 `show_card` 外的调用缺少只读证据 |

出站流的 `conflict` 带 `outbox_filter_changed`，`gone` 带 `outbox_epoch_changed` 或 `outbox_cursor_expired`。恢复规则见[出站流](./m7-kernel-primitives.md#b4-的-http-与存储契约)。

令牌中间件直接返回的 401、路由未匹配时的响应、已建立的 SSE 流与外部循环的 OpenAI 兼容模型端点各自沿用现有契约。Connection 使用 `ClientError`，见[多端双工通道](./m3-duplex-channel.md#5-请求)。

## 9. 输入状态

`GET /v1/sessions/{id}/inputs?input_id=…` 与 `GET /v1/sessions/{id}/events` 接受相同的令牌。客户端提供写入日志的 `input_id`，长度为 1 至 128 字节。缺少 ID 或长度无效返回 `400 invalid_argument`。Session 不存在、归属不符或已标记删除均返回 `404 not_found`，关闭的 Session 仍可查询。

查询覆盖至本次投影的日志位置 `as_of_seq`。未命中返回 `200 {"found":false,"as_of_seq":"…"}`。命中返回 200，含 `found: true`、`as_of_seq`、`input_id`、`run_id`、`event_id`、`accepted_seq` 及 `acceptance_kind`。序号与 Events 的 `seq` 一样用十进制字符串。接受类型为 `requested`（含重答）、`steered` 或 `answered`（另有 `answered_call_id`）。同一 ID 多次公开接受时返回 `409 conflict`，`reason` 为 `input_accepted_more_than_once`。首版仅支持按 `input_id` 查询。

命中同时返回 `run_status`（`queued`、`running`、`suspended`、`completed`、`failed`、`interrupted`）、`attempt_count` 与 `takeover_count`。后者只计 Run 在 `running` 时开始的 Attempt。`calls` 对整个 Run 按 `call_id` 统计至少一次以 `read_only=false` 开始的调用，旧记录缺字段也按 false 解释：

| 字段 | 含义 |
|---|---|
| `writes_without_result` | 到查询上界仍无 `ToolResult` 的调用数 |
| `writes_with_error` | 结果带 `is_error=true` 的调用数，含内核合成的错误结果 |
| `writes_backgrounded` | 结果带 `backgrounded` 的调用数，后台占位也是 `ToolResult` |
| `resumes_with_pending_writes` | 从挂起恢复时仍有任一上述调用缺结果的恢复次数 |

`steered` 另外返回 `since_acceptance`，其中 `calls` 仅基于此次接受之后开始的调用和此后可见的挂起、恢复与结果。`attempts_without_prior_suspend` 统计该范围中 Attempt 编号大于 1、且自上次 Attempt 或接受位置以来未见 `RunSuspended` 的开始次数。

平台派生的接受事件视为未命中。定时 ID 按 `capname.IsScheduleInput` 判定，委托 ID 按当前 Session 的父调用和 `session.DelegatedInputID` 匹配，卡片动作按 `card.InputID` 或回答的 `ToolResult.card_action` 识别，Call 派生输入按事件的 `from_call` 识别。后台结果投递没有 `input_id`。

超过扫描预算时返回不可重试的 `422 scan_budget_exceeded`，省略 `Retry-After`。调用方改用 Events 接口分页读取并自行归约，见 [ADR-0066](../adr/0066-input-status-by-server-side-scan.md)。`as_of_seq` 之后仍可接受同一输入。调用方依据自身的业务证据决定如何处理提交结果未知的情形。
