# M2 云端代码沙箱

> 状态：S1、S4（K8s Provider，2026-11-21）已实现 · 依赖：[M1 设计](./m1-device-nodes.md) · ADR-0008、0009、0053（取代 0010）

## 1. 定位与范围

S1 的定位是"代码解释器"：Agent 编写并运行 Python 来处理数据、计算、出图，单次执行最长分钟级。后续阶段：S2 工件存储与设备↔沙箱文件传递；S3 快照与预热池；S4 生产 Provider（2026-11-21 已选型：K8s，隔离运行时经 RuntimeClass，默认 gVisor，见 ADR-0053 的选型结论与本文 §7）。

## 2. 沙箱即 Node

每个 Session 的沙箱是一个虚拟 Node，ID 为 `sbx_<session_id>`。对模型暴露的工具名是 `sandbox__<capability>`，由 AgentDef 白名单中的 `sandbox:<glob>` 启用。沙箱 Node ID 与工具名的约定定义在 `internal/capability/capname`。

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

## 7. 生产 Provider（S4，K8s，2026-11-21 已实现）

隔离技术的比较与压测数字在 ADR-0053 的"选型结论"。实现是 `internal/sandbox/k8s`，`serve -sandbox k8s` 启用；它不依赖 client-go，只用 Pod、`pods/exec`（WebSocket 的 `v5.channel.k8s.io`，要求 Kubernetes 1.30 或更高）与 NetworkPolicy 的 REST 接口。

- **每个沙箱一个 Pod**，名称是沙箱 ID 的摘要（`sbx-<24 位十六进制>`，原 ID 记在注解 `yanshi.io/sandbox-id`）；`runtimeClassName` 来自 `-sandbox-runtime-class`（默认 `gvisor`，有 KVM 的部署可配 `kata`）。安全配置与 Docker 实现对齐，并满足 Pod Security 的 restricted：非 root、`allowPrivilegeEscalation: false`、丢弃全部 capability、`seccompProfile: RuntimeDefault`、只读根文件系统 + `/tmp` 内存卷、CPU 与内存的请求等于上限、不挂载 ServiceAccount 令牌、不注入服务地址。进程数上限没有 Pod 级字段，由沙箱节点池 kubelet 的 `podPidsLimit` 设置。
- **工作区**：运行期间是 emptyDir。`Stop` 把工作区打包流式写入对象存储（与工件共用，键 `sbxws_<摘要>`）后删除 Pod；`Ensure` 创建 Pod 后解包恢复，再给 Pod 打上注解 `yanshi.io/restored`。没有这个注解的 Pod（恢复中途失败）不打包，以免残缺的工作区覆盖上一次的保存；对象存储的写入是原子的，打包失败时上一次的保存不变。
- **上限**：`-sandbox-workspace-limit-mb`（默认 1024，按打包后的大小）。超过时 `Stop` 返回 `sandbox.ErrWorkspaceTooLarge`、不删除 Pod，并给 Pod 打上 `yanshi.io/oversize`；控制器的空闲回收重新记一次活跃、稍后再试；此后每次执行的 stderr 末尾提示模型删除不需要的文件。emptyDir 的硬上限是两倍，超过时 kubelet 驱逐 Pod。
- **Pod 在 Stop 之外消失**（驱逐、节点故障）：下次 `Ensure` 重建并恢复上一次保存的工作区，其后的改动丢失。这与 Docker 命名卷不同（容器崩溃不丢工作区），是不依赖动态卷供应的代价（ADR-0053 第 6 条）。
- **网络**：沙箱命名空间的 deny-all NetworkPolicy（`deploy/k8s/sandbox`）。构造 Provider 时检查它存在（选中全部 Pod、Ingress 与 Egress、没有规则），否则 serve 拒绝启动。
- **执行**：经 apiserver 的 exec，命令包成 `sh -c 'cd /workspace && exec "$@"' … timeout -s KILL <秒> <命令>`，超时在沙箱内强制执行；stdin 按 16KB 分帧，结束时发送 v5 的关闭帧。
- **权限**：控制器与 Janitor 用 ServiceAccount `yanshi-sandbox-ctl`，Role 只在沙箱命名空间内：Pod 的 get / create / delete / patch、`pods/exec`、NetworkPolicy 的 list。其余角色带 `-sandbox=k8s` 只为登记沙箱工具，不构造 Provider、不需要权限。
- **部署**：每个集群应用一次 `deploy/k8s/cluster`（RuntimeClass，`scheduling` 把沙箱 Pod 放到 `yanshi.io/pool: sandbox` 节点池）；每个单元应用一次 `deploy/k8s/sandbox`。

测试：`sandboxtest.RunProvider` 是 Provider 的一致性套件，Docker 与 K8s 实现都跑它。K8s 的集成测试另测超限工作区、Pod 丢失后恢复与网络策略检查，需要一个装有隔离运行时的集群：

```
YANSHI_TEST_K8S=1 YANSHI_TEST_K8S_API=https://127.0.0.1:6443 YANSHI_TEST_K8S_TOKEN_FILE=<令牌> \
YANSHI_TEST_K8S_CA_FILE=<CA> YANSHI_TEST_K8S_NAMESPACE=yanshi-sandbox YANSHI_TEST_K8S_RUNTIME_CLASS=gvisor \
go test ./internal/sandbox/k8s
```

压测工具 `scripts/sandboxbench` 比较运行时的兼容性、冷热启动、Stop、典型负载开销与密度：

```
go run ./scripts/sandboxbench -runtime runsc -out runsc.json          # Docker；-runtime 为空即 runc
go run ./scripts/sandboxbench -provider k8s -runtime gvisor -k8s-token-file … -k8s-ca-file …
```

Docker 沙箱测试可用 `YANSHI_TEST_DOCKER_RUNTIME=runsc` 在 gVisor 下运行；`make eval` 接受 `-sandbox k8s` 与相应参数，在集群里的沙箱上评测。
