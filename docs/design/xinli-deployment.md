# xinli 的生产入口与服务凭据（P4-A11）

实现依据：[Persona 架构](./persona-architecture.md) §8、§10、Q103、Q119；[鉴权](./auth.md)；[MCP 登记](./m5-mcp.md)；[进程角色](./deployment.md)。账号、登录态、权益与逐库注销属于 A12，产品 BFF 属于 B1。

## 子命令与角色

从 `apps/xinli` 构建 `./cmd/xinli`，或使用仓库的 `make build`。三个子命令共用存储配置与装配：

```sh
xinli migrate
xinli serve -business-lines xinli
xinli dev -business-lines demo -kernel-url http://127.0.0.1:8080
```

`migrate` 只迁移 Persona 与应用两个数据库，二者必须已经存在；内核自行运行 `yanshi migrate`，xinli 不连接或迁移内核库。Persona 成功而应用失败时，重新运行同一命令即可；不宣称跨库原子提交。迁移 DSN 必须直连 PostgreSQL，不经事务连接池。`serve` 默认且只允许 PG，启动与 `/readyz` 只读迁移账本，不执行 DDL；发布先迁移，再启动服务。

`-roles persona,orchestrator,bff` 默认全部开启，可以任意非空组合：

| 角色 | 启动内容 |
|---|---|
| persona | `/mcp`，保留后续 Persona 查询 API 的路由装配位置 |
| orchestrator | 持久任务执行、删除待办恢复、自然到期、Persona 注销批量清理、自动调度；任务扫描包含空孤儿清理 |
| bff | `mountBFF` 挂载点；A11 没有产品接口，也没有假成功路由 |

只有 Persona 或 BFF 角色才监听 `-addr`。每个生产进程独立监听 `-health-addr`，提供 `/healthz` 与 `/readyz`；只有编排角色的进程无需监听 MCP。就绪检查覆盖本进程使用的 Persona 库，以及编排角色使用的应用库，时限两秒；数据库不可达或缺少迁移返回 503。内核短暂不可用不阻止本地就绪，任务通过既有持久退避恢复。

分开运行时各进程使用相同的 Persona 库，所有编排副本使用相同的应用库；事实与任务的持久扫描恢复工作，不依赖进程内通知。生产不挂载 `/v1/fixture/*`、`/v1/dev/*` 或开发页面和静态资源。BFF 角色目前也使用共享 Persona 存储，就绪不能证明未来账号或产品 API 已实现。

```sh
xinli serve -roles persona -addr :8081 -health-addr :9091
xinli serve -roles orchestrator -health-addr :9092
xinli serve -roles bff -addr :8082 -health-addr :9093
```

stdout 的首行 JSON 是进程就绪协议。生产含 `health_url`、`roles`，监听产品端口时还有 `api_url`，启用 Persona 时还有 `mcp_url` 和 `registrations`；开发保持 `mcp_url`、`fixture_url`、`page_url`、`registrations` 四字段。日志只写 stderr，错误不回显 DSN、凭据或下游正文。

## 参数与环境变量

参数优先于环境变量。环境变量存的是实际值；名称以 `_ENV` 结尾的配置存的是另一个环境变量的名字。

