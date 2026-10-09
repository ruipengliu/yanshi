---
status: accepted
---

# 实时增量从执行进程直接拉取

多进程部署下，token 增量由订阅方直接从执行该 Attempt 的进程拉取；执行进程的内部地址记录在 `AttemptStarted.live_endpoint` 中。我们没有选择以下两种方案：

- **PostgreSQL `NOTIFY`**：`NOTIFY` 在提交时要获取一把全局锁，所有发出通知的事务（包括每次 Event 追加）都会在这里串行化。而且每个监听进程都会收到全部 Session 的通知，开销随"增量速率 × 进程数"增长，违背线性扩容。
- **引入消息中间件**：私有化部署会多一个必须运维的组件（ADR-0001、ADR-0007）。

## Consequences

- 不新增存储或服务注册表；"谁在产生增量"由日志决定，与 fencing 使用同一事实源。
- 进程之间需要能互相访问内部端口（`-peer-addr`）。K8s 下用 Pod IP 即可。
- 对外 API 不暴露 `live_endpoint`。
- 增量仍然是尽力而为的：执行进程崩溃到被接管之间，没有增量。
