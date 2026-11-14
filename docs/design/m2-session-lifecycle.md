# Session 生命周期与数据删除

> 状态：已实现 · 依赖：[M0 核心原语](./m0-core-primitives.md)、[沙箱](./m2-sandbox.md)、[工件](./m2-artifacts.md)、[鉴权](./auth.md) · ADR-0015
>
> 2026-11-14：ADR-0036、ADR-0039、ADR-0050 与 ADR-0037 的内核部分已整合进 [ADR-0052](../adr/0052-attested-results-and-provenance.md)，ADR-0040 已取消，ADR-0010 已被 [ADR-0053](../adr/0053-self-hosted-sandbox-isolation-baseline.md) 取代。本文引用这些 ADR 之处，现行规则以 ADR-0052、ADR-0053 为准（[ADR 索引](../adr/README.md)）。

## 1. 要解决的问题

1. **数据只增不减。** 事件日志、工件、沙箱工作区都没有清理路径。沙箱设计 §4 和工件设计 §5 都把这件事留给了"Session 关闭语义"。
2. **C 端合规。** EndUser 删除一段对话、注销账号，或者数据超过保留期时，平台要能把该 EndUser 的数据从所有存储中删干净，并且能证明已经删除。
3. **按用户查找 Session。** 现在 Session 只以日志的形式存在，无法列出"某个 EndUser 的全部 Session"。注销账号需要它，App 的"我的对话"列表也需要它。

## 2. 状态

```
   active ──close──▶ closed ──delete──▶ deleting ──▶ deleted（只剩删除记录）
     │                                     ▲
     └───────────────delete────────────────┘
```

| 状态 | 含义 | 资源 |
|---|---|---|
| active | 可以接收输入；空闲时沙箱计算资源照常回收，工作区保留（现有行为） | 全部保留 |
| closed | 不再接收输入；对话和工件仍然可以查看、下载 | 沙箱工作区销毁 |
| deleting | 对外不可见（一律 404），后台正在清理 | 逐项删除 |
| deleted | 数据全部删除，只保留不含个人信息的删除记录 | 无 |

- **关闭不可逆。** "继续之前的话题"就新建一个 Session（以后可以带上摘要或 Memory）。这样关闭后的 Session 不需要再考虑并发写入，沙箱工作区也可以放心销毁。
- 关闭时如果有活跃的 Run，先中断它。
- `SessionClosed{by, reason}` 是一个新的 Event，属于 Session 的终态事件：之后不能再有任何 Run 事件（由 `Reduce` 校验）。