| 参数 | 环境变量 | 默认或限制 |
|---|---|---|
| `-persona-dsn` | `YANSHI_PERSONA_PG_DSN` | 独立 Persona 库，生产必填 |
| `-app-dsn` | `YANSHI_XINLI_PG_DSN` | 独立应用库，编排与迁移必填 |
| `-business-lines` | `XINLI_BUSINESS_LINES` | 必填，逗号分隔 |
| `-roles` | `XINLI_ROLES` | `persona,orchestrator,bff`；dev 按 kernel-url 决定编排 |
| `-addr` | `XINLI_ADDR` | serve `:8081`，dev `127.0.0.1:0` |
| `-health-addr` | `XINLI_HEALTH_ADDR` | serve `:9091`，dev 不另开探针端口 |
| `-kernel-url` | `XINLI_KERNEL_URL` | 编排必填；不含用户信息、查询或 fragment |
| `-store` | `XINLI_STORE` | serve 只允许 `pg`；dev 默认 `memory`，可选 `pg` |
| `-mcp-credentials-file` | `XINLI_MCP_CREDENTIALS_FILE` | 凭据引用 JSON 文件，与下行互斥 |
| `-mcp-credentials-env` | `XINLI_MCP_CREDENTIALS_ENV` | 凭据引用 JSON 所在变量名 |
| `-kernel-tokens-file` | `XINLI_KERNEL_TOKENS_FILE` | 按业务线的令牌来源 JSON 文件，与下行互斥 |
| `-kernel-tokens-env` | `XINLI_KERNEL_TOKENS_ENV` | 令牌来源 JSON 所在变量名 |
| `-kernel-token-env` | `XINLI_KERNEL_TOKEN_ENV` | 兼容单业务线配置，不能与令牌映射并用 |
| `-kernel-audience` | `XINLI_KERNEL_AUDIENCE` | `yanshi` |
| `-kernel-token-ttl` | `XINLI_KERNEL_TOKEN_TTL` | `15m`；大于 1 秒、至多 1h，还须符合内核 max_ttl |
| `-kernel-token-refresh-before` | `XINLI_KERNEL_TOKEN_REFRESH_BEFORE` | `1m`，小于 TTL；0 使用默认窗口 |
| `-auto-schedule` | `XINLI_AUTO_SCHEDULE` | true；关闭只停自动调度，恢复和清理仍运行 |
| `-mcp-dir` | `XINLI_MCP_DIR` | 可选登记输出目录，文件权限 0600；只允许 Persona 角色 |
| `-console-url` | `XINLI_CONSOLE_URL` | dev 的控制台链接 |
| `-insecure-dev-mcp` | `XINLI_INSECURE_DEV_MCP` | false；只能 dev，且不得与凭据配置并用 |
| `-exit-on-stdin-eof` | `XINLI_EXIT_ON_STDIN_EOF` | false；供父进程管理生命周期 |

`migrate` 只接受两个 DSN 参数。库名须互不相同；若设置了 `YANSHI_PG_DSN`，也检查它的库名，防止误配；该变量不会成为业务库 DSN 的回退。不同 schema 不算独立数据库。未提供内核 DSN 时，部署方仍须保证三库隔离。

## Persona MCP 服务凭据

凭据引用文件示例（实际秘密通过 Secret 注入环境变量，不进登记文件）：

```json
[
  {"business_line":"xinli","token_env":"XINLI_MCP_OLD","expires_at":"2026-12-01T00:00:00Z"},
  {"business_line":"xinli","token_env":"XINLI_MCP_NEW","expires_at":"2027-01-01T00:00:00Z"}
]
```

每把凭据只属于一个业务线，必须有期限；禁止同一秘密重复登记或跨业务线复用。启动拒绝缺失环境值、未知业务线，以及没有任何有效凭据的业务线。最后一把尚未过期的凭据用于生成该业务线的登记，旧新两把在各自期限内都可校验。

内核登记沿用现有能力：

```yaml
name: xinli
business_line: xinli
url: https://persona.example.com/mcp
headers:
  Authorization: "Bearer ${XINLI_MCP_NEW}"
timeout: 30s
attests_speakers: true
cite_captures: true
tools:
  no_approval: [file_changes, file_day, set_status, update_commitments, propose_merge, file_week]
```

Persona 和内核部署中对应环境值必须一致。请求头验证覆盖初始化、工具发现和每次工具请求；恒定长度摘要做常量时间比较，未知、缺失、过期一律返回固定 401。普通工具及独立的 `captures` 路径都在访问 Store 前核对 `_meta["yanshi/business_line"]` 与凭据业务线一致，并要求 EndUser 与 Session 非空。MCP 按业务线装配独立的有状态 HTTP Handler，保留已有协议协商和工具结果元数据；不同业务线的 MCP Session 相互不可见，不能通过复用连接继承另一把凭据的身份；协议元数据仍只有持有该业务线凭据的可信内核才能声明，不是客户端登录态。

轮换顺序：先在 Persona 配置新旧两把并滚动重启，再更新内核登记及环境并重启使用新把，最后在旧把过期或无流量后移除。Persona 每请求重新检查到期，但凭据集合在启动时加载；内核 `${ENV}` 也只在加载登记时展开。不是运行时热加载，也没有单令牌撤销列表。

dev 默认每业务线生成一把 256-bit 随机凭据，有效期 24 小时，通过 stdout 就绪协议与私有登记文件交给父进程；重启后换把。该机器协议包含开发秘密，不应收集为普通日志。dev 的 fixture/devui 始终只允许回环监听。显式 `-insecure-dev-mcp` 可供隔离开发使用，serve 一律拒绝。dev 无 kernel-url 时沿用评测的 `AllowUnmanaged` 夹具行为。

