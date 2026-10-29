# 生产部署形态：一个二进制、按角色分组的模块化单体

> 状态：设计（2026-10-11）；§6 的角色开关、两个 DSN、就绪检查与队列指标已实现，部署清单已写、尚未在集群上验证 · 来源：[延展性评审](../review/2026-10-11-scalability-review.md) §8、§9 · 关联：[章程](../charter.md) §4、§5 原则 9，ADR-0001（私有化）、ADR-0008（沙箱信任边界）、ADR-0013（跨进程增量）、ADR-0028（分片键）、ADR-0029（工作类别与移交）· 术语以 [CONTEXT.md](../../CONTEXT.md) 为准
>
> 本文回答两个问题：生产环境怎么部署；进程边界按什么划。实施前须完成 §6 的代码改动，上线前逐项核对 [上线检查清单](../launch-checklist.md)。

## 1. 原则

- **进程边界沿扩缩容维度、权限与故障域划分；包边界沿领域划分。** 同一个镜像、同一套包，按角色裁剪后以不同的 Deployment 运行。不按领域拆服务（Session、Memory、用量……），理由见 §5。
- **单进程形态长期保留**（ADR-0001、章程原则 9）：开发、模拟测试与小规模私有化部署都运行全部角色；生产形态只是把角色分到不同进程，执行语义不变，`internal/sim` 仍然覆盖它。
- **一条业务线一个单元（cell）。** Token、配额、AgentDef、内容安全、保留策略都以 BusinessLine 为单位，一条业务线落一个单元不需要改代码；单元内按 Session 与 EndUser 两类键分片 PostgreSQL（ADR-0028、ADR-0051），EndUser 键不加业务线。一个 Session 只属于一个单元，入口按业务线路由，不做跨单元双活。

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
| sandbox-ctl | 沙箱控制器 | 沙箱队列深度 | 单独节点池 + gVisor RuntimeClass（ADR-0010）；只有这个角色持有容器或 K8s API 权限（ADR-0008：受信控制器与不可信沙箱不同 Pod，网关与 Worker 不碰沙箱） |
| janitor | 关闭、删除、保留策略、清理、远期停放迁移 | 固定 1～2 个 | 靠租约互斥；小规模部署可并入 worker-bg |
| Call 中转 | 音频中转、转写、run_task | 并发通话数（每路网关收发约 160KB/s） | 初期留在 gateway；带宽或 CPU 挤占 SSE 时拆成同镜像的独立角色，客户端由 CallStart 的应答指向它 |
| migrate | 应用迁移 | 一次性 Job | 发布流水线中先于滚动更新；含 `CREATE INDEX CONCURRENTLY` 的迁移以 `-- yanshi:no-transaction` 单独执行 |

每个角色只是 `serve` 的一个子集：模拟测试与单进程部署运行全部角色，角色之间不新增任何契约。

## 4. 基础设施

- **PostgreSQL。** 每个分片一个主库加同步备库；已确认的 Event 不能因故障切换丢失或 seq 倒退，所以同步复制是硬要求，旧主须被隔离（fencing）后才允许新主接受写入。连接分两类：SQL 经 PgBouncer 事务模式复用；`LISTEN` 与迁移锁必须直连（事务池不保留会话状态），因此 serve 需要两个 DSN（§6）。故障切换后 `pg.Notifier` 重连、清空缓存并唤醒全部等待者，日志末尾对账补上期间的事件。
- **对象存储。** 任何 S3 兼容实现（ADR-0011）；私有化环境用 SeaweedFS、MinIO 或 Ceph RGW。
- **入口。** 支持 WebSocket 升级与小时级空闲连接；SSE 与 WebSocket 不经过会缓冲响应的中间层。握手限速与带抖动的 `Retry-After` 由网关自身处理。
- **可观测性。** 指标在 peer-addr 的 `/metrics`，只记录 ID（ADR-0015）。链路追踪按 `run_id`、`attempt`、`call_id` 关联，列入上线检查清单。
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

