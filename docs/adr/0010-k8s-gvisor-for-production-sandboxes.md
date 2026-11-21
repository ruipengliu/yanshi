---
status: superseded by ADR-0053
---

# 生产沙箱运行在 Kubernetes + gVisor 上

> 已被 [ADR-0053](./0053-self-hosted-sandbox-isolation-baseline.md) 取代（2026-11-14）：私有化是硬约束，自托管隔离是基线；隔离技术（gVisor、Kata、Firecracker 等）仍待选型，本文的 gVisor 选择不作为实施依据。2026-11-21 ADR-0053 完成选型：默认仍是 gVisor，但改为经 RuntimeClass 配置、有 KVM 时可选 Kata，依据与约束以 ADR-0053 的选型结论为准。

私有化部署的目标环境是 Kubernetes，生产沙箱以 Pod 形式运行，并通过 RuntimeClass 使用 gVisor（runsc）隔离：C 端用户与模型生成的代码属于不可信代码，普通容器隔离不足；gVisor 不依赖硬件虚拟化（KVM），在客户 IDC 中可普遍部署。Firecracker / Kata 隔离更强但依赖 KVM，作为可选实现保留在 `sandbox.Provider` 接口之后。开发环境用 Docker 实现（可选 runsc 运行时）。

## 暂缓（2026-10-09）

生产沙箱暂不在 K8s 上运行，后续可能选用云厂商的沙箱服务，届时作为 `sandbox.Provider` 的一个实现接入。选型时须一并决定：云厂商沙箱只用于公有云部署（私有化部署仍需自托管实现），还是修订 ADR-0001。在此之前，本 ADR 不作为实施依据。
