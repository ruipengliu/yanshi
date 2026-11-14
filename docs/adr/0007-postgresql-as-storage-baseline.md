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

- 单个 PostgreSQL 主库是写入上限。线性扩容靠按 `session_id` 分片到多个 PostgreSQL 集群（Session 之间无跨分片事务；现按 Session 与 EndUser 两类键分片，见 ADR-0028、ADR-0051，2026-11-14 补注），或替换为 PG 协议兼容的分布式数据库；业务代码只依赖 `eventlog.Log` 等接口，不感知分片。
- 单二进制开发模式继续使用内存实现；所有实现必须通过同一套一致性测试，并通过确定性模拟的差分测试（同一种子在两种实现上产生逐字节相同的日志）。
- 跨进程的实时增量已由 [ADR-0013](0013-live-deltas-pulled-from-executing-process.md) 解决：订阅方直接从执行进程拉取（`AttemptStarted.live_endpoint`），没有引入消息总线。（2026-11-14 修订：原写"实时增量（LiveBus）仍是进程内的……跨进程增量分发留待引入消息总线时解决"。）
- 开发与测试通过 `docker compose`（`make deps-up`）启动 PostgreSQL。

## 重审条件

（2026-11-14 补充）上文已写明两条出路：分片（已实现，ADR-0028、ADR-0051）与换用 PG 协议兼容的分布式数据库；自研日志存储"时机未到"。任一出现即重新评估：

- 单个分片的写入（Event 追加、WAL 速率）接近扩容压测（`docs/design/m2-scale-test.md`）测得上限的 70%，且增加分片无法摊开（例如热点槽位）；
- 停写迁移槽位的停机时间超出预算（与 ADR-0051 一并重审）；
- 私有化客户的环境无法运维 PostgreSQL 主备。