**P4 只支持新建单元，或全部槽位留在库 0 的既有单元。把旧数据迁离库 0 不在 P4 支持范围。** 静态映射无法覆盖旧 Session 之后生成的旧式工件与派生 ID；支持这种迁移需要另行设计在线目录。多库新单元必须关闭旧数据回退且兼容条目为空；既有单元允许使用兼容清单，但全部槽与条目必须仍指向库 0。不能添加一条无关映射绕过启用前提。历史预检与兼容条目的语义仍见 [ADR-0051](../adr/0051-shard-slots-and-id-location.md) §5。

#### 配置与迁移（C5a）

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

文件包含凭据时按 Secret 管理；DSN 和兼容清单不写日志。解析拒绝未知字段、重复字段、重复或缺失分片、遗漏 DSN，并调用 `ValidateActivation`。启动逐库校验系统标识与 database OID：同一分片的 SQL/直连必须是同一物理数据库和 schema，不同分片不能以两个 schema 或两个地址重复装配同一个数据库。连接角色需要 `pg_control_system()` 的 EXECUTE 权限（或 `pg_monitor`），用于这个身份检查；数据库名相同不能证明身份相同。

生产先运行 `yanshi migrate -shards <file>`，再让各角色以 `-migrate=false` 启动；开发可用 `-migrate=true` 逐库迁移。每库使用自己的 SQL 池、直连池与 Notifier；迁移锁只走直连，启动同时检查 SQL 入口可见全部迁移、Notifier 已建立连接。所有角色使用相同配置，全部角色合在一个进程仍可运行。`notify-shards` 只控制每库内部的通知频道数，与数据库分片数无关。

`/readyz` 的 `X-Yanshi-Routing` 返回 `Locator.Identity()` 的摘要（不含 DSN）；发布时核对所有角色实例的摘要相同，不能只比兼容版本标签。C5a 启动和就绪暂时要求全部登记分片可用，**尚未实现 §7 / ADR-0051 §7 的部分就绪**；按分片报告、接流量策略属于 C5b。路由本身不会在访问健康键时查询其他库。

#### 队列、离线操作与验证边界

三类队列公平轮转，各库一次 Claim 尝试默认最多一秒；空库或失败库之后继续尝试，全部为空才返回空队列。租约携带来源分片，Renew / Park / Release 回到原库并复核 Session 定位与来源一致。各库的入队通知合并给等待方，转发随进程上下文取消。Waking 在选定分片内原子写日志、队列与信封；`pg.WithTx` 遇到另一 pool 的路由或 `pg.Exec` 报错，不能静默跨库独立提交。

C5a 的 Offline 路径按分片顺序遍历：Index 时间扫描、删除待办、Inbox 归属恢复、Memory 按来源 Session 删除、Usage 报表与清理、Presence / Ledger / Outbox 清理、到期提升、EndUser 屏障清理。`Promote` 的 limit 是每库批量，返回合计；有界总预算与公平续扫留 C5b。沙箱 ClaimIdle 已有删除候选的副作用，因此每次交回一个分片的一批并轮转，不能在后库失败时丢掉前库已认领的工作。

未指定 EndUser 的 Session 列表按 `(last_input_at, session_id)` 全局倒序合并；现有游标也是每库下一轮的边界，不漏掉未消费的行。Usage 的相同分组数字列求和；匿名 `~` 行留原库并跨库汇总。删除请求只存库 0，读取请求后逐库按 request_id 统计 tombstone，不再 JOIN 请求表。上述完整结果任一库失败即返回错误，不能把缺失部分当零。可重复清理尝试其余分片后仍返回聚合错误。

多库 `Outbox.List/Maintain/Stats` 已遍历；多库流的 `Read/Head` 暂时返回 `outbox_unknown_shard`，不将库 0 的流当作完整流。多分片游标、HTTP 与 xinli 消费者由 C5b 完成。

新增 `TestShardedConsistency` 在内存与两个真实 database 上跑适用存储套件（旧夹具全部留库 0，跨片键另测）；`TestShardedRouting`、队列/故障/事务与启动配置测试覆盖两库边界。`pgtest.Database` 创建随机、可清理 database，测试角色需要 CREATEDB；既有 Docker PostgreSQL 测试角色满足该前提，schema 只用于每库内部的用例隔离。

