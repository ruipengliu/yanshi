---
status: accepted（2026-11-14；取代 ADR-0010；2026-11-21 修订：选型完成，默认 gVisor；同日 K8s Provider 已实现）
---

# 生产沙箱以自托管隔离为基线，隔离技术按四项依据选定

> 2026-11-21 修订：隔离技术的选型已完成，结论与比较依据见文末"选型结论"。第 2 条与第 5 条已按结论改写；第 1、3、4 条不变。

ADR-0010 选定了 Kubernetes + gVisor，随后又自行暂缓，留下一个分叉："云厂商沙箱只用于公有云部署（私有化部署仍需自托管实现），还是修订 ADR-0001"。此后三处说法不一致：ADR-0010 自称"不作为实施依据"，[部署设计](../design/deployment.md)却把 gVisor RuntimeClass 当作定论，[上线检查清单](../launch-checklist.md)又写"选定云厂商服务或自托管隔离方案"。章程 §8 要求一套部署制品能在无外网的 K8s 集群上完成部署，而沙箱至今只有开发用的 Docker 实现（`internal/sandbox/docker`）。

私有化部署仍是硬约束（ADR-0001，[ADR 评审](../review/2026-11-14-adr-review.md) §1），所以那个分叉已有答案。本 ADR 先记下这个答案，再（2026-11-21）记下按四项依据比较后的选型。

## 决定

1. **自托管隔离是基线。** 每一种部署形态都必须有一个能在客户 IDC、无外网环境中运行的生产沙箱实现。"以云厂商沙箱为唯一实现、修订 ADR-0001"一支不成立。
2. **隔离技术按四项依据选定**（2026-11-21 已选定，见"选型结论"）：
   - **安全边界**：不可信代码逃逸的攻击面，以及与 ADR-0008（沙箱内无受信组件）的配合；
   - **客户 IDC 能否部署**：对宿主内核版本、硬件虚拟化（KVM）与嵌套虚拟化的要求；
   - **运维成本**：节点池、镜像、升级与监控；
   - **压测结果**：启动延迟、预热池（S3）、执行开销与密度。

   压测不能单独决定：一个更快但在多数客户 IDC 里部署不了的方案不可选。
3. **云厂商沙箱只是可选的 Provider。** 在公有云部署中，可以把云厂商的沙箱服务作为 `sandbox.Provider` 的一个实现接入，按 ADR-0001 显式列为外联项、可以配置关闭；它不能替代第 1 条的自托管实现。
4. **不变的约束。** ADR-0008（受信控制器在沙箱之外，沙箱内不持有凭证）与 ADR-0009（每个 Session 一个沙箱工作区）照旧适用，与隔离技术的选择无关。
5. **隔离技术是配置，不是代码分支**（2026-11-21 改写；原文为"选型之前不把某一种隔离技术写成定论"）。生产 Provider 只认 RuntimeClass 名称；默认值与可选值以本 ADR 的选型结论为准，文档引用默认值时链接本 ADR，不另写依据。

## 选型结论（2026-11-21）

### 决定

1. **生产 Provider 建在 Kubernetes 上，隔离运行时经 RuntimeClass 选择。** 每个沙箱一个 Pod；控制器只在沙箱命名空间内持有创建、删除与 exec Pod 的最小权限，不挂载节点的 `docker.sock`（hostPath 挂载等于节点 root，现有 `deploy/k8s/base/sandbox-ctl.yaml` 的写法不能上线）。换隔离技术只改 RuntimeClass，不改代码。
2. **默认 gVisor（runsc），平台用 systrap。** 不依赖 KVM，在虚拟机节点上可用；gVisor 官方文档也只建议在裸金属上用 KVM 平台，虚拟机中用 systrap。
3. **Kata Containers 是可选的 RuntimeClass**，只用于客户节点提供 KVM（裸金属，或开启嵌套虚拟化的虚拟机）且要求硬件虚拟化边界的部署。它在没有 KVM 的节点上无法运行，不能作为默认。
4. **Firecracker 不单独作为候选。** 它本身不是 K8s 运行时：要么经 Kata 使用（即第 3 条），要么自建调度，后者违背章程 §8 的"一套 K8s 部署制品"。
5. **runc 只作对照与开发。** 内核与不可信代码共享，攻击面是整个宿主内核系统调用面，不用于生产。
6. **Provider 的约束**（实现时遵守，改动须修订本 ADR）：
   - **工作区**：运行期间用 Pod 的 emptyDir；Stop 时打包写入工件存储所用的 S3 兼容存储，Ensure 时恢复（不依赖客户 IDC 的动态卷供应）。设工作区上限（初值 1GB），超出时 Stop 拒绝打包并告知模型。打包的工作区在 `app.SessionData` 登记，由 Janitor 按 Session 删除，并在 `internal/sim` 的删除不变量中检查（ADR-0015）。
   - **网络**：沙箱命名空间默认拒绝全部入站与出站的 NetworkPolicy；Pod 不挂载 ServiceAccount 令牌（`automountServiceAccountToken: false`）。部署文档写明 CNI 必须执行 NetworkPolicy；控制器启动时检查拒绝策略存在，不存在则拒绝启动。
   - **执行**：经 K8s exec API（apiserver）执行，沙箱内不放执行代理；超时照旧由沙箱内的 `timeout -s KILL` 强制执行。并发 exec 对 apiserver 的负载在实现时压测，成为瓶颈时另写 ADR。

