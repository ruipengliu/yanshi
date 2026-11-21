# M2 云端代码沙箱

> 状态：S1 已实现 · 依赖：[M1 设计](./m1-device-nodes.md) · ADR-0008、0009、0053（取代 0010）

## 1. 定位与范围

S1 的定位是"代码解释器"：Agent 编写并运行 Python 来处理数据、计算、出图，单次执行最长分钟级。后续阶段：S2 工件存储与设备↔沙箱文件传递；S3 快照与预热池；S4 生产 Provider（2026-11-21 已选型：K8s，隔离运行时经 RuntimeClass，默认 gVisor，见 ADR-0053 的选型结论与本文 §7）。

## 2. 沙箱即 Node

每个 Session 的沙箱是一个虚拟 Node，ID 为 `sbx_<session_id>`。对模型暴露的工具名是 `sandbox__<capability>`，由 AgentDef 白名单中的 `sandbox:<glob>` 启用。

| 能力 | 幂等 | 说明 |
|---|---|---|
| `run_python` | 否 | 执行一段 Python 代码 |
| `exec` | 否 | 在 `/workspace` 中执行 shell 命令 |
| `write_file` | 是 | 写工作区文件 |
| `read_file` | 是 | 读工作区文本文件 |

在隔离沙箱内执行默认**不需要审批**（ADR-0008：破坏面被限制在沙箱内）。stdout/stderr 各截断到 16KB 返回给模型；完整输出的保存属于 S2。

## 3. 派发与执行

```
Worker ──Dispatch(sbx_X)──▶ Inbox[sbx_X] + 沙箱队列.Enqueue(sbx_X)
                                   │
沙箱控制器（多实例）──Claim(租约)──┘
   └─ 逐个处理 Inbox[sbx_X]：确保沙箱存在 → Executor（call_id 去重，共享账本）→ Hub.Result
```

- **沙箱队列**复用 `workqueue.Queue` 的语义（租约、唤醒、停放），只是键是沙箱 Node ID。持有租约的控制器独占该沙箱，同一沙箱内的调用串行执行。
- **去重账本**放在共享存储中（`sandbox.Ledger` 的内存 / PG 实现，按真实 Session 与 call_id 去重，再绑定 Session 适配给 `nodesdk.Ledger`）。控制器在执行途中崩溃后，接管者看到"执行中"：`exec` / `run_python` 返回"结果未知"，读写文件则重做。
- **中断**会撤回 Inbox 条目；执行中的控制器监视 Inbox，发现条目被撤回时取消执行。
- 执行期间控制器持续续租。

## 4. 生命周期

- **创建**：首次执行时 `Provider.Ensure` 创建沙箱（幂等）。
- **活跃记录**：每次执行开始和结束都记入 `Activity` 存储（共享）。
- **回收**：任一控制器实例周期性检查空闲超过 `IdleTTL` 的沙箱，停止其计算资源，保留工作区。下次执行时 `Ensure` 重新创建并挂回原工作区。
- **销毁**：Session 关闭或删除后，由 Janitor 销毁沙箱及其工作区（[Session 生命周期](./m2-session-lifecycle.md)）。

## 5. Provider 接口与 Docker 实现

`sandbox.Provider` 提供 `Ensure / Exec / WriteFile / ReadFile / Stop / Destroy` 等操作。Docker 实现的安全配置如下：

- 默认 `--network none`；
- `--cap-drop ALL`、`no-new-privileges`、以非 root 用户运行；
- 只读根文件系统 + `/tmp` tmpfs；
- 限制 CPU、内存、进程数；
- 可选 `--runtime runsc`（gVisor，`serve` 与 `eval` 的 `-sandbox-runtime`）。

工作区是命名卷 `yanshi-ws-<id>`，容器删除后仍然保留。命令超时由沙箱内的 `timeout -s KILL` 强制执行，客户端取消时也会删除执行进程。

## 6. 模拟测试

模拟世界加入多个沙箱控制器（使用确定性的假 Provider），随机注入控制器崩溃（包括执行途中）、租约过期，以及用户中断。新增不变量：**`exec` / `run_python` 对同一 `call_id` 至多执行一次**。

## 7. 生产 Provider（S4，待实现）

隔离技术的比较与压测数字在 ADR-0053 的"选型结论"。实现要点（ADR-0053 选型结论第 6 条）：

- **每个沙箱一个 Pod**，名称由沙箱 ID 派生；`runtimeClassName` 来自配置（默认 gVisor，有 KVM 的部署可配 Kata）。安全配置与 Docker 实现对齐：非 root、`allowPrivilegeEscalation: false`、丢弃全部 capability、只读根文件系统 + `/tmp` 内存卷、CPU 与内存限制。
- **工作区**：运行期间是 emptyDir；`Stop` 把工作区打包写入对象存储再删除 Pod，`Ensure` 创建 Pod 后恢复。工作区上限初值 1GB，超出时 `Stop` 拒绝打包并告知模型。打包的工作区在 `app.SessionData` 登记，由 Janitor 随 Session 删除（[Session 生命周期](./m2-session-lifecycle.md)）。
- **网络**：沙箱命名空间默认拒绝全部入站与出站；Pod 不挂载 ServiceAccount 令牌。控制器启动时检查拒绝策略存在，否则拒绝启动。
- **执行**：经 K8s exec API；超时由沙箱内的 `timeout -s KILL` 强制执行。
- **控制器权限**：只在沙箱命名空间内创建、删除、读取 Pod 与 `pods/exec`；不挂载节点的 `docker.sock`。
- 实现后在 K8s 上复测冷启动（判据 p95 ≤ 3 秒，超过则预热池 S3 成为上线前提）与并发 exec 对 apiserver 的负载。

压测工具 `scripts/sandboxbench` 直接驱动 Docker Provider，比较运行时的兼容性、冷热启动、典型负载开销与密度：

```
go run ./scripts/sandboxbench -runtime runsc -out runsc.json   # -runtime 为空即 runc
```

Docker 沙箱测试可用 `YANSHI_TEST_DOCKER_RUNTIME=runsc` 在 gVisor 下运行。
