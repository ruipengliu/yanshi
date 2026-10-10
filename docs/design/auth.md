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
keys:
  - kid: demo-2026-10
    alg: EdDSA            # EdDSA | ES256 | RS256
    public_key: |
      -----BEGIN PUBLIC KEY-----
      ...
max_ttl: 1h               # 拒绝有效期更长的令牌
```

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