## 内核 TokenSource

`kernel.Client.Tokens` 替代静态 `Token`。`TokenSource.Token(ctx, businessLine)` 按业务线取令牌；Client 在每次 HTTP 请求前调用它。无业务线的方法由 `kernel.WithBusinessLine` 接收任务 owner；有显式业务线的方法必须与 context 一致。缺失或冲突直接拒绝，不按 Session ID 猜测，也不修改共享客户端身份。

`kernel.NewTokenSource` 的配置示例：

```json
{
  "xinli": {"key_file":"/run/secrets/xinli-signing.pem"},
  "another": {"token_file":"/run/secrets/another-service.jwt"}
}
```

每条业务线在 `key_file`、`token_file`、`token_env` 中选一个：

- 私钥模式：读取 PKCS#8 PEM，头部须有 `kid` 和 `business_line`，兼容 `yanshi keygen` 的格式。支持 Ed25519/EdDSA、P-256/ES256、至少 2048-bit RSA/RS256。签发 `iss`、`aud`、`scope=service`、`iat`、`exp`，不带 `sub`；内核只配置公钥。互斥保护缓存与刷新，进入刷新窗口后重读密钥文件并签发，刷新失败拒绝发送，不回退旧令牌。同路径原子替换可轮换私钥；先登记新公钥、后换私钥、最后等旧令牌过期再撤旧公钥。
- 静态模式：每次调用重读环境变量或文件，支持外部提供方轮换文件；只做业务线、服务 scope、audience、时间的选择检查，不代替内核验签。进入过期前窗口仍未拿到替换值则拒绝。运行进程的环境变量由启动方固定时，替换它需要重启；文件可原子替换。

生产编排启动前确认配置业务线均能取令牌；没有共享静态令牌回退。dev 可以不配置令牌，连接内核的回环 `-auth none`。JWT 的业务线 `max_ttl`、部署 audience 与公钥配置仍由部署方保持一致；内核撤销现状不变。

## 验证与边界

新增测试覆盖：生产无开发路由；合并/分离角色、后台不占 MCP 端口；PG 就绪、独立迁移与未迁移拒绝；正确/错误/过期/跨业务线/轮换凭据、连接建立后的凭据切换和到期；静态来源重读、三种签名、并发刷新、私钥轮换、刷新失败与 Client 业务线隔离。既有 dev 跨进程 fixture/MCP/EOF 测试继续使用生成的凭据。

真实评测由 `xinlifixture.StartWithOptions` 启动 `xinli dev`，开发入口为每条业务线生成临时凭据，放入 stdout 就绪协议的每条 `registrations[].headers.Authorization`。父进程经 `mcpcap.ParseServer` 保留完整 headers；开发秘密是直接值，不依赖父进程里不存在的环境变量。运行器用 `newEvalMCP` 按业务线分别装配 Connector，各自持有原登记、工件服务、日志和 logger；工具发现与执行仍使用内核原 Connector，其 HTTP transport 每次注入对应 headers。这样 `eval` 与 `eval-nohealth` 的同名 `xinli` 登记不会复用另一条业务线的缓存连接，工具名仍相同。

`internal/eval/capture_pages_test.go` 的直连 SDK 路径也传递登记 headers。`TestXinliFixtureMCPHeadersAcrossBusinessLines` 通过真实 dev 子进程、原就绪解析与运行器同一装配，轮流发现并调用两条业务线的工具；无凭据请求必须返回 401。Persona 的 fixture、Captures 和 Parity 测试均配置测试凭据，Parity 保留原始工具文字、Meta 比较和 golden，不过滤或改写差异。不改内核执行路径、工具说明、AgentDef 或评测基线。

现有内核 `mcpcap.LoadDir` 要求 Server 名称唯一，Connector 缓存也按名称索引；因此不能把多个名为 `xinli` 的业务线登记加载到同一个内核 Connector。因此登记同名 Server 的业务线须分属不同单元（单元的定义见[部署设计](./deployment.md) §1，2026-11-14 修订：原写"生产按现有一业务线一单元部署"），每个内核目录只放所属单元中业务线的登记；多业务线 Persona 可服务多个这样的内核。A11 不更改该内核限制。全账号注销、产品 BFF 与用户登录态仍待 A12/B1；Persona 清理完成不代表逐库注销完成。
