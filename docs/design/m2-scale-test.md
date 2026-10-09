# M2 扩容压测与可观测性

> 状态：首轮完成（2026-10-09）· 依赖：[章程](../charter.md) §8 成功标准

## 1. 要回答的问题

章程 §8 的两条成功标准还没有被测量过：

1. **线性扩容**：Worker 数量翻倍，吞吐近似翻倍，无单点瓶颈。
2. **故障透明**：随机杀掉任意 Worker 或网关实例，进行中的 Run 自动恢复，用户仅感知到短暂延迟。

另外要弄清楚已知的风险点在负载下是否成立：

- 每次认领都读取全部日志（`Store.Load`，[长任务设计](./m2-long-runs.md) §7）；
- 空闲 Worker 每 100ms 轮询一次队列；
- PostgreSQL 的 Event 追加与 `NOTIFY`。

## 2. 方法

不依赖真实模型。

- **`bench` 模型**：按 `bench/latency=200ms,turns=2` 这样的参数，模拟模型延迟（期间流式输出增量），先调用 `turns` 次工具再回复。这样一个 Run 包含 3 次模型调用和 2 次工具调用，接近真实 Agent 的形态。
- **`yanshi bench` 压测客户端**：并发打开 S 个 Session（每个 Session 是一个 EndUser，持有 SSE 连接），在每个 Session 中连续提交 Run，测量吞吐、端到端延迟（提交到收到 `RunCompleted`）和首个增量的延迟。请求轮流发往多个 API 进程，令牌用业务线开发密钥签发（开启鉴权）。
- **`scripts/bench.sh` 编排**：使用全新数据库，启动 P 个 `serve` 进程（每个 W 个 Worker），运行压测，抓取各进程的 `/metrics`。

实验：

| 实验 | 变量 | 观察 |
|---|---|---|
| E1 扩容 | P = 1, 2, 4，W 固定，S 足够让 Worker 饱和 | 吞吐是否 ∝ P×W；瓶颈在哪里 |
| E2 故障 | 稳定负载下 `kill -9` 一个进程 | 所有 Run 是否完成；延迟尖峰有多大 |
| E3 空闲开销 | 无负载，P×W 个 Worker | 数据库每秒查询数 |

## 3. 指标

Prometheus 格式，暴露在内部监听地址（`-peer-addr`）的 `/metrics` 上，不放在对外 API 上。

| 指标 | 说明 |
|---|---|
| `yanshi_model_call_duration_seconds{kind}` | 模型调用耗时；kind = turn / summary |
| `yanshi_model_tokens_total{direction}` | 输入、输出 token |
| `yanshi_runs_finished_total{status}`、`yanshi_run_duration_seconds` | Run 终态与从请求到终态的时长 |
| `yanshi_attempts_total{kind}` | start / takeover / resume |
| `yanshi_commit_conflicts_total` | 乐观并发冲突 |
| `yanshi_session_load_events`、`yanshi_session_load_seconds` | 认领时读取的事件数与耗时 |
| `yanshi_queue_claims_total{result}` | ok / empty / error：空闲轮询的代价 |
| `yanshi_compactions_total` | 上下文压缩 |
| `yanshi_http_requests_total{route,code}`、`yanshi_http_request_duration_seconds{route}` | 对外 API |
| `yanshi_nodes_connected`、`yanshi_live_peer_streams` | 长连接数 |

指标只是观测手段，不影响执行结果；模拟测试中照常累加，无需关注。

## 4. 结果（2026-10-09，Apple M4 10 核；PostgreSQL 16 运行在 Docker Desktop 中）

环境限制：PostgreSQL 的每次往返都要经过 Docker Desktop 的网络转发。因此这里的绝对数值只代表开发机，结论关注的是相对扩展性和每个 Run 的数据库开销。模型延迟为 200ms，每个 Run 包含 3 次模型调用和 2 次工具调用；W=32，S=2×P×W，每组 30 秒、重复 2 次。

### E1 扩容

