---
status: deferred
---

# 生产沙箱运行在 Kubernetes + gVisor 上

私有化部署的目标环境是 Kubernetes，生产沙箱以 Pod 形式运行，并通过 RuntimeClass 使用 gVisor（runsc）隔离：C 端用户与模型生成的代码属于不可信代码，普通容器隔离不足；gVisor 不依赖硬件虚拟化（KVM），在客户 IDC 中可普遍部署。Firecracker / Kata 隔离更强但依赖 KVM，作为可选实现保留在 `sandbox.Provider` 接口之后。开发环境用 Docker 实现（可选 runsc 运行时）。

## 暂缓（2026-10-09）

生产沙箱暂不在 K8s 上运行，后续可能选用云厂商的沙箱服务，届时作为 `sandbox.Provider` 的一个实现接入。选型时须一并决定：云厂商沙箱只用于公有云部署（私有化部署仍需自托管实现），还是修订 ADR-0001。在此之前，本 ADR 不作为实施依据。