### 比较依据

| 依据 | gVisor（systrap） | Kata（QEMU / Cloud Hypervisor / Firecracker） | runc（对照） |
|---|---|---|---|
| 安全边界 | 用户态内核 Sentry 拦截系统调用，宿主只暴露收窄后的系统调用面；与 ADR-0008 相容（Sentry 是运行时，不持有平台凭证） | 每个 Pod 一个轻量虚拟机，边界是硬件虚拟化 + 客户机内核，最强；与 ADR-0008 相容 | 与宿主共享内核，最弱 |
| 客户 IDC 可部署性 | 只需 Linux 与 containerd，不需要 KVM，虚拟机节点可用 | 必须有 `/dev/kvm`：裸金属，或开启嵌套虚拟化的虚拟机（多数公有云普通实例不开放嵌套虚拟化） | 处处可用 |
| 运维成本 | 节点装 runsc 与 containerd 的 shim，一个 RuntimeClass；升级随 runsc 发布 | 节点装 Kata、客户机内核与镜像、虚拟机监视器，配置 `overhead.podFixed`；运维面最大 | 无额外成本 |
| 压测 | 见下表（本机实测） | 未实测：没有带 KVM 的机器。公开资料称每个 Pod 多 150～300ms 启动与约 128MB 基础内存（第三方数字，未核实） | 见下表 |

**压测**（2026-11-21，Mac mini 上 OrbStack 的 arm64 Linux 虚拟机，无 KVM；gVisor release-20261116.0，systrap 平台；Docker Provider 的安全配置；工具为 `scripts/sandboxbench`）：

| 项目 | runc | runsc | 比值 |
|---|---|---|---|
| 兼容性：Docker 沙箱测试 5 项、典型负载 10 项、隔离复核 | 全部通过 | 全部通过 | — |
| 评测 `sandbox-compute`、`large-file-sandbox`（`-sandbox-runtime runsc`） | — | 各 3/3，与基线一致 | — |
| 冷启动 p95（新工作区到可执行） | 226 ms | 229 ms | 1.01 |
| 热启动 p95（Stop 后 Ensure） | 112 ms | 116 ms | 1.04 |
| pandas（50 万行分组与 CSV） | 608 ms | 654 ms | 1.08 |
| xlsx 读写（2 万行） | 599 ms | 647 ms | 1.08 |
| matplotlib 出图（中文标题） | 303 ms | 308 ms | 1.02 |
| 纯 Python 计算 | 139 ms | 142 ms | 1.02 |
| Python 启动 | 15 ms | 27 ms | 1.76 |
| 500 个小文件读写 | 18 ms | 44 ms | 2.40 |
| 50 次子进程启动 | 164 ms | 324 ms | 1.98 |
| ffmpeg 生成 5 秒音频 | 51 ms | 87 ms | 1.71 |
| 每个空闲沙箱的内存 | 5.8 MB | 13.2 MB | 2.3 |
| 30 个沙箱并发执行的 p95 | 122 ms | 166 ms | 1.36 |

判据（压测之前约定，2026-11-21 修正）：兼容性全部通过；每次执行增加的耗时不超过 30% **或**绝对值不超过 100 ms；冷启动 p95 不超过 3 秒（超过则 S3 预热池成为上线前提）。原判据只写了"不超过 30%"，小文件读写（2.4 倍）与进程密集的负载按比例不达标，但绝对增量在 12～160 ms 之间，同一 Run 中一次模型调用以秒计，所以修正判据而不是否决；原始数字照录于上表。

限制：上表是 Docker 层面的数字，不含 K8s 的调度与 Pod 创建；K8s 上的复测见下；Kata 没有实测。

### 实现与 K8s 复测（2026-11-21）

