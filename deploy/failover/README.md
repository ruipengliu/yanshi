# 隔离同步主备演练

唯一项目为 `yanshi-failover`，主库/备库/健康实例的回环端口分别为 55430/55431/55432。此目录不引用根 Compose 或 `.local` 配置。数据卷可整体删除；不能将项目名改成共享 `yanshi`。

从仓库根目录在具备 Go/Docker 的 Mac mini 执行：

```bash
make failover
make shard-capacity
cat eval-results/failover/kernel.json
cat eval-results/failover/persona.json
cat eval-results/failover/capacity-1.json
cat eval-results/failover/capacity-2.json
make failover-down
```

编码机通过 `dev run '命令'` 执行上述入口；先由用户执行 `dev fmt`。`make failover` 不在 check 中，脚本显式设置开关并逐场景重建隔离集群；没有设置开关时测试跳过。每次运行会删除上次隔离现场，结果文件不会随卷删除；失败后保留最后现场，不自动恢复旧主。禁止并发运行不同工作树的同名项目；脚本锁遗留时先确认没有活跃驱动，再删除对应临时锁目录。

固定代理由 Go 驱动绑定，应用保持原 DSN；管理连接才直连主备。复制使用 `FIRST 1 (failover_standby)` 与 `synchronous_commit=on`。代理关闭 → `pg_promote()` → 停旧主并核实 → 代理改指新主。旧主 `restart: no`，没有回选机制。提升后的新主没有同步备库，只承诺本次提升前已确认提交不丢；生产恢复同步冗余、fencing、epoch 规则及详细断言见 [部署 §10](../../docs/design/deployment.md#10-p4-c6隔离同步主备切换与容量)。

本交付没有运行数值；Mac mini 实跑后填回部署文档的结果表，并执行 [最终回归](../../docs/design/day-acceptance.md#p4-关闭前的最终回归)。容量采用两个独立 database、同一健康 PG 实例；不声称独立机器的线性扩容能力。
