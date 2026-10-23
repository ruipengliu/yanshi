# 上线检查清单

上线（对真实 EndUser 开放）之前必须逐项完成。每项完成时注明日期与依据（提交、文档或评测报告）。新增"上线前必须替换或确认"的事项时，加到这里。

## 必须完成

- [ ] **内容安全接入真实提供商**（[设计](./design/m4-moderation.md)，ADR-0021）。当前 `serve -moderation mock` 只是关键词模拟，没有真实识别能力。接入时：
  - 实现 `moderation.Moderator`（文本、图片与音频：语音消息由模型直接听，不经转写），按提供商的错误语义区分"违规"与"不可用"；
  - 确认流式草稿的处理是否满足监管要求，必要时实现流式检查（按片段检查、违规时取消生成）；
  - 评估是否需要对外部来源（MCP、网页）的调用结果单独检查；
  - 上线配置中 `yanshi_moderation_mock` 必须为 0；
  - 容灾：与合规确认一个故障域独立的等价提供商，配置为 `-moderation 主,备`（`moderation.Failover`，备用方只在主方超时或出错时使用）；全部不可用时是否允许暂存输出、恢复后重试检查。
- [ ] **价格表填入合同价**（[设计](./design/m4-quota-usage.md)）。`pricing.yaml` 中 ark、tokenhub 的价格与沙箱单价是占位值。
- [ ] **生产沙箱**（ADR-0010，[设计](./design/m2-sandbox.md)）。当前只有 Docker 实现；选定云厂商服务或自托管隔离方案后实现生产 Provider。
- [ ] **业务线配置**：每条业务线的密钥、保留策略与配额已按约定填写（`businesslines/*.yaml`）。
- [ ] **健康信息的单独同意**（ADR-0022）：开启 `memory.health` 的业务线须在产品中取得用户的单独同意，并能出示同意记录；未取得的不得开启。
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
  - 沙箱控制器运行在独立节点池，使用 gVisor RuntimeClass，只有它持有容器或 K8s API 权限（ADR-0008、ADR-0010）；
  - `-model-concurrency`、`-model-rps` 按提供商的账号上限除以进程数配置（[模型调用](./design/model-calls.md)），弹性扩容时随副本数联动；
  - 所有进程的 `-notify-shards` 一致（ADR-0028）；
  - 生产进程不带 `-faults`（服务端故障注入与 `/v1/debug/*` 只用于调试部署，ADR-0033），也不带 `-insecure-remote`（`-auth none` 监听非回环地址）与 `-xinli-mock`（模拟的 xinli MCP Server，只用于调试控制台）；
  - 部署清单（Helm 或 Kustomize）进仓库，含网络策略：出网只允许模型、内容安全、推送三类外联（ADR-0001）；
  - 配置分发（AgentDef、发布配置、业务线公钥、价格表在所有进程一致）与按 `run_id` 的链路追踪；
  - 容量矩阵与准入阈值在目标环境实测后填入（评审 §8.4 第 4、6 项）。

## 建议确认

- [ ] 推送接入真实通道（APNs、各厂商推送，私有化环境的自建通道；作为外联项列入部署文档，ADR-0001）：提醒（`notify.Pusher`，[多端双工通道](./design/m3-duplex-channel.md) §9）与设备离线唤醒（`node.Waker`）目前都只是日志桩。
- [ ] Android 绑定实测（[端侧 SDK](./design/m3-client-sdks.md) §4）：开发机没有 NDK，`make mobile` 只编译了 Java 绑定。需要在装有 NDK 的机器上 `gomobile bind -target=android` 生成 aar，并在真机上跑一遍冒烟流程（原生能力、订阅、提交、推送登记）。
- [ ] SDK 分发：`@yanshi/client` 发布到业务线可访问的 npm 仓库，`Yanshi.xcframework` 与 aar 的分发方式（私有 CocoaPods / SwiftPM 二进制包 / Maven）及版本号约定。
- [ ] 监控告警覆盖：`yanshi_moderation_checks_total{result="error"}`、`yanshi_usage_record_errors_total`、`yanshi_release_reload_errors_total`、Run 失败率（按 `agent` 版本）、`yanshi_model_retries_total`、`yanshi_model_admission_wait_seconds`、`yanshi_handshakes_rejected_total`。
