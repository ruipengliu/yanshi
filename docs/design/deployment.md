# 生产部署形态：一个二进制、按角色分组的模块化单体

> 状态：设计（2026-10-11）；§6 的角色开关、两个 DSN、就绪检查与队列指标已实现，部署清单已写、尚未在集群上验证 · 来源：[延展性评审](../review/2026-10-11-scalability-review.md) §8、§9 · 关联：[章程](../charter.md) §4、§5 原则 9，ADR-0001（私有化）、ADR-0008（沙箱信任边界）、ADR-0013（跨进程增量）、ADR-0028（分片键）、ADR-0029（工作类别与移交）· 术语以 [CONTEXT.md](../../CONTEXT.md) 为准
>
> 本文回答两个问题：生产环境怎么部署；进程边界按什么划。实施前须完成 §6 的代码改动，上线前逐项核对 [上线检查清单](../launch-checklist.md)。

## 1. 原则

- **进程边界沿扩缩容维度、权限与故障域划分；包边界沿领域划分。** 同一个镜像、同一套包，按角色裁剪后以不同的 Deployment 运行。不按领域拆服务（Session、Memory、用量……），理由见 §5。
- **单进程形态长期保留**（ADR-0001、章程原则 9）：开发、模拟测试与小规模私有化部署都运行全部角色；生产形态只是把角色分到不同进程，执行语义不变，`internal/sim` 仍然覆盖它。
- **单元（cell）是一组共享 EndUser 身份与 Memory 的业务线**（2026-11-14 修订，原为"一条业务线一个单元"）。Token、配额、AgentDef、内容安全、保留策略都以 BusinessLine 为单位，所以一个单元承载几条业务线都不需要改代码；单元内按 Session 与 EndUser 两类键分片 PostgreSQL（ADR-0028、ADR-0051），EndUser 键不加业务线，同一 EndUser 的跨业务线 Memory / Grant 因此在同一个库（章程 G4、ADR-0003）。一个 Session 只属于一个单元，入口按业务线路由到它所在的单元，不做跨单元双活。
  - **默认所有业务线在同一个单元。** 只有不与其他业务线共享 EndUser 的业务线（例如私有化客户的独立部署）才单独成为一个单元。
  - **按业务线拆单元可以作为扩容手段**，代价是分属不同单元的业务线之间不能共享 Memory / Grant：内核没有跨单元的召回与授权路径。需要共享 Memory 的业务线被迫拆开时，重审 ADR-0028、ADR-0051。
  - **现有限制**：`mcpcap.LoadDir` 要求 MCP Server 名称在一个内核中唯一，两条业务线登记同名的 Server（如评测中的 `eval` 与 `eval-nohealth` 都登记 `xinli`）时须分属不同单元（[xinli 部署](./xinli-deployment.md)）。

## 2. 单元内的拓扑

```
Internet ──▶ Ingress（WebSocket、SSE 长超时；握手限速在网关内，见 m3-duplex-channel）
                 │
          ┌──────┴──────┐
          │  gateway    │  HTTP API · Connection（WebSocket）· SSE · Call 中转
          │  N 个 Pod   │  peer 服务：向其他进程提供本进程的实时增量（ADR-0013）
          └──────┬──────┘
                 │ 读写 PostgreSQL · 拉取 peer 增量
   ┌─────────────┼───────────────┬────────────────┐
   │             │               │                │
worker        worker-bg     sandbox-ctl        janitor
交互池        后台池         沙箱控制器          1～2 个
(Interactive) (Background)  独占节点池          租约互斥
                            持有容器/K8s API 权限
   │             │               │
   └─────────────┴───────┬───────┘
                         ▼
   PostgreSQL：主 + 同步备（Patroni 或等价方案）× 分片数
   PgBouncer 事务池（SQL）· 直连（LISTEN、迁移的 advisory lock）
   S3 兼容对象存储（工件；工作区归档）
   外联（ADR-0001 显式列出、可替换）：模型提供商、内容安全提供商、推送通道
   migrate Job：滚动发布前执行 `yanshi migrate`；serve 以 -migrate=false 启动
   Prometheus：抓取各 Pod peer-addr 上的 /metrics
```

## 3. 角色

| 角色 | 承担 | 扩缩信号 | 部署要点 |
|---|---|---|---|
| gateway | HTTP API、Connection、SSE、Call 中转、peer 服务 | 连接数、握手速率、Call 并发 | headless Service 使 `-peer-advertise` 得到稳定的 Pod IP；PodDisruptionBudget 限制同时下线数；终止宽限期 > `-drain-timeout`；通知分片归属路由（评审 §8.3）落地后，Session 按分片固定到少数网关副本，进程只 LISTEN 自己分片的频道 |
| worker（交互池） | 只认领 Interactive 工作（ADR-0029） | 交互队列的等待时长 | 实例小而多，保证首字延迟；长任务在两步之间移交给后台池 |
| worker-bg（后台池） | 认领任意工作，承接长任务 | 后台队列深度、模型并发余量 | Worker 数可以很大（goroutine 便宜），实际受 `-model-concurrency` 与提供商账号上限约束；`-drain-timeout` 内完成当前步后以 handoff 移交 |
| sandbox-ctl | 沙箱控制器 | 沙箱队列深度 | 单独节点池 + 按 ADR-0053 选定的隔离运行时（选型前不预设 gVisor，2026-11-14 修订）；只有这个角色持有容器或 K8s API 权限（ADR-0008：受信控制器与不可信沙箱不同 Pod，网关与 Worker 不碰沙箱） |
| janitor | 关闭、删除、保留策略、清理、远期停放迁移 | 固定 1～2 个 | 靠租约互斥；小规模部署可并入 worker-bg |
| Call 中转 | 音频中转、转写、run_task | 并发通话数（每路网关收发约 160KB/s） | 初期留在 gateway；带宽或 CPU 挤占 SSE 时拆成同镜像的独立角色，客户端由 CallStart 的应答指向它 |
| migrate | 应用迁移 | 一次性 Job | 发布流水线中先于滚动更新；含 `CREATE INDEX CONCURRENTLY` 的迁移以 `-- yanshi:no-transaction` 单独执行 |