有有效定时的 Session 不按空闲关闭（[M7 §5.4](./m7-kernel-primitives.md#54-语义)）。索引以 `lifecycle.Session.NextDue` 记录最早到期，`IdleBefore` 排除非零值；定时事件写日志后更新，不受 TouchEvery 节流。Janitor 关闭前读投影修正遗漏，service 在关闭的乐观并发决策中再次检查。反向残留由 `Index.ScheduledAfter` 按 Session ID 分页校准（每业务线每轮至多 100 行，游标跨轮前进并回绕），避免固定前一批有效定时挡住后续 Session。delete_at、主动关闭与用户删除照常生效。

## 3. 删除

### 3.1 物理删除

删除就是从各个存储中物理删除数据，包括事件日志。这与"日志只追加"（ADR-0004）不矛盾：只追加约束的是 Session 存续期间的写入；删除的是整个 Session，对它的全部读写都随之结束。

备份中的副本不会立即消失。备份保留期就是删除完全生效的上限，部署文档中需要写明，例如"删除在 30 天内传递到备份"。ADR-0015 记录了另一种可选方案：加密销毁（crypto-shredding）。

### 3.2 删除记录与可见性

`session_tombstones(session_id, business_line, requested_at, completed_at, reason)`：

- 记录写入即生效。之后对该 Session 的普通 API 请求都返回 404（仅服务令牌可读删除进度），所有组件都不能再为它创建数据。
- 写入记录的同一条语句发出 `NOTIFY yanshi_deletions`（负载为 Session ID）。订阅中的事件流等这个通知结束自己（`lifecycle.Watcher`），不再每个保活周期查一次；兜底由 `pg.Notifier` 统一的限速校准承担（见 [M2 扩容压测](./m2-scale-test.md) §8）。订阅者多时，周期查询本身就是可观的负载（[延展性评审](../review/2026-10-11-scalability-review.md) §4.3）。
- **Session 删除记录中不保存 EndUser。** 保留的记录只用于证明"某个 Session 已于某时被删除"。按 EndUser 注销的屏障在删除执行期与完成后的宽限期内保存 EndUser，之后由 Janitor 清除迟到数据再删除（§3.4）。

### 3.3 清理过程

清理由后台 Janitor 完成。它沿用工作队列（`workqueue.Queue`，新增一张 `janitor_items` 表）的租约语义：任何进程都可以执行，崩溃后由其他进程接管。每一步都是幂等的：

1. 中断活跃的 Run，撤回各 Node Inbox 中属于该 Session 的调用；
2. 从工作队列中移除该 Session（PostgreSQL 的 work_items 与远期停放表 work_dues 由 Remove 在同一事务中清除，登记在 `app.SessionData` 的 work_queue），沙箱队列中移除它的沙箱；
3. 销毁沙箱（容器与工作区卷），删除去重账本和活跃记录；
4. 删除工件（先删内容，再删元数据）、由该 Session 写入的 Memory 与在场记录（[多端双工通道](./m3-duplex-channel.md) §6）；
5. 删除事件日志与快照，再扫一遍第 1～4 步，最后删除 Session 索引（保留 EndUser 供两遍 Inbox 清扫，ADR-0051）；
6. 写入 `completed_at`。

**防止复活**：删除期间可能还有正在进行的写入，例如 Worker 正在追加事件、网关正在写回设备结果、工件正在上传、沙箱控制器正在执行。这些写入的入口都要检查删除记录：

- `Service`、`Hub.Result`、工件上传、沙箱 `Ensure` 在写入前检查删除记录；`presence.Service.Put` 在写入后复查；
- Janitor 在删除日志后、删除索引前**再扫一遍**第 1～4 步，清理检查与删除之间漏进来的写入。索引已删后的重试，由 Inbox 离线扫描恢复 EndUser、逐用户撤回；删除记录不保存 EndUser。
- 写入方的顺序是"先写、后查"：写入后查到删除记录，就撤回自己刚写的数据；Janitor 的顺序是"先记、后清"。两者无论如何交错，总有一方能看到另一方。写入方一律经 `lifecycle.Recheck(ctx, deletions, sessionID, withdraw)` 复查与撤回，写法一致、可以 grep。
- **登记制**（[延展性评审](../review/2026-10-11-scalability-review.md) §9.1）：第 1～4 步清除的各处数据登记在 `app.SessionData` 中（`lifecycle.Holder`：名称与按 Session 删除的函数），Janitor 按登记顺序逐一清除，不再逐个依赖各存储。新增一处存放 Session 数据的地方时：在 `app.SessionData` 登记、写入入口经 `lifecycle.Recheck` 复查、在 `internal/sim` 的删除不变量中检查。
- **关闭也受同样的保护**：沙箱控制器创建（或恢复）沙箱之后，会复查 Session 是否已关闭或删除，是则销毁（`lifecycle.Guard`）。关闭方先在索引中记下关闭，再让 Janitor 入队，因此工作区不会残留。这一条是模拟测试发现的：关闭时控制器可能已经读到了待执行的调用。
- **日志被删除时的冲突**：持有租约的 Worker 或网关追加事件时，如果日志已被删除，追加会冲突，同步后却读不到新事件。`session.Store.Update` 在冲突后同步时把这种情况识别为 `ErrGone`，调用方放弃而不是重试（同样由模拟测试发现）。Worker 放弃时还必须**丢弃全部本地状态**：否则本地投影仍认为某个调用"已由我开始、尚无结果"，而按需续约可能还没发现租约已被移除，下一步就会再次执行非幂等调用（模拟测试在 2000 个种子中发现了这种情况）。

### 3.4 按 EndUser 删除（注销账号）

`DELETE /v1/end_users/{id}` 只接受服务令牌，因为注销由业务线发起。它会：

1. 先写按业务线隔离、按 EndUser 分片的删除屏障，再通过 Session 索引找到全部 Session，逐个写入 Session 删除记录；
2. 删除该 EndUser 的 Node 登记；业务线须先让该用户的令牌失效，屏障用于拦截已在途的写入与重连登记；
3. 删除该 EndUser 的 Memory 与 Grant（M4）、推送设备登记（[多端双工通道](./m3-duplex-channel.md) §9），并匿名化用量（ADR-0019）；删除档位分配 `end_user_tiers`（[配额与用量](./m4-quota-usage.md)）。档位由 `Service.DeleteEndUser` 固定清理，清理失败不封口，不依赖调用方传入 remover。

所有按 EndUser 写入的入口先写、后查屏障，查到后撤回本次写入并返回不存在（计量匿名化）。新 Session 写入删除记录，交给 Janitor 清理。屏障在全部清除完成后再保留一个宽限期，默认 24 小时，可经 `app.Options.EndUserGrace` 配置；Janitor 在删除每条到期屏障前，经 `PruneEndUsers` 的清理回调再次清除档位分配（`Core.EndUserData`），兜住宽限期内落盘、复查前崩溃的写入；清除失败保留屏障，下一轮重试。清理与屏障删除按单个 EndUser 串行，PG 使用同一事务。屏障移除后，同一 EndUser ID 可以重新使用。删除失败或未完成时不开始倒计时，重复注销复用原屏障与删除请求。

### 3.5 设备端数据

设备 SDK 的去重账本保存了调用结果，可能包含个人数据。设备可能离线，服务端无法保证立即清除，因此：

- SDK 定期清理账本中超过保留期（默认 7 天）的记录；
- 删除 Session 时，Janitor 向曾执行过该 Session 调用的设备投递一个**保留能力名的调用**（`__forget_session`，目标 Session 写在参数中），设备 SDK 收到后清除对应记录。这样复用了 Inbox 的离线投递、重投与确认，不需要新增协议消息；设备离线时，会在上线后收到。
  - 清除时仍在执行的该 Session 的调用：SDK 先记下清除标记、取消它们，再清除；它们之后写下的记录在写入后复查标记并删除（"先写、后查"），之后到达的同一 Session 的调用不执行。标记只在进程内：进程退出时执行中的调用随之结束，不会再写入。
- 账号注销时 Node 登记已被删除，Janitor 不再向这些设备发送清除请求：清除本地数据由业务 App 在注销登出时自行完成。

## 4. Session 索引

`sessions(session_id, business_line, end_user, agent, created_at, last_input_at, closed_at, delete_at)`：

- 这是日志的**投影**，可以由日志重建，不是事实源（ADR-0004）。
- 在创建、提交输入、关闭时写入。距上一个 Run 不到一分钟（`lifecycle.TouchEvery`）的输入不更新最后输入时间，连续对话因此不必每个 Run 都写一次索引。代价是最后输入时间至多滞后一分钟：列表排序可能差这么多，按空闲时长关闭的保留策略多等一分钟。
- 用途：`GET /v1/sessions` 列出调用方自己的 Session（分页）、按 EndUser 删除、按保留策略查找过期的 Session。

Index 的 Get / Touch / SetNextDue / MarkClosed / Delete 显式带真实 EndUser，PG 按 `(end_user, session_id)` 等值访问。仅未知归属的删除恢复路径使用 LookupSession 读真实索引行；C4/C5 按 Session ID 的 EndUser 槽位定位唯一库，槽位不作授权。调用点与兼容约束见 [ADR-0051 §8](../adr/0051-shard-slots-and-id-location.md#8-c1--c2--c3a--c3b-交付边界与后续生成点)。

## 5. 保留策略

在业务线配置（`businesslines/*.yaml`）中声明：

```yaml
retention:
  close_after_idle: 720h      # 最后一次输入后多久自动关闭；0 = 不自动关闭
  delete_after_close: 4320h   # 关闭后多久自动删除；0 = 不自动删除
  agents:                     # 可选：按 AgentDef 名称覆盖（整条替换，不与上面的值合并）
    xinli-day:
      close_after_idle: 36h
      delete_after_close: 168h
```

按 AgentDef 覆盖使短期保留的 Session（如旁听，[旁听](./captures.md) §8）与长期保留的 Session 并存于同一业务线：Janitor 先按每个覆盖查询该 AgentDef 的 Session，再以业务线的值查询其余 AgentDef 的（`lifecycle.Retention.Rules`）。按保留期删除的原因是 `retention`，此时 Session 写入的 Memory 保留（ADR-0037，[Memory](./m4-memory-grant.md) §7）。

由 Janitor 周期性地按 Session 索引扫描执行。平台不设默认值：保留多久是业务线的合规决策。

### 5.1 绝对删除期限（ADR-0050）

`POST /v1/sessions` 可带 RFC3339 格式的 `delete_at`，只有服务令牌可以设置；用户令牌带此字段返回 403。非空期限必须晚于 `service.Service` 使用的时钟当前时间，否则返回 400。未设置时没有绝对期限。内核不推算业务日，擦除后重建时由业务线传回原来的期限。

期限同时记入 `SessionCreated.delete_at` 与 Session 索引，投影通过 `State.Created` 读取。委托派生的子 Session 从父日志继承期限，不延长原文副本的寿命。Janitor 在保留策略扫描中通过 `lifecycle.Index.DueBefore` 扫描期限早于当前时间的 Session；即使业务线没有配置 Retention，也会扫描。到期以 `retention` 删除，保留既有 Memory。与保留策略取较早者，期限不要求 Session 已关闭；实际清理由周期扫描与清理队列推进。

### 5.2 Erasure 与删除进度（ADR-0050）

`DELETE /v1/sessions/{id}?reason=erasure` 只接受服务令牌，用户令牌返回 403，其他显式 reason 值（含空值、多值）返回 400。不带 reason 时保持用户 `end_user`、服务 `service` 的删除行为。

`lifecycle.ReasonErasure` 删除 Session 日志与各处副本，保留删除记录之前写入的 Memory；委托派生的子 Session 同因删除，包含创建子 Session 与父删除交错的路径。登记字段 `Holder.KeepOnRetention` 保留原名，语义扩展为 retention 与 erasure 都跳过既有 Memory；普通删除与注销账号仍清除 Memory。`lifecycle.Recheck` 不按原因豁免：新 Memory 写入后查到任何删除记录，都按本次 ID 撤回，不能因保留既有 Memory 而让迟到写入留下。

`GET /v1/sessions/{id}/deletion` 只读 `Tombstone`，只接受服务令牌，并按删除记录的 BusinessLine 授权。返回 `reason`、`requested_at`，完成后另有 `completed_at`（RFC3339）；未完成时省略后者。没有删除记录或属于其他业务线时返回 404。该接口不加载 Session，因此日志清除后仍可读；开发模式须显式给出 `business_line`。完成标记沿用单个 Session 的清理语义，子 Session 各有自己的删除记录。

## 6. API

| 接口 | 权限 | 说明 |
|---|---|---|
| `GET /v1/sessions?cursor=&limit=` | 用户：自己的；服务：本业务线（可按 `end_user` 过滤） | 按最近输入时间倒序 |
| `GET /v1/sessions?order=session_id&end_user=&cursor=&limit=` | 沿用列表授权；必须确定一个 EndUser | 恢复用，按不可变 Session ID 升序，包含已关闭项 |
| `POST /v1/sessions/{id}/close` | 拥有该 Session 的调用方 | 幂等 |
| `DELETE /v1/sessions/{id}` | 同上；`reason=erasure` 仅服务令牌 | 202，异步删除；之后普通访问返回 404 |
| `GET /v1/sessions/{id}/deletion` | 仅服务令牌，本业务线 | 单个 Session 的原因、请求时间与完成时间 |
| `DELETE /v1/end_users/{id}` | 仅服务令牌 | 202，返回删除请求 ID |
| `GET /v1/deletions/{id}` | 仅服务令牌 | 删除进度（Session 数与已完成数），供业务线向用户证明已删除 |

## 7. 测试

恢复分页（Q117）由 `lifecycle.Index.ListStable` 提供。`cursor` 是上一页最后一个 Session ID（不含），响应仍为 `sessions` 与 `next_cursor`，没有更多时后者为空。省略 `order` 或使用 `order=recent` 保持产品排序，未知值返回 400；limit 沿用默认 50、上限 200。PG 的比较与索引使用 `COLLATE "C"`，与内存的字符串顺序一致；索引由独立并发迁移 `0031_sessions_stable_order.sql` 创建。

Touch 和关闭不会改变扫描位置。逻辑删除之后、Janitor 清索引之前仍可能列出，创建失败也可能留下空索引，读者须逐 ID 读取确认可访问性。分页不是并发快照：新建 ID 落在游标之前、或项在后页读取前已清除时，单轮列表不会包含它；应用通过先取 H、后追出站和已知项逐 ID 确认收敛，不能把未列出直接当删除（[M7](./m7-kernel-primitives.md) §6.5）。

- 一致性套件：Session 索引与删除记录（`lifecycletest`）的内存与 PG 实现。
- **模拟测试**新增"删除"动作，随机发生在 Run 进行中、设备执行中、沙箱执行中，以及 Janitor 崩溃时。新增不变量：
  - 删除完成后，日志与各 Holder 不再有该 Session 的数据；retention 与 erasure 保留既有 Memory，迟到的新 Memory 撤回；
  - 删除记录写入后，不会再出现该 Session 的新事件、工件或沙箱。
- e2e：注销一个同时有设备 Node、沙箱工作区和工件的用户，验证数据全部删除，Docker 卷也已移除。

## 8. 不包括

- Memory 的删除与 Grant 撤销（M4，届时接入 §3.4）。
- 导出个人数据（数据可携带权）：Session 索引与日志已经足够实现，可以单独排期。
- 日志与指标中只有 ID，没有内容；这一点作为约定写进 AGENTS.md。
