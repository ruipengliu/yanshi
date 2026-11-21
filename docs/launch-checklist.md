# 上线检查清单

上线（对真实 EndUser 开放）之前必须逐项完成。每项完成时注明日期与依据（提交、文档或评测报告）。新增"上线前必须替换或确认"的事项时，加到这里。

## 必须完成

- [ ] **内容安全接入真实提供商**（[设计](./design/m4-moderation.md)，ADR-0021）。当前 `serve -moderation mock` 只是关键词模拟，没有真实识别能力。接入时：
  - 实现 `moderation.Moderator`（文本、图片与音频：语音消息由模型直接听，不经转写），按提供商的错误语义区分"违规"与"不可用"；
  - 确认流式草稿的处理是否满足监管要求，必要时实现流式检查（按片段检查、违规时取消生成）；
  - 评估是否需要对外部来源（MCP、网页）的调用结果单独检查；
  - 上线配置中 `yanshi_moderation_mock` 必须为 0；
  - 容灾：与合规确认一个故障域独立的等价提供商，配置为 `-moderation 主,备`（`moderation.Failover`，备用方只在主方超时或出错时使用）；全部不可用时是否允许暂存输出、恢复后重试检查。
- [x] **修复 Memory 写入闸门的文字覆盖**（ADR-0023 的 2026-11-14 修订）。原先字符二元组只认汉字、ASCII 小写字母与数字，其他文字组成的内容被判为有依据而免审。2026-11-15 修复：NFKC 规范化后覆盖 Unicode 字母、附加符号与数字，识别不了的按无依据处理；测试见 `internal/runtime/memgate_test.go`，评测用例 `memory-gate-foreign-script`。
- [ ] **审核服务故障时长 Run 的行为**（ADR-0054，2026-11-15 已实现挂起等待）。输出审核不可用时 Run 挂起、退避后重新生成，至多等 6 小时后失败。上线前须与合规确认：挂起期间只展示过已推送并随即撤回的草稿、等待上限的取值是否可接受；并按线上数据确认退避参数。
- [ ] **价格表填入合同价**（[设计](./design/m4-quota-usage.md)）。`pricing.yaml` 中 ark、tokenhub 的价格与沙箱单价是占位值。
- [x] **生产沙箱**（ADR-0053，[设计](./design/m2-sandbox.md) §7）。2026-11-21：隔离技术选定（K8s Provider，隔离经 RuntimeClass，默认 gVisor，有 KVM 时可选 Kata），K8s Provider 已实现（`serve -sandbox k8s`），在单节点 k3s + gVisor 上通过一致性套件并复测（冷启动 p95 667 ms，30 路并发 exec p95 208 ms，见 ADR-0053）；部署清单改为沙箱命名空间内的最小权限，不再挂载 `docker.sock`。在目标集群上的验证见下面的"生产部署形态"。云厂商沙箱只作为公有云部署可选的 Provider。
- [ ] **业务线配置**：每条业务线的密钥、保留策略与配额已按约定填写（`businesslines/*.yaml`）。
- [ ] **健康信息的单独同意**（ADR-0022 及其修订）：开启 `memory.health` 的业务线须在产品中取得用户的单独同意，每个用户同意后经 `PUT /v1/end_users/{id}/consents/health` 登记（返回的 `granted_at` 是平台侧的同意记录，业务线自己也须能出示同意记录）；用户撤回时调用 `DELETE`（需要删除已有健康信息时带 `?purge=true`）。未取得同意流程的业务线不得开启；未登记的用户不写入、不召回健康信息。
- [x] **注入判断的模式**（ADR-0030）：2026-11-20 在留出集上评估（[判断设计](./design/m6-decisions.md) §9），自托管的 Qwen3-4B 与 Qwen3-8B 都未达到判据（假阳性率 ≤ 1% 时检出率 55.4%、70.7%，判据 ≥ 80%），**保持影子模式**，书面接受残留风险：来自他人与外部来源的注入没有平台层面的阻断，安全边界仍是未声明只读的工具须审批（ADR-0017）、高风险能力须审批（`RequiresApproval`）与 Memory 写入闸门（ADR-0023）；声称已获授权的写法与委托回复中转的注入检出最低。重新评估的条件见判断设计 §9。上线时可开启 `-injection-check local/<模型>` 以影子模式记录分布（需部署自托管推理端点），不开启也不影响安全边界。
- [x] **说话人误标**（ADR-0052 第 8 条）：被误标为佩戴者的他人文字算用户原话，不进入注入判断，平台无法发现。2026-11-20 书面接受为上线的残留风险：注入判断不能兜底（佩戴者对助手下指令是正常用法，检查佩戴者的话会大量误报）；现有缓解是不依赖标签的高风险审批（`RequiresApproval`）与业务线对标签正确性的责任（ADR-0052 第 4 条）；重审条件见 ADR-0052（误标在评测或线上被观察到造成越权写入）。
- [ ] **Call 的语音输出审核**（ADR-0027，[设计](./design/m3-call.md) §6）：播出的语音边生成边播放，我们的检查（助手整句生成时）只能中止剩余的播放，之前的部分依赖厂商的严格审核（`strict_audit`）。须与合规确认这一方式可接受；不可接受时改为按句缓冲、检查后再下发（增加数百毫秒延迟）。
- [ ] **实时语音模型的价格与用量**：`pricing.yaml` 中 `volc/1.2.6.1` 为刊例价，按合同价填写；每个会话的第一轮约有 5K token 的内置文本输入，评估对短通话成本的影响。
- [ ] **评测基线**：上线所用的 AgentDef 版本与模型通过 `make eval`，基线随代码提交（ADR-0018）。
- [x] **委托的阻断项**（[评审](./review/2026-10-11-scalability-review.md) §11 问题 1、2）：对真实 EndUser 开放委托（AgentDef 白名单含 `agent:`）之前，修复委托输入绕过 Memory 写入闸门与子 Session 可被复活。2026-10-11 完成，见评审 §11.4。
- [ ] **生产部署形态**（[部署设计](./design/deployment.md)，[延展性评审](./review/2026-10-11-scalability-review.md) §4.5、§4.6、§8）。先完成设计 §6 的代码改动，再按清单部署：
  - 角色开关：`serve -roles ...` 按角色裁剪（网关、交互池 Worker、后台池 Worker、沙箱控制器、Janitor），全部角色等价于现在的 serve；
  - PostgreSQL 主 + 同步备，故障切换后已确认 Event 不丢、seq 不倒退，旧主隔离；SQL 经 PgBouncer 事务模式，`LISTEN` 与迁移锁经独立的直连 DSN（`-pg-listen-dsn`）；
  - 就绪探针检查数据库可达、LISTEN 已生效、迁移已全部应用，而不是只返回 200；
  - 发布前以独立任务运行 `yanshi migrate`，serve 以 `-migrate=false` 启动（缺迁移时拒绝启动）；
  - `-drain-timeout` 小于编排系统的终止宽限期，滚动发布时进行中的 Run 移交而不是等租约过期（ADR-0029）；PodDisruptionBudget 限制同时下线的网关数；
  - 沙箱：每个集群应用一次 `deploy/k8s/cluster`（RuntimeClass；节点池预装 runsc 与 containerd 的 shim），每个单元应用一次 `deploy/k8s/sandbox`（沙箱命名空间、deny-all 网络策略、控制器的 Role）；CNI 必须执行 NetworkPolicy，沙箱节点池的 kubelet 设 `podPidsLimit`（Docker 实现为 256，Pod 规格没有对应字段）；集群 1.30 或更高（exec 的 v5 流协议）。只有沙箱控制器与 Janitor 绑定沙箱命名空间的权限（ADR-0008、ADR-0053），其出站须放行 apiserver；在目标集群复测冷启动（判据 p95 ≤ 3 秒）、首次拉取沙箱镜像的耗时与并发 exec 对 apiserver 的负载；
  - `-model-concurrency`、`-model-rps` 按提供商的账号上限除以进程数配置（[模型调用](./design/model-calls.md)），弹性扩容时随副本数联动；
  - 所有进程的 `-notify-shards` 一致（ADR-0028）；
  - 生产进程不带 `-faults`（服务端故障注入与 `/v1/debug/*` 只用于调试部署，ADR-0033），也不带 `-insecure-remote`（`-auth none` 监听非回环地址）；`xinli dev` 的夹具与页面只用于开发，不作生产服务；
  - 部署清单（Helm 或 Kustomize）进仓库，含网络策略：出网只允许模型、内容安全、推送三类外联（ADR-0001）；
  - 配置分发（AgentDef、发布配置、业务线公钥、价格表在所有进程一致）与按 `run_id` 的链路追踪；
  - 容量矩阵与准入阈值在目标环境实测后填入（评审 §8.4 第 4、6 项）。