| P | 优化前（runs/s） | 优化后（runs/s） | 相对 P=1 | 相对理论上限* | p50 / p99 延迟 |
|---|---|---|---|---|---|
| 1 | 37.9 | 50.5 | 1.00× | 95% | 1.25s / 1.39s |
| 2 | 99.6 | 102.6 | 2.03× | 96% | 1.23s / 1.47s |
| 4 | 163.8 | 185.6 | 3.67× | 87% | 1.34s / 1.81s |

\* 理论上限 = P×W ÷ 每个 Run 的模型耗时（0.6s）。

P=4 时，剩余的损失来自数据库往返在开发环境下的延迟：PostgreSQL 约占 2 核，Docker 网络转发约占 1.4 核，而 yanshi 的全部进程合计不到 1.5 核。

### E2 故障

P=4、负载稳定时，`kill -9` 一个同时承担 API 和 Worker 的进程：

- 6812 个 Run 全部完成，没有超时，也没有失败；被杀进程正在执行的 32 个 Run 全部被其他进程接管。
- 吞吐立即降到剩余 3 个进程的水平，中间没有断档；连在被杀进程上的客户端切换到其他进程（重连 64 次，1 次提交需要重试）。
- 被接管的 Run 多花的时间等于租约 TTL：TTL 为 30s 时最长延迟 31.3s；**默认 TTL 改为 10s 后为 11.3s**。

### E3 空闲开销

P=4、W=32、无负载时的数据库查询：**优化前每秒 1218 次，优化后每秒 4.2 次**。

## 5. 据此做的改动

| 发现 | 改动 |
|---|---|
| pgx 默认连接池（max(4, CPU 数)，本机为 10）远小于 Worker 数，94% 的取连接操作需要等待，P=1 只达到上限的 71% | 部署时按 Worker 数配置 `pool_max_conns`（压测中用 20）；见 §6 关于数据库连接总数 |
| 每个 Run 约 48 次"查日志末尾"，因为每个等待者被通知唤醒后都要查一次 | `NOTIFY` 负载带上新的末尾 seq（`sid:seq`）；`Notifier` 缓存本次连接期间收到的值，`WaitAbove` 直接使用，只在缓存缺失或轮询兜底时查询。每个 Run 的查询从 109 次降到 88 次 |
| Worker 每一步都续租 | 距上次续租不足 TTL/3 时跳过（租约必然仍有效） |
| 空闲 Worker 各自每 100ms 轮询，开销与 Worker 总数成正比 | `runtime.IdleGate`：同一进程内同一时刻最多一个空闲 Worker 轮询；入队时发 `NOTIFY`（`workqueue.Signaler`）立即唤醒，否则以 50ms～1s 退避轮询 |
| 进程崩溃后，Run 要等满 30s 租约才被接管 | 默认 `-lease-ttl 10s`，心跳为 TTL/3 |

## 6. 遗留问题

- **数据库连接总数**：进程数 × 连接池大小不能超过 PostgreSQL 的 `max_connections`（默认 100）。规模扩大后需要在 PostgreSQL 前加连接池代理（PgBouncer 的事务模式），而 `LISTEN` 连接需要直连。这件事等确定部署形态时一起设计。
- ~~每个 Run 约 37 次读日志~~：已解决（2026-10-09）。SSE 直接告知增量总线当前执行进程（`live.Follower`），不再重复读日志；Worker 在通知值显示没有新事件时跳过 `Sync`（`eventlog.HeadHinter`）。复测：每个 Run 的查询 87.5 → 77.1 次，其中读日志 37 → 20.6 次；4 进程吞吐 188.8 runs/s。
- ~~`Store.Load` 读取全部日志~~：已引入投影快照（`session.Snapshots`，每 200 个事件写一次，带投影版本号）。加载时只回放快照之后的事件，长 Session 的加载成本不再随历史增长；压测中的 Session 较短（约 250 个事件），收益不明显。
- ~~沙箱控制器的空闲轮询还没有接入门控~~：已接入（`workqueue.IdleGate`）。
- 生产规模的绝对容量需要在目标部署环境（独立的 PostgreSQL、真实网络）中重新测量。
