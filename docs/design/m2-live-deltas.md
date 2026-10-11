# M2 跨进程实时增量

> 状态：已实现 · 依赖：[M0 核心原语](./m0-core-primitives.md) §5 · ADR-0013 · 契约：`AttemptStarted.live_endpoint`

## 1. 问题

模型的 token 增量不进日志，只经 Session 级的 `live.Bus` 扇出（M0 §5）。目前 `live.Bus` 只在进程内生效。多个进程共享 PostgreSQL 部署时，如果执行 Run 的 Worker 在进程 A、客户端的 SSE 连在进程 B，客户端只能看到已提交的完整消息，看不到逐字输出。在负载均衡下，这几乎是必然发生的情况。

语义不变：增量是易失、尽力而为的，带有 `run_id` 与 `attempt`。丢失的增量由随后提交的完整事件覆盖。

## 2. 方案：从执行进程直接拉取

```
客户端 ──SSE──▶ 进程 B（httpapi.stream）
                 ├─ 已提交事件：读日志（不变）
                 └─ 增量：peer.Bus.Subscribe
                       ├─ 本进程 MemBus
                       └─ 跟随日志：当前 Attempt 的 live_endpoint = A → GET A/internal/v1/live/{session}
进程 A：Worker ──Publish──▶ 本进程 MemBus ──▶ peer.Handler（逐行 JSON）
```

- **地址记在 Attempt 上。** Worker 开始 Attempt 时，在 `AttemptStarted.live_endpoint` 中写入本进程的内部地址。一个 Attempt 只存在于一个进程的生命周期内，所以这个地址准确指明了"此刻谁在产生这个 Run 的增量"。不需要额外的服务注册表或心跳。单进程内存模式下该字段为空。
- **订阅方跟随日志。** 订阅者读取该 Session 的日志，找到活跃 Run 的最新 Attempt：
  - Run 处于 running 状态、地址非空、且不是本进程时，连接该地址拉取增量；
  - 出现新的 Attempt（接管或恢复）时，切换到新地址；
  - Run 挂起或进入终态时，断开连接。
  - 连接断开但 Attempt 没变时，按退避间隔重连。执行进程崩溃后，在接管者的 `AttemptStarted` 出现之前，增量是中断的。
- **内部接口与对外 API 分开监听。** 内部接口在独立的 `-peer-addr` 上监听，可以配置共享令牌（`YANSHI_PEER_TOKEN`）。对外 API 输出事件时会清除 `live_endpoint`，不向客户端暴露内部地址。

## 3. 扩展性

- 流量只在真正存在"生产者 → 消费者"关系的进程之间流动：每个远程 SSE 订阅对应一条进程间连接，与进程总数无关。
- 不经过数据库：PostgreSQL 的写入路径（Event 追加）不承担 token 级流量。
- 同一进程上同一 Session 有多个订阅者（例如两台设备、HTTP 与 Connection 各一个）时，共用一条上游连接，最后一个订阅者取消时断开（2026-10-11，[延展性评审](../review/2026-10-11-scalability-review.md) §4.5）。

## 4. 暂不做

- ~~**晚加入者补齐**~~：已实现，见[多端双工通道](./m3-duplex-channel.md) §4。
- **消息中间件实现**：`live.Bus` 是接口。将来如果私有化环境里已有 NATS 之类的组件，可以再提供一个实现。

## 5. 测试

- `peer` 包：两个"进程"共享同一个内存日志，验证：增量能跨进程送达；Attempt 切换到其他进程后，订阅跟着切换；挂起后停止拉取；令牌校验生效。
- e2e：`TestCrossInstanceOnPostgres` 中，API 实例本身没有 Worker，验证它的 SSE 能收到执行实例产生的增量。