每个角色只是 `serve` 的一个子集：模拟测试与单进程部署运行全部角色，角色之间不新增任何契约。

## 4. 基础设施

- **PostgreSQL。** 每个分片一个主库加同步备库；已确认的 Event 不能因故障切换丢失或 seq 倒退，所以同步复制是硬要求，旧主须被隔离（fencing）后才允许新主接受写入。连接分两类：SQL 经 PgBouncer 事务模式复用；`LISTEN` 与迁移锁必须直连（事务池不保留会话状态），因此 serve 需要两个 DSN（§6）。故障切换后 `pg.Notifier` 重连、清空缓存并唤醒全部等待者，日志末尾对账补上期间的事件。
- **对象存储。** 任何 S3 兼容实现（ADR-0011）；私有化环境用 SeaweedFS、MinIO 或 Ceph RGW。
- **入口。** 支持 WebSocket 升级与小时级空闲连接；SSE 与 WebSocket 不经过会缓冲响应的中间层。握手限速与带抖动的 `Retry-After` 由网关自身处理。
- **可观测性。** 指标在 peer-addr 的 `/metrics`，只记录 ID（ADR-0015）。`yanshi_db_pool_*` 由一个进程级收集器汇总活动分片的 SQL 池，沿用单库的无标签指标，不含独立直连池；来源随存储关闭注销，详见 [M2 扩容压测](./m2-scale-test.md) §3。链路追踪按 `run_id`、`attempt`、`call_id` 关联，列入上线检查清单。
- **配置分发。** AgentDef、发布配置、业务线公钥、价格表、MCP 登记以挂载卷（ConfigMap/Secret）分发，所有进程同一版本；发布配置的热加载（10 秒）保持不变。
- **外联。** 模型提供商、内容安全提供商、推送通道是仅有的出网依赖，按 ADR-0001 在部署清单中显式列出、可关闭或替换；网络策略默认禁止其他出网。

## 5. 为什么不按领域拆微服务

拆进程的判据只有四条：扩缩维度不同、权限与故障域不同、资源形态不同（连接型、计算型、带宽型）、发布节奏不同。§3 的角色全部来自前三条；第四条在本项目不成立：单仓库、契约在 `proto/`、一次发布一个镜像。

按领域拆（Session 服务、Memory 服务、用量服务……）会直接损害现有的几项资产：

- 热路径是同步串联的：Submit 要经内容安全、配额、写日志、入队，一个 Run 仍有 34 条数据库语句（[扩容压测](./m2-scale-test.md) §7）；每拆一刀多一跳网络与一个失败点。
- ADR-0028 要求一个事务只涉及一个键，这是同进程、同连接池里才便宜的约束；跨服务后要么退化为多步写入，要么引入分布式事务。
- 每个服务边界都要新增契约、一致性套件与差分模拟的接入；`internal/sim` 把整套语义放在一个进程里确定性重放，是项目最大的质量资产。
- 章程要求单进程可运行；模块化单体天然满足，微服务要再维护一套合并形态。

当某个角色出现第四条判据（独立团队、独立发布）时再拆，拆的粒度仍是角色，不是领域。

## 6. 实施前要做的代码改动

| 改动 | 说明 | 验收 |
|---|---|---|
| 角色开关（已实现） | `serve -roles gateway,worker,sandbox,janitor`，默认全部；没有 gateway 的进程不监听 `-addr`，存活与就绪检查在 `-peer-addr` 上；没有 worker 时 Worker 数为 0；没有 sandbox 时沙箱工具照常登记、调用由其他进程的控制器执行。存储由装配包 `internal/app` 构造（评审 §9.1），核心对象图由 `app.NewCore` 构造：serve、模拟测试、评测与 e2e 测试共用同一张图，serve 只决定每个角色起多少个 Worker、控制器与 Janitor | 任一角色组合都能启动；全部角色等价于原来的 serve；已验证"只有 gateway"与"只有 worker、janitor"的两个进程共用 PostgreSQL 完成 Run |
| 两个 DSN（已实现） | `-pg-dsn` 可经 PgBouncer 事务池；`-pg-listen-dsn`（`YANSHI_PG_LISTEN_DSN`）直连，用于 `LISTEN` 与迁移锁（`pg.OpenDirect`，小连接池）；为空时共用 `-pg-dsn` | 经事务池的部署中通知正常送达；故障切换后 LISTEN 在约定 RTO 内恢复（心跳 10 秒、超时 10 秒后重建，见 [M2 扩容压测](./m2-scale-test.md) §8） |
| 就绪探针（已实现） | `/healthz` 只返回 200；`/readyz`：数据库可达、本进程的 LISTEN 连接已建立（`pg.Notifier.Ready`）、迁移已全部应用（每分钟复查） | 滚动发布时流量不会打到尚未监听通知的 Pod |
| 部署清单（已写，未在集群上验证） | Kustomize：`deploy/k8s/base`（每个角色一个 Deployment、migrate Job、网关的 Service 与 PDB、网络策略、各角色共用的探针与配置挂载），`deploy/k8s/overlays/example`（以仓库中的开发配置生成 ConfigMap，密钥为占位值）；镜像 `deploy/Dockerfile`。交互池与后台池是两个 Deployment（`-worker-pool interactive` 与 `any`） | 清单可审查（`kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/k8s/overlays/example`）；一套制品在无外网 K8s 完成部署（章程 §8），须在目标环境验证 |