分片差分采用方案 (a)：模拟队列只读各库头部候选，按优先级、全局逻辑入队/归还顺序选择，并涵盖 Waking 内部入队。生产仍公平轮转。`CompareShards` 仅在新差分种子启用：保留全部 Session 日志、状态指纹，出站列表按 Session/seq/信封 ID 规范化，忽略跨库不存在的全局插入顺序；C5a 直接核对各库全部信封，不测试 C5b 的流游标。普通单库种子不启用该选项、不改变原指纹定义。新增单库内存/双库内存/双库 PG 三方比较，逐库删除不变量，以及 Session 分布、跨片父子与同用户跨片 Session 计数；短 Horizon 另外要求远期停放和提升覆盖。

`make check` 默认包含 3 个双库种子，`make sim` 以 `-sim.shardseeds=100` 增加同量级双库种子。以上改动尚待远端 `dev fmt`、`make check`、`make sim` 验证；没有将静态检查记为测试通过。

C5b 继续完成 fan-out 并发上限、每库与总预算、续扫、完整用量汇总缓存、删除进度的有界失败、按分片就绪与部分可服务、多分片出站及 xinli 消费者、长期故障隔离。P4 仍不做在线迁槽、自动再平衡、旧数据迁出库 0 或网关通知归属。

### 到期表的首次升级与回滚

当前没有真实用户，这次升级采用一次性替换，作为前述通用滚动发布流程的例外，不加兼容开关。旧版本不认识 work_dues，也不使用新版本的事务锁，不能与已启用到期表的新写入者混跑。

1. 先执行 schema 迁移（包含 work_dues 及其并发索引），旧版本此时尚不写到期表。
2. 停止流量和提交任务的入口，停止并排空全部队列写入者，包括 gateway、两类 Worker、Janitor，以及任何直接写队列的进程；确认没有旧写入者仍在运行。
3. 一次性替换为新版本后恢复全部角色与流量，确保 Janitor 同时运行。

回滚前同样停止流量与全部队列写入者，再用新版本的一次性维护进程构造 Session 队列，反复调用 `Queue.Promote`，before 设为覆盖 work_dues 全部 due_at 的远未来（例如全部期限早于 9999 年末时使用该时刻），limit 可用 1000，直到到期表为空；每批检查错误，失败则重试维护，不能启动旧版本。该维护直接调用 Promote，不受 Janitor 的 Sweep 时间预算限制；候选扫描仍只读，每个 Session 独立事务。它会保留类别、ord、ready_at 和精确 parked_until，不能用 Enqueue 代替，否则会提前唤醒。确认 `SELECT count(*) FROM work_dues` 为零后再启动全部旧版本进程；保留新增表和迁移记录，不回退 schema。远期项回到 work_items 后，旧版本仍按 parked_until 执行，但会重新承担百万停放的认领扫描代价。

## 8. 未做

- 通知分片的归属路由（评审 §8.3）；在此之前每个网关进程监听全部分片，单元内的进程数受通知投递上限约束。
- 跨单元的业务线迁移与 Session 导出。
- 多地域部署与灾备切换：Session 数据只在一个单元内，异地只能冷备或按业务线整体切换。

## 9. 单次调用部署参数

单次调用在 gateway HTTP 处理路径中同步执行，经 Core 共享模型、内容安全、配额、计量与删除屏障；不新增进程角色。各进程的声明与发布配置应一致。

| serve 参数 | 默认 | 语义 |
|---|---|---|
| -oneshots | oneshots | 声明目录；不存在或没有声明时不提供单次调用（请求返回 404），跳过单次调用限额校验 |
| -oneshot-concurrency | 8 | 本进程全部单次调用的并发上限，必须为正；model-concurrency 大于 0 时必须严格小于它 |
| -oneshot-wait | 2s | 每一层并发名额独立的等待上限，0 为只尝试立即获取，负值拒绝 |
| -oneshot-deadline | 60s | 从声明校验之前开始的总时限，含审核、排队与一次 Schema 修复；必须为正 |

业务线 YAML 可写 `oneshots: {max_concurrency: 4}`；0 表示只受合计及声明上限限制。三层都是进程内上限，分角色或多副本部署必须按提供商账号容量静态分摊，扩缩容时联动配置。只部署 gateway 也会调用模型，因此应计入持有提供商的进程数。声明模型在配有 Quota 时必须有价格；发布选择每 10 秒热加载，声明、并发及业务线设置修改需重启。