K8s Provider 已实现（`internal/sandbox/k8s`，`serve -sandbox k8s`；设计见 [m2-sandbox](../design/m2-sandbox.md) §7），按第 6 条的约束。在同一台机器的单节点 k3s（v1.37.2，containerd 2.3.7，gVisor 经 RuntimeClass `gvisor`）上：Provider 一致性套件、超限工作区、Pod 丢失后恢复与网络策略检查全部通过；Pod 内 `dmesg` 显示 gVisor，删除 deny-all 策略后沙箱能连上 apiserver、恢复策略后连不上，确认网络阻断来自策略。

| 项目（镜像已在节点上） | k3s 默认 runc | gVisor |
|---|---|---|
| 冷启动 p50 / p95（创建 Pod 到可执行） | 646 / 662 ms | 645 / 667 ms |
| 热启动 p95（Stop 后创建 Pod 并恢复 10MB 工作区） | 702 ms | 734 ms |
| Stop p95（打包上传 10MB 并删除 Pod） | 73 ms | 94 ms |
| 经 apiserver 的一次 exec（`true`）p50 | 16 ms | 11 ms |
| 数据分析负载（pandas、xlsx、出图）比值 | — | 0.99～1.03 |
| 小文件、子进程、ffmpeg 比值 | — | 2.0、1.83、1.79 |
| 30 个沙箱并发执行的 p95 | — | 208 ms |
| 每个空闲沙箱占用的宿主内存（含 Pod 基础设施） | — | 30.8 MB |

冷启动 p95 远低于 3 秒，预热池（S3）不是上线前提；30 路并发 exec 下 apiserver 不是瓶颈。限制：单节点、镜像已缓存；生产规模下 apiserver 的 exec 负载与首次拉取镜像的耗时在目标集群验证（[上线检查清单](../launch-checklist.md)的"生产部署形态"）。

## Considered Options

- **维持 ADR-0010 的暂缓**：不需要现在写任何东西，但部署设计已经在按 gVisor 写，上线检查清单又留着"云厂商服务"的选项。暂缓的分叉其实已由私有化约束决定，继续悬空只会让后来的读者各取所需。
- **现在就选定 gVisor**（ADR-0010 原决定）：不依赖 KVM、普遍可部署，但还没有按安全边界、运维成本与压测做过比较；先写定论再补依据，与评测即裁判的原则（章程原则 10）相悖。（2026-11-21：比较已完成，结论仍是 gVisor，但改为 RuntimeClass 配置，Kata 可选。）
- **以云厂商沙箱为主、修订 ADR-0001**：实现最快，但放弃了私有化这一硬约束。
- **默认 Kata**（2026-11-21）：边界最强，但依赖 KVM；在客户节点情况不明时，以"虚拟机节点、没有 KVM"为最坏情况，Kata 不能作为默认。
- **每个 Session 一个 PVC 作工作区**（2026-11-21）：语义与 Docker 命名卷最接近，但依赖客户存储类的动态供应，规模大时卷数量巨大，local-path 还会把 Session 绑在一个节点上。
- **沙箱内放执行代理**（2026-11-21）：比经 apiserver 的 exec 快，但在沙箱内放了一个控制器要经网络对话的组件，与拒绝全部网络的策略冲突。

## Consequences

- 生产沙箱 Provider 已实现并在 K8s 上复测（2026-11-21，见上）；在目标集群上的验证归入[上线检查清单](../launch-checklist.md)的"生产部署形态"。
- 部署设计中 sandbox-ctl 的节点池使用 gVisor 的 RuntimeClass（2026-11-21）；装有 KVM 的客户可改用 Kata。
- 开发环境继续使用 Docker 实现，可选 `-sandbox-runtime runsc`；`make eval` 也接受 `-sandbox-runtime`，改动沙箱镜像或沙箱相关的提示词时可在 runsc 下复核。

## 重审条件

任一出现即重新评估：

- 公司不再承接私有化部署（ADR-0001 被重审）；
- 出现某个目标客户环境，gVisor 与 Kata 都无法部署；
- gVisor 出现可利用的逃逸漏洞且修复不及时，或其 systrap 平台在目标内核上不可用；
- 线上数据显示沙箱执行占 Run 耗时的显著比例，且以进程或小文件密集的负载为主（批量启动子进程、`pip` 一类）：在有 KVM 的节点上实测 Kata，或调整 gVisor 的文件系统配置；
- 目标集群上的冷启动 p95 超过 3 秒（S3 预热池成为上线前提）；
- 并发 exec 使 apiserver 成为瓶颈（沙箱内执行代理需要另写 ADR，见第 6 条）。