## 7. 容量与扩缩

- 扩缩信号：模型准入等待（`yanshi_model_admission_wait_seconds`）、连接数（`yanshi_nodes_connected`、`yanshi_live_peer_streams`）、握手拒绝（`yanshi_handshakes_rejected_total`）、认领结果（`yanshi_queue_claims_total`）。队列等待与积压：`yanshi_queue_wait_seconds{queue, priority}`（认领时观察，工作变为可认领到被认领的时长），`yanshi_queue_ready{queue, business_line, priority}` 与 `yanshi_queue_oldest_wait_seconds`（Janitor 所在进程每 15 秒统计）。它们也是 Submit 准入阈值的依据（`-admission-max-wait`，评审 §8.4 第 5 项）。
- 单元容量上限由 PostgreSQL 分片数与通知投递次数决定（评审 §8.3）；容量矩阵与准入阈值由评审 §8.4 第 4 项的压测给出，在目标环境实测后填入部署清单。
- 模型并发是全局约束：`-model-concurrency`、`-model-rps` 按账号上限除以持有该提供商的进程数配置（[模型调用](./model-calls.md) §4）；弹性扩容时须随副本数联动，或实现共享令牌服务。

Janitor 的 Sweep 同时把 Horizon 内的远期停放移回工作队列（[M7 §5.4](./m7-kernel-primitives.md#54-语义)）。默认 Horizon 一小时、Sweep 每分钟；每批 1000 项，每批重新取当前时间，直到不足一批或时间预算用尽。一次 Sweep 的全部队列迁移共用 `Janitor.PromoteBudget`，默认 SweepEvery 的一半（30 秒），可通过字段设置；在批次边界检查预算，允许已开始的一批完成。Horizon 须覆盖多轮扫描与重试。Janitor 停摆超过 Horizon，或积压长期超过预算内的迁移能力，会导致定时晚到；恢复后继续迁移，不会提前执行。

### 数据库分片与兼容定位

[ADR-0051](../adr/0051-shard-slots-and-id-location.md) 固定 1024 槽：SHA-256 的前八字节按大端解释后 `% 1024`，再由部署静态表映射到稳定分片号；不能 `% 当前库数`。Session 数据读 Session ID 的自身槽位，ByEndUser 的 Session 索引读其第二个槽位；工件与调用随 Session，Memory、Grant、用量、Tier 等随 EndUser。槽位只决定查哪个库，授权仍核对真实归属。

`internal/app.Sharded` 在装配层把每库一整套 `Stores` 组合起来；每库由 `PostgresShard` 构造自己的 Log、三类 Queue、Waking、Snapshots、Outbox 与其他存储。在线方法以参数中的真实键经 `Locator` 选唯一分片，不逐库探测。PG 工件与索引的 `Select` 只断言目标就是本库，不再另选连接。内存分片沿用同一包装，serve、sim、eval、e2e 仍经 `app.Stores` / `app.NewCore` 装配。

**P4 只支持新建单元，或全部槽位留在库 0 的既有单元。把旧数据迁离库 0 不在 P4 支持范围。**（阶段缺口，见 ADR-0051 §5；2026-11-14 补充） 静态映射无法覆盖旧 Session 之后生成的旧式工件与派生 ID；支持这种迁移需要另行设计在线目录。多库新单元必须关闭旧数据回退且兼容条目为空；既有单元允许使用兼容清单，但全部槽与条目必须仍指向库 0。不能添加一条无关映射绕过启用前提。历史预检与兼容条目的语义仍见 [ADR-0051](../adr/0051-shard-slots-and-id-location.md) §5。

#### 配置与迁移

不传 `-shards` 且未设置 `YANSHI_SHARDS` 时，保持原单库入口：`-pg-dsn`、可选的 `-pg-listen-dsn`，全部槽位到库 0。内存开发入口同样保持原行为。

多库使用受控 JSON 文件；`serve -storage postgres -shards <file>` 与 `yanshi migrate -shards <file>` 读取同一文件，也可用 `YANSHI_SHARDS` 指定文件路径。SQL 和直连 DSN 都必须显式填写，不从数组位置推断分片号：

```json
{
  "table": {
    "shards": [0, 1],
    "ranges": [
      {"first": 0, "last": 511, "shard": 0},
      {"first": 512, "last": 1023, "shard": 1}
    ]
  },
  "compatibility": {
    "version": "new-cell-v1",
    "legacy_data_in_shard0": false,
    "entries": []
  },
  "databases": [
    {"id": 0, "sql_dsn": "postgres://sql-proxy/kernel0", "direct_dsn": "postgres://pg0/kernel0"},
    {"id": 1, "sql_dsn": "postgres://sql-proxy/kernel1", "direct_dsn": "postgres://pg1/kernel1"}
  ]
}
```

文件包含凭据时按 Secret 管理；DSN 和兼容清单不写日志。解析拒绝未知字段、重复字段、重复或缺失分片、遗漏 DSN，并调用 `ValidateActivation`。迁移 0041 建立固定 `public.shard_identity`（Global，每库各一行），含随机 `instance_id` 与配置中的 `shard_id`。分片迁移先应用 schema，再用 INSERT ON CONFLICT 初始化空身份，随后只读核对，绝不改绑已有分片号。固定 public 防止两个 schema 把同一个 database 伪装成两个分片。SQL/直连必须读取同一身份和业务 schema；实例 ID 不得跨片重复，DSN 互换或重复都拒绝。运行角色只需 SELECT 身份表，不再依赖 `pg_control_system()`。主备复制保留该行，切换协议与旧主隔离见 §10。

生产先运行 `yanshi migrate -shards <file>`，再让各角色以 `-migrate=false` 启动；开发可用 `-migrate=true` 逐库迁移。每库使用自己的 SQL 池、直连池与 Notifier；迁移锁只走直连，启动检查可达 SQL 入口的迁移与身份，并启动每库独立重连的 Notifier。`-migrate=false` 时暂时不可达的库保留池、等待恢复，不阻止健康片冷启动；可达库身份/迁移错误仍阻止启动。恢复目标必须再次通过身份、schema 与迁移校验。所有角色使用相同配置，全部角色合在一个进程仍可运行。`notify-shards` 只控制每库内部的通知频道数，与数据库分片数无关。

`/readyz` 的 `X-Yanshi-Routing` 返回 `Locator.Identity()` 的摘要（不含 DSN）；发布时核对所有角色实例的摘要相同，不能只比兼容版本标签。就绪探针返回每片 `sql`、`migrations`、`migration_version`、`listen`、`identity`，即使失联也保留该片。探测并发 4、每片 1 秒、整轮 4 秒，结果缓存至多 1 秒。

| 角色 | 就绪规则 | 理由 |
|---|---|---|
| gateway | 至少一片 SQL、迁移、身份与 LISTEN 正常 | 可以承接健康目标的请求及订阅；不要求控制库 0 正常 |
| worker / worker-bg | 至少一片 SQL、迁移与身份正常 | 认领轮转避开坏片，通知断开由轮询兜底 |
| sandbox | 同 worker | 健康 Session 的沙箱工作可独立执行 |
| janitor | 同 worker | 独立清理不能因另一片失联停摆 |
| 组合角色 | 含 gateway 时采用 gateway 规则，否则采用 worker 规则 | 每个已启用角色至少能处理一片工作 |
| migrate | 全部登记库完成迁移与身份核验才成功 | 发布不能把缺片报告为完成 |

目标依赖仍逐项检查：Session 日志片、EndUser 索引/屏障片、用量片分别路由，碰到不可用依赖返回固定 503 与 `Retry-After: 1`，不返回 SQL/DSN。库 0 只限制删除请求等实际依赖其控制数据的操作；无关健康 Session 继续。每个短存储操作默认 1 秒；订阅 Wait 的生命周期仍由调用者取消。汇总最多 4 秒，健康 Run 触及业务线配额时只能使用有效完整缓存或成功的完整汇总。

#### 队列、离线操作与验证边界

三类队列公平轮转，各库一次 Claim 尝试默认最多一秒；空库或失败库之后继续尝试，全部为空才返回空队列。租约携带来源分片，Renew / Park / Release 回到原库并复核 Session 定位与来源一致。各库的入队通知合并给等待方，转发随进程上下文取消。Waking 在选定分片内原子写日志、队列与信封；`pg.WithTx` 遇到另一 pool 的路由或 `pg.Exec` 报错，不能静默跨库独立提交。

Offline 统一经 `app.fanout`：并发默认 4、每片 1 秒、每轮 4 秒，起点轮转。分类完整见 [ADR-0051](../adr/0051-shard-slots-and-id-location.md) §6：清理与保留候选允许健康片继续；List、Stats、Report、CountRequest、跨库 Inbox/Memory 清理必须全片成功。候选扫描保存每路径/每片的环扫位置，失败不推进，重启重扫；删除、队列提升和出站地板本身是持久进度。单批至多每片 limit，不丢弃已推进位置对应的行。多片 Janitor 累计错误后继续独立阶段与其他候选；某一删除步骤或注销屏障仍须所有依赖完成后才确认。

业务线 Spent 缓存完整部分和，键含 `Identity()` 的版本和摘要、统计窗口及业务线，值含全部分片、截至时间和到期时间。默认有效期 15 秒，从扫描开始计算，最多 1024 项；只有全片成功才发布。有效完整缓存可跨短暂失联使用，到期且缺片返回不可用，不补零。EndUser 直接读取其所属片。Report 不用配额缓存。静态映射重新装配会丢弃旧缓存。软配额的超出窗口包含缓存期内新增费用；它不是额度预留。

未指定 EndUser 的列表仍以 `(last_input_at,session_id)` 稳定归并，外部游标是各片共同的安全边界；不完整时不返回列表。删除进度先从库 0 读请求并授权，再有界统计所有片，缺片返回 503，不返回 done。

`GET /v1/outbox?shard=N` 读指定片，游标同时绑定分片和 epoch；跨片使用返回 403。`/v1/outbox/shards` 返回配置中全部分片及 `version`/`identity`，每项含状态、安全起点 H 和该片槽位列表，失联项保留且没有 H。每片独立订阅和周期复查。

xinli 多片消费从该枚举启动每片独立的长轮询、退避和恢复。应用迁移 0009 将 `outbox_recovery` 主键扩为 `(business_line,shard)`；-1 保留单片旧进度和短事务互斥行，多片各自从新 H 对账，不重用旧游标。恢复仅重放本片 Session；已取得 H 的片在账号/索引对账等待期间仍接收出站，只有完整对账与追赶后才进入 follow。进度中的恢复 Revision 区分收页与恢复的并发写入，CAS 冲突有界合并最新游标，不按断网退避；重置会使旧恢复步骤失效。每片事务保留业务线锁与账号终态检查，避免删除后迟到页复活通知。重试日志包含阶段、分片、固定错误类别与相关 ID。应用不依赖内核包。

新增 `TestShardedConsistency` 在内存与两个真实 database 上跑适用存储套件（旧夹具全部留库 0，跨片键另测）；`TestShardedRouting`、队列/故障/事务与启动配置测试覆盖两库边界。`pgtest.Database` 创建随机、可清理 database，测试角色需要 CREATEDB；既有 Docker PostgreSQL 测试角色满足该前提，schema 只用于每库内部的用例隔离。

分片差分采用方案 (a)：模拟队列只读各库头部候选，按优先级、全局逻辑入队/归还顺序选择，并涵盖 Waking 内部入队。生产仍公平轮转。`CompareShards` 仅在新差分种子启用：保留全部 Session 日志、状态指纹，出站列表按 Session/seq/信封 ID 规范化，忽略跨库不存在的全局插入顺序；C5a 直接核对各库全部信封，不测试 C5b 的流游标。普通单库种子不启用该选项、不改变原指纹定义。新增单库内存/双库内存/双库 PG 三方比较，逐库删除不变量，以及 Session 分布、跨片父子与同用户跨片 Session 计数；短 Horizon 另外要求远期停放和提升覆盖。

`make check` 默认包含 3 个双库种子，`make sim` 以 `-sim.shardseeds=100` 增加同量级双库种子。

`TestDayShards` 使用两个真实内核 database 与独立 xinli 进程，覆盖两片创建、输入、Run、跨片委托、级联删除、EndUser 注销及两片出站消费，并用 TCP 代理切断一片的实际 SQL/LISTEN 连接、验证健康输入/Run/创建/消费继续及恢复后续传；`make check` 和 `make day-short` 都包含它。使用脚本模型、最多 32 次寻找跨片父子，测试打印总耗时并断言小于 60 秒；正常预期约 10–20 秒，具体耗时以对应源码摘要的远端报告为准。冷启动要求两片持久进度均完成 follow，并校验版本、分片与非空游标；失败诊断只读取恢复阶段、版本与 ID，不输出游标。`TestShardOutageHealthySessionContinues` 覆盖可用性接缝上的故障与恢复，真实 TCP 断片恢复由双库 day 场景覆盖，角色探针与冷启动另由装配测试覆盖。P4 仍不做在线迁槽、自动再平衡、旧数据迁出库 0 或网关通知归属。

### 到期表的首次升级与回滚

当前没有真实用户，这次升级采用一次性替换，作为前述通用滚动发布流程的例外，不加兼容开关。旧版本不认识 work_dues，也不使用新版本的事务锁，不能与已启用到期表的新写入者混跑。

1. 先执行 schema 迁移（包含 work_dues 及其并发索引），旧版本此时尚不写到期表。
2. 停止流量和提交任务的入口，停止并排空全部队列写入者，包括 gateway、两类 Worker、Janitor，以及任何直接写队列的进程；确认没有旧写入者仍在运行。
3. 一次性替换为新版本后恢复全部角色与流量，确保 Janitor 同时运行。

回滚前同样停止流量与全部队列写入者，再用新版本的一次性维护进程构造 Session 队列，反复调用 `Queue.Promote`，before 设为覆盖 work_dues 全部 due_at 的远未来（例如全部期限早于 9999 年末时使用该时刻），limit 可用 1000，直到到期表为空；每批检查错误，失败则重试维护，不能启动旧版本。该维护直接调用 Promote，不受 Janitor 的 Sweep 时间预算限制；候选扫描仍只读，每个 Session 独立事务。它会保留类别、ord、ready_at 和精确 parked_until，不能用 Enqueue 代替，否则会提前唤醒。确认 `SELECT count(*) FROM work_dues` 为零后再启动全部旧版本进程；保留新增表和迁移记录，不回退 schema。远期项回到 work_items 后，旧版本仍按 parked_until 执行，但会重新承担百万停放的认领扫描代价。

## 8. 未做

- 通知分片的归属路由（评审 §8.3）；在此之前每个网关进程监听全部分片，单元内的进程数受通知投递上限约束。
- 跨单元的业务线迁移与 Session 导出。
- 多地域部署与灾备切换：Session 数据只在一个单元内，异地只能冷备或按单元整体切换。

## 9. 单次调用部署参数

单次调用在 gateway HTTP 处理路径中同步执行，经 Core 共享模型、内容安全、配额、计量与删除屏障；不新增进程角色。各进程的声明与发布配置应一致。

| serve 参数 | 默认 | 语义 |
|---|---|---|
| -oneshots | oneshots | 声明目录；不存在或没有声明时不提供单次调用（请求返回 404），跳过单次调用限额校验 |
| -oneshot-concurrency | 8 | 本进程全部单次调用的并发上限，必须为正；model-concurrency 大于 0 时必须严格小于它 |
| -oneshot-wait | 2s | 每一层并发名额独立的等待上限，0 为只尝试立即获取，负值拒绝 |
| -oneshot-deadline | 60s | 从声明校验之前开始的总时限，含审核、排队与一次 Schema 修复；必须为正 |

业务线 YAML 可写 `oneshots: {max_concurrency: 4}`；0 表示只受合计及声明上限限制。三层都是进程内上限，分角色或多副本部署必须按提供商账号容量静态分摊，扩缩容时联动配置。只部署 gateway 也会调用模型，因此应计入持有提供商的进程数。声明模型在配有 Quota 时必须有价格；发布选择每 10 秒热加载，声明、并发及业务线设置修改需重启。

## 10. P4-C6：隔离同步主备切换与容量

### 10.1 环境、持久性与代理

`deploy/failover/compose.yaml` 固定项目名 `yanshi-failover`。主库、流复制备库使用相同 `pgvector/pgvector:pg16` 镜像，独立健康 PG 实例承载另一内核分片及应用库。三者仅发布到 Mac mini 回环端口 55430、55431、55432，卷和网络均属于该项目；与共享 `yanshi` 的 54329、58333 无关。入口的每条 Compose 命令都显式给 `-p yanshi-failover -f deploy/failover/compose.yaml`，不继承 `dev run` 的共享 Compose 文件。固定端口意味着不同工作树须串行使用此环境；脚本用临时目录锁排斥并发运行。`down -v` 只删除此演练项目。

主库配置 `synchronous_standby_names='FIRST 1 (failover_standby)'`、`synchronous_commit=on`，保持 `fsync` 默认开启。开始迁移和负载前，驱动核对 `pg_stat_replication` 为 streaming/sync。`on` 在返回提交成功前等待同步备库将提交 WAL 刷入持久存储；不要求备库已经应用，但提升会重放可用 WAL。它满足本演练“单次主库失效不丢已确认写入”，不承诺主备磁盘同时损坏仍无损。`remote_apply` 还等待备库查询可见，本场景无备库读流量，因此没有选它。依据：[PG16 同步提交设置](https://www.postgresql.org/docs/16/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT)、[PG16 备库提升](https://www.postgresql.org/docs/16/warm-standby.html#STANDBY-SERVER-OPERATION)。

固定端点采用 Go 驱动内可控 TCP 代理：每次演练绑定一次回环地址，所有 SQL 和 LISTEN 连接（包括 xinli 子进程和生产 serve 身份检查）使用该地址。管理连接仅用于核对复制及 `pg_promote()`。代理不按“端口可达”自动挑主；切换先关闭准入和存量连接，提升备库，再停止旧主容器并核实它不在运行列表，最后原地址改指新主。任一步失败都不开放入口。旧主配置 `restart: "no"`，代理目标只有新主，没有恢复后自动回选的路径。不得对保留的现场再执行 `up` 来恢复旧主；下一轮先删除整个隔离项目及卷再建。

备库覆盖 `synchronous_standby_names=''`，提升后继续本地持久提交；这是单次切换后的明确降级状态，不代表仍有同步冗余。生产须通过 Patroni 或等价 HA 控制器选主、STONITH/实例停机/网络隔离确认旧主失去写权限，再更新 HAProxy 或等价固定主库服务端点；SQL 可再经 PgBouncer 事务池，LISTEN/迁移锁走绕过事务池的固定端点。仅 TCP 健康检查、DNS 改名或 `default_transaction_read_only` 不能取代旧主隔离。旧主只能经 rewind/重新基准备份并作为备库加入；在再次承诺容忍主库故障之前恢复同步冗余。本演练不部署自动选主或验证生产 HA 控制器。

同一物理流的同步提升保留 `public.shard_identity`、出站 epoch、cursor key、地板和检查点，不旋转 epoch。测试同时比对身份与旧事件前缀，并运行生产 `yanshi serve -migrate=false` 的 `/readyz` 身份核验。恢复备份、PITR 或不能证明连续性时仍须按 [M7 §6](./m7-kernel-primitives.md#6-出站流outbox) 在开放入口前旋转 epoch；不能用换 epoch 掩盖本演练丢失已确认数据。

### 10.2 演练与结果记录

`make failover` 显式设置 `YANSHI_TEST_FAILOVER=1`，不属于 `make check`，不依赖 `deps-up`。每种场景重新创建隔离集群；所有应用进程和池保持原 DSN。报告只含时间、状态与计数，不保存输入、工具正文、凭据或游标。

| 场景 | 步骤与硬断言 |
| --- | --- |
| 内核 `TestFailoverKernel` | 两分片 `app.NewCore`、Worker、Janitor 和 xinli 消费者；片 1 同步提升。保留客户端读到的 Event 前缀和信封，持续提交带 input_id 的输入；近期 ID 重投必须返回同 Run/duplicate，持续负载先以完整日志核对全部确认输入，未知结果查无记录后才沿用原 ID，日志只出现一次且 seq 连续。真实工具经 FileLedger 的 Executor 先写 started 再产生副作用，结果回写前跨切换，恢复后重开 Executor 重投同 call_id，副作用计数仍为一。工具适配器替代设备传输，此处不另验 WebSocket/mobile。 |
| 内核恢复边界 | 真实长事务取得 xid 后阻挡已完成 Run 的信封，旧游标不能越过；旧主退出后事务中止，同 epoch 的旧游标能继续读到该信封。Notifier 的 Generation 增长、重新 LISTEN 并补提示；原连接池、Worker 认领、消费与 fake 发送继续。停机期间健康片持续创建、输入、完成 Run、读取出站并提交删除；故障片返回有界 503。恢复后 Janitor 删除完成、日志为空，生产 serve 核对两片身份成功。 |
| Persona `TestFailoverPersona/writes` | 复用上午补传、自动整理和 file_day；先确认用户修改。下午 file_day 已提交但通知尚未发出时保持 MCP 在途，下一批原文暂停于事务提交前并切换；同时尝试查询及用户编辑。恢复后已确认事实、总览、revision/progress 和编辑不变；未知上传同 ID 重交，同 ID 同引用；后续编辑按原 rev 重试，自动整理继续到新 revision，领域变化/消费收敛。 |
| Persona `TestFailoverPersona/deletions` | 在另一套全新主备中，确认用户修改后擦除原文，停编排器保留持久删除待办，再提升。待办 ID/载荷、数量及进度保持，恢复编排器完成删除与重新整理；旧 Session 日志为空、擦除批次重投拒绝、无关用户编辑保留。 |

内核和未知上传使用 3 秒客户端截止，Persona 可用性探针使用 20 秒；明确不可用的探针须返回 503，持续负载及人为暂停的未知请求可记录状态 0（连接错误/客户端截止），之后必须按原 ID 对账。报告的 `window_requests` 只统计代理关闭至新目标开放期间结束的请求，分别使用 `http_503` 等 HTTP 状态、`timeout`、`network` 计数；0 不当作成功；`max_request_ms` 记录最长耗时。`switch_window_ms` 从代理关闭到新目标开放（含健康片验证操作）；`recovery_ms` 从开放到实际服务/Notifier 恢复；`elapsed_ms` 为完整用例耗时。窗口包含人为验证时间，不是生产 RTO 承诺。

| Mac mini / 源码摘要 | 场景 | 结果 | 用例耗时 | 切换窗口 | 恢复 | 窗口内请求 | 最长请求 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-29，`b3d49d144a99b0a2` | 内核（片 1 提升，片 0 持续服务） | passed | 5551ms | 604ms | 483ms | http_202=6、http_503=22 | 9ms |
| 同上 | Persona writes | passed | 7382ms | 3607ms | 33ms | http_503=3、timeout=1（人为暂停的未知上传，3 秒截止，之后按原 ID 对账） | 3002ms |
| 同上 | Persona deletions | passed | 3077ms | 523ms | 32ms | http_503=2 | 2ms |

切换窗口包含驱动的人为验证步骤（暂停的提交、健康片操作），不是生产 RTO 承诺。

当前预算分层如下，均保留调用方更短期限：

| 层 | 预算 | 范围与原因 |
| --- | --- | --- |
| PG 建连 / 初始 Ping | 1 秒 | TCP、PG 握手及认证不可达时快速失败；Persona 与应用库沿用此约束 |
| Persona 借连接 | 1 秒 | 每个事务、直接扫描和 View 末尾屏障核对显式 Acquire；只限制尚未开始的操作 |
| xinli gate 借连接 | 1 秒 | 与锁排队分开，不把健康连接上的正常等待塞进短预算 |
| owner gate 等待 | 15 秒 | PG/内存一致；取得后回调继承父 context，保持串行、撤销与注销屏障 |
| Persona 事务内语句 / 行锁 | 15 秒 | 用事务内 SET LOCAL 设置，不污染归还连接或 DDL 迁移 |
| Persona 存储完整操作 | 30 秒 | 覆盖正常锁等待、全量账本读写和回调；不自动重试未知提交 |
| 普通认证请求 / Persona 查询与命令 HTTP | 30 秒 | 恢复正常排队与执行余量；默认 HTTP transport 的 TCP/TLS 建连各 1 秒，不限制正常响应头计算为 1 秒 |
| 已有 MCP 工具调用 | 15 秒 | 保留一天场景原有注册值，不变更模型、AgentDef 或结果判定 |

**为什么分层**：快速有界失败只用于"不可达、无法开始"（建连、握手、借连接）；正常的锁排队、全量账本读写与回调使用宽裕上限。曾经把 Persona 完整操作统一限制为 2 秒、owner 等待限制为 1 秒，failover 演练能通过，但真实模型的一天（`make day-ark`，以 main 对照）中下午整理两次未完成：`file_day / file_changes / captures` 经内核 Worker→MCP→Persona Store、不经 query/command 客户端，正常并发下的排队与账本读写超过 2 秒即成为工具错误。由此确定上表的分层，并在 `day-ark` 报告中增加逐阶段诊断（工具 `is_error` 与按工具计数、存储与编排告警、HTTP 状态、任务错误类别与重试），因为 `failed_runs=0` 不能证明执行层没有故障。

Persona 不可达返回固定 503 与 `Retry-After: 1`，凭据无效仍为 401（鉴权存储错误不伪装为 401）。演练中故意暂停于提交前的上传持有应用 owner gate；它因 3 秒截止取消后在被切断的连接上失败、回滚并释放 gate，同 owner 的 GET 随后进入 Persona 并得到 503；即使 gate 未释放，15 秒等待也有界返回 503。Persona 可用性探针使用 20 秒客户端上界、不接受状态 0；内核探针为 3 秒。没有为加速切换放开串行或盲重试写入。

无需主备环境的回归包括：同 owner 的慢操作（内存/PG）正常排队后成功；只接受 TCP 不应答的 Persona HTTP 与 PG 握手；Store 快速借连接超时、两次大于 2 秒的同 owner 正常写入与调用方主动缩短的行锁期限；鉴权存储错误不伪装 401；查询/命令半截响应在预算内映射为 503；父观察池断链恢复和离线代理恢复后的 schema 清理。

### 10.3 双库容量配方与报告

`make shard-capacity` / `scripts/shard-capacity.sh` 在独立项目中运行 `TestShardCapacity`。单库与双库都创建新的 database/schema、使用固定 TCP 代理、同样的两个 Worker、同一脚本模型与 FileLedger；双库采用 0–511 / 512–1023 槽位。固定 16 个 EndUser，每人 4 个 Session，每 Session 3 个 Run，每 Run 一次工具调用并消费完成信封，共 64 Session、192 Run。客户端并发固定 1，先后执行两个配置；吞吐的分母含创建、Run 执行与出站消费，p50/p95 是输入提交到消费到完成信封的完整延迟，不含建 Session。不设性能阈值，但 Session/信封总数与副作用去重必须正确。

当前两个独立 database 位于隔离的 healthy PG 实例，故没有 CPU/磁盘隔离，不据此宣称双机线性扩容。记录每库 Event、Session、完成信封数、schema 的 `pg_stat_user_tables` 插入/更新/删除增量、10ms 采样的应用池连接峰值及结束时 `pg_stat_activity` 的本库连接数。PG 统计异步发布，结束后等两秒采样，表写次数是观察值。WAL 为同一实例 LSN 增量，包含应用/Persona 的背景活动，不能冒称每库 WAL。报告同时给出总吞吐和 p50/p95；比较每库行数与写入量是否大致随槽位分摊，不把 16 用户的小样本偏差判为失败。

| Mac mini / 源码摘要 | 配置 | Run/s | p50 / p95 ms | 各库 Session / Event / 写元组（插/改/删）/ 池连接峰值 | 实例 WAL |
| --- | --- | --- | --- | --- | --- |
| 2026-10-29，`419431b4bdce0442` | 单库 | 22.4 | 38.2 / 75.1 | 库 0：64 / 1600 / 2090·256·192 / 5 | 0.95MB |
| 同上 | 双 database、同 PG 实例（0–511 / 512–1023） | 23.3 | 37.8 / 71.8 | 库 0：32 / 800 / 1066·128·96 / 5；库 1：32 / 800 / 1016·123·90 / 5 | 0.94MB |

负载按槽位均分（各 8 个 EndUser、32 个 Session），每库写入约为单库的一半；吞吐与延迟基本不变，因为客户端并发为 1 且两个 database 共用一个 PG 实例——这一结果只说明分摊正确，不说明多机扩容能力。

结果文件分别为 `eval-results/failover/capacity-1.json`、`capacity-2.json`。故障隔离使用 §10.2 内核演练的两个独立 PG 实例验证，容量入口不再切断同实例中的单 database；不将前者的故障隔离结论当作后者已有独立资源。Persona 一年账本＋八天原文的追加、分页、重交、擦除、p50/p99、SQL 与装载成本仍见 [Persona 架构容量章节](./persona-architecture.md#persona-大工作集成本a14)，本入口补内核出站与双库分摊，不替代 A14 配方。

### 10.4 Mac mini 命令

在本工作树调用；没有 Go 的编码机不执行 Compose/Go，不手写 SSH，不提交 Git。`dev fmt` 会取回格式化结果；其余运行产物留在远端。

```bash
dev fmt
dev run 'make failover'
dev run 'make shard-capacity'
dev run 'cat eval-results/failover/kernel.json'
dev run 'cat eval-results/failover/persona.json'
dev run 'cat eval-results/failover/capacity-1.json'
dev run 'cat eval-results/failover/capacity-2.json'
dev run 'docker compose -p yanshi-failover -f deploy/failover/compose.yaml ps -a'
dev run 'make failover-down'
```

`failover` 和 `shard-capacity` 每次运行都会删除上次隔离现场，先读取需要保留的结果。失败后保留容器/卷供检查；`failover-down` 显式删除独立卷。不得将上述命令中的项目名换成共享 `yanshi`。最终回归命令见 [一天验收](./day-acceptance.md#p4-关闭前的最终回归)。