- [ ] **数据库预分片与扩容演练**（[ADR-0051](./adr/0051-shard-slots-and-id-location.md) §5.4，[部署设计](./design/deployment.md) §10.5）：
  - [x] 部署配置把 1024 槽均分到 8 个分片，每片一个独立 database（2026-11-21：示例 `deploy/k8s/overlays/example/shards.json`，所有角色与 migrate 任务经 `YANSHI_SHARDS` 读取同一份，`TestExampleDeploymentShards` 校验）；上线后改片数需要尚未实现的迁槽工具；生产配置按实例分布改写 DSN，每片主 + 同步备；
  - [x] 整片搬迁演练（2026-11-21：`make shard-move`，`TestShardMoveKernel`）：新主机在线建同步备库、关闭该片准入、提升、隔离旧主、改指端点；两端各表行数与摘要一致，`shard_identity`、出站 epoch 与 `Identity()` 不变，其他分片持续服务。Mac mini 上 1GB 与 10GB 的停写窗口为 0.4～0.6 s，不随数据量增长；
  - [ ] 在目标环境用未来 12 个月预期的最大数据量按部署设计 §10.5 的步骤实测该片的不可用时间（含 HA 控制器的隔离与代理改指），不超过 15 分钟，结果记入部署设计 §10.5；
  - [x] 搬迁报告不含原始 EndUser（只含行数、摘要、分片号、耗时与 Identity()）；回滚是反向搬迁，旧主的数据在新主恢复同步冗余后删除、至迟 7 天；从备份恢复时旋转 epoch 并重放删除记录（2026-11-21，部署设计 §10.5）。

