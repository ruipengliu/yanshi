# 生产部署形态：一个二进制、按角色分组的模块化单体

> 状态：设计（2026-10-11）；§6 的角色开关、两个 DSN、就绪检查与队列指标已实现，部署清单已写、尚未在集群上验证 · 来源：[延展性评审](../review/2026-10-11-scalability-review.md) §8、§9 · 关联：[章程](../charter.md) §4、§5 原则 9，ADR-0001（私有化）、ADR-0008（沙箱信任边界）、ADR-0013（跨进程增量）、ADR-0028（分片键）、ADR-0029（工作类别与移交）· 术语以 [CONTEXT.md](../../CONTEXT.md) 为准
>
> 本文回答两个问题：生产环境怎么部署；进程边界按什么划。实施前须完成 §6 的代码改动，上线前逐项核对 [上线检查清单](../launch-checklist.md)。

## 1. 原则

- **进程边界沿扩缩容维度、权限与故障域划分；包边界沿领域划分。** 同一个镜像、同一套包，按角色裁剪后以不同的 Deployment 运行。不按领域拆服务（Session、Memory、用量……），理由见 §5。
- **单进程形态长期保留**（ADR-0001、章程原则 9）：开发、模拟测试与小规模私有化部署都运行全部角色；生产形态只是把角色分到不同进程，执行语义不变，`internal/sim` 仍然覆盖它。
- **一条业务线一个单元（cell）。** Token、配额、AgentDef、内容安全、保留策略都以 BusinessLine 为单位，一条业务线落一个单元不需要改代码；单元内再按 Session 分片 PostgreSQL（ADR-0028）。一个 Session 只属于一个单元，入口按业务线路由，不做跨单元双活。

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
| janitor | 关闭、删除、保留策略、清理 | 固定 1～2 个 | 靠租约互斥；小规模部署可并入 worker-bg |
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
| 角色开关（已实现） | `serve -roles gateway,worker,sandbox,janitor`，默认全部；没有 gateway 的进程不监听 `-addr`，存活与就绪检查在 `-peer-addr` 上；没有 worker 时 Worker 数为 0；没有 sandbox 时沙箱工具照常登记、调用由其他进程的控制器执行。存储由装配包 `internal/app` 构造（评审 §9.1） | 任一角色组合都能启动；全部角色等价于原来的 serve；已验证"只有 gateway"与"只有 worker、janitor"的两个进程共用 PostgreSQL 完成 Run |
| 两个 DSN（已实现） | `-pg-dsn` 可经 PgBouncer 事务池；`-pg-listen-dsn`（`YANSHI_PG_LISTEN_DSN`）直连，用于 `LISTEN` 与迁移锁（`pg.OpenDirect`，小连接池）；为空时共用 `-pg-dsn` | 经事务池的部署中通知正常送达；故障切换后 LISTEN 在约定 RTO 内恢复（心跳 10 秒、超时 10 秒后重建，见 [M2 扩容压测](./m2-scale-test.md) §8） |
| 就绪探针（已实现） | `/healthz` 只返回 200；`/readyz`：数据库可达、本进程的 LISTEN 连接已建立（`pg.Notifier.Ready`）、迁移已全部应用（每分钟复查） | 滚动发布时流量不会打到尚未监听通知的 Pod |
| 部署清单（已写，未在集群上验证） | Kustomize：`deploy/k8s/base`（每个角色一个 Deployment、migrate Job、网关的 Service 与 PDB、网络策略、各角色共用的探针与配置挂载），`deploy/k8s/overlays/example`（以仓库中的开发配置生成 ConfigMap，密钥为占位值）；镜像 `deploy/Dockerfile`。交互池与后台池是两个 Deployment（`-worker-pool interactive` 与 `any`） | 清单可审查（`kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/k8s/overlays/example`）；一套制品在无外网 K8s 完成部署（章程 §8），须在目标环境验证 |

## 7. 容量与扩缩

- 扩缩信号：模型准入等待（`yanshi_model_admission_wait_seconds`）、连接数（`yanshi_nodes_connected`、`yanshi_live_peer_streams`）、握手拒绝（`yanshi_handshakes_rejected_total`）、认领结果（`yanshi_queue_claims_total`）。队列等待与积压：`yanshi_queue_wait_seconds{queue, priority}`（认领时观察，工作变为可认领到被认领的时长），`yanshi_queue_ready{queue, business_line, priority}` 与 `yanshi_queue_oldest_wait_seconds`（Janitor 所在进程每 15 秒统计）。它们也是 Submit 准入阈值的依据（`-admission-max-wait`，评审 §8.4 第 5 项）。
- 单元容量上限由 PostgreSQL 分片数与通知投递次数决定（评审 §8.3）；容量矩阵与准入阈值由评审 §8.4 第 4 项的压测给出，在目标环境实测后填入部署清单。
- 模型并发是全局约束：`-model-concurrency`、`-model-rps` 按账号上限除以持有该提供商的进程数配置（[模型调用](./model-calls.md) §4）；弹性扩容时须随副本数联动，或实现共享令牌服务。

## 8. 未做

- 通知分片的归属路由（评审 §8.3）；在此之前每个网关进程监听全部分片，单元内的进程数受通知投递上限约束。
- 跨单元的业务线迁移与 Session 导出。
- 多地域部署与灾备切换：Session 数据只在一个单元内，异地只能冷备或按业务线整体切换。
