---
status: accepted（由 ADR-0050 修订：去掉擦除与修订信封）
---

# 业务线经拉取式出站流获取事件

业务线服务端要知道它的 Session 里发生了什么：当天整理完成后归集到项目、把回复转发到微信与飞书、把提问转成推送。现在唯一的途径是按 Session 订阅 SSE，几十万个 Session 意味着几十万条长连接。

## 决定

（设计见 `docs/design/m7-kernel-primitives.md` §6）

- `GET /v1/outbox`：服务令牌，按业务线、按 PostgreSQL 分片的游标长轮询，只返回平台登记的事件种类。
- **只含 ID**（Session、序号、种类、Run、调用），内容按 ID 读事件。出站流不是新的个人数据存储。
- 出站记录与事件在同一事务中写入，记下写入事务的 xid8；游标是 (xid8, id)，只返回低于当前快照 `xmin` 的行。自增序号不是提交顺序，按它推进游标会永久漏掉晚提交的行；水位之下的事务都已结束，游标因此单调不漏。
- 至少一次投递，消费方按 (Session, 序号) 去重，游标由消费方保存。一般的行保留 72 小时、随 Session 删除；删除信封（`session_deleted`、带范围的 `capture_erased`）不随 Session 删除，因为 Janitor 删掉日志与索引之后它们是唯一的线索。游标过期时返回错误，消费方经删除记录与 Session 索引对账。
- 可选的 webhook 只作"有新数据"的提示，不含内容。

## Considered Options

- **每个 Session 一条 SSE**：现状，连接数随 Session 数增长。
- **推送式 webhook 携带事件内容**：消费方简单，但内核要承担重试、退避与投递状态，内容也会离开内核进入不受删除约束的地方。
- **消息队列（Kafka 等）**：新增基础设施组件，私有化部署多一个依赖（章程 §3 不重造、ADR-0001）。

## Consequences

- 新存储 `outbox` 通过一致性套件 `outboxtest`，在 `app.SessionData` 登记。
- 长事务会拖住水位、增加延迟，需要"水位滞后"的指标。
- App 内的实时体验仍走 Connection 与 SSE；出站流服务于服务端的编排与渠道转发。
