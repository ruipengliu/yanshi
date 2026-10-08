---
status: accepted
---

# 以 PostgreSQL 作为持久化存储基线

Event 日志、工作队列、Node 目录与 Inbox 的持久化实现统一基于 PostgreSQL：日志表以 `(session_id, seq)` 为主键，唯一约束天然实现乐观并发追加；工作队列用 `FOR UPDATE SKIP LOCKED` 认领；`LISTEN/NOTIFY` 实现 `Wait`，并以轮询兜底。选择它是因为私有化部署（ADR-0001）要求依赖少、客户 IDC 普遍具备运维能力，且一个组件即可覆盖全部 M1 存储接口。

## Considered Options

- **Kafka / Pulsar 作日志**：顺序追加天然，但乐观并发追加（expectedSeq）与按 Session 随机读取不便，且仍需另一个库存放队列与目录。
- **FoundationDB**：事务与扩展性最佳，但私有化运维门槛高、生态小。
- **自研日志存储**：时机未到。

## Consequences

- 单个 PostgreSQL 主库是写入上限。线性扩容靠按 `session_id` 分片到多个 PostgreSQL 集群（Session 之间无跨分片事务），或替换为 PG 协议兼容的分布式数据库；业务代码只依赖 `eventlog.Log` 等接口，不感知分片。
- 单二进制开发模式继续使用内存实现；所有实现必须通过同一套一致性测试，并通过确定性模拟的差分测试（同一种子在两种实现上产生逐字节相同的日志）。
- 实时增量（LiveBus）仍是进程内的：多进程部署时，token 增量只送达连在同一进程的客户端，已提交事件不受影响。跨进程增量分发留待引入消息总线时解决。
- 开发与测试通过 `docker compose`（`make deps-up`）启动 PostgreSQL。