## 建议确认

- [ ] 推送接入真实通道（APNs、各厂商推送，私有化环境的自建通道；作为外联项列入部署文档，ADR-0001）：提醒（`notify.Pusher`，[多端双工通道](./design/m3-duplex-channel.md) §9）与设备离线唤醒（`node.Waker`）目前都只是日志桩。
- [ ] Android 绑定实测（[端侧 SDK](./design/m3-client-sdks.md) §4）：开发机没有 NDK，`make mobile` 只编译了 Java 绑定。需要在装有 NDK 的机器上 `gomobile bind -target=android` 生成 aar，并在真机上跑一遍冒烟流程（原生能力、订阅、提交、推送登记）。
- [ ] SDK 分发：`@yanshi/client` 发布到业务线可访问的 npm 仓库，`Yanshi.xcframework` 与 aar 的分发方式（私有 CocoaPods / SwiftPM 二进制包 / Maven）及版本号约定。
- [ ] 监控告警覆盖：`yanshi_moderation_checks_total{result="error"}`、`yanshi_usage_record_errors_total`、`yanshi_release_reload_errors_total`、Run 失败率（按 `agent` 版本）、`yanshi_model_retries_total`、`yanshi_model_admission_wait_seconds`、`yanshi_handshakes_rejected_total`。
