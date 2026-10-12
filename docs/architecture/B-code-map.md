# 附录 B 概念到代码对照

> 本套文档正文不出现代码位置；这一页是唯一的例外，供后续开发从设计跳到代码。对照的提交为 `bcb006d`。目录名相对仓库根。

## 顶层

| 位置 | 内容 |
|---|---|
| `cmd/yanshi` | 唯一的二进制：`serve`（按角色运行）、`chat`、`node`、`eval`、`bench`、`migrate`、`keygen`、`token`、`export-case` |
| `proto/yanshi/v1` | 契约：Event 与内容块、Connection 协议、投影快照；生成代码在 `gen/` 与 `sdk/web/src/gen` |
| `internal/` | 服务端全部实现（下表） |
| `sdk/nodesdk`、`sdk/mobile`、`sdk/web` | Go SDK、gomobile 绑定、TypeScript SDK |
| `agents/`、`businesslines/`、`mcp/`、`pricing.yaml` | AgentDef 与发布配置、业务线（公钥、保留策略、配额）、远程 MCP 登记、价格表 |
| `evals/` | 评测用例与基线 |
| `deploy/` | 镜像、沙箱镜像、Kustomize 清单 |
| `docs/` | 章程、术语表、设计文档、决策记录、评审、上线清单；本套文档在 `docs/architecture` |

## 第 2 章 核心模型

| 概念 | 位置 |
|---|---|
| Event 日志接口、内存与 PostgreSQL 实现、一致性套件、原子唤醒契约 | `internal/eventlog`（`memlog`、`pglog`、`eventlogtest`） |
| 投影：Reduce、State、有界化、校验 | `internal/session`（`state.go`、`events.go`） |
| 投影快照与版本号 | `internal/session`（`snapshot.go`、`pgsnapshot`、`snapshottest`） |
| 决策-提交循环、`CommitAndWake`、历史访问 | `internal/session`（`store.go`） |
| 工作队列接口、内存与 PostgreSQL 实现、类别、空闲门控 | `internal/workqueue`（`memqueue`、`pgqueue`、`workqueuetest`） |
| 时钟与 ID 的注入 | `internal/clock`、`internal/ids` |

## 第 3 章 执行引擎

| 概念 | 位置 |
|---|---|
| Worker、Step、advance、调用推进、错误预算、优雅停机 | `internal/runtime`（`worker.go`） |
| 上下文压缩：触发、选点、摘要请求复用前缀 | `internal/runtime`（`compact.go`） |
| 请求组装：摘要 + 历史、召回位置、结果截断、Call 转写呈现 | `internal/runtime`（`transcript.go`） |
| 召回、配额检查、Memory 写入闸门 | `internal/runtime`（`recall.go`、`quota.go`、`memgate.go`） |
| 实时增量总线、晚加入者 Snapshot、跨进程拉取 | `internal/live`（`peer`） |
| 模型网关抽象、token 估算、错误分类 | `internal/model` |
| 模型调用守卫：时限、重试、准入 | `internal/model`（`guard.go`） |
| 提供商：OpenAI 兼容（方舟、TokenHub、私有化）、复述、脚本化、压测、Codex 评分 | `internal/model/openaicompat`、`echo`、`script`、`bench`、`codex` |
| 工作类别判定、优先级 | `internal/workqueue`（`workqueue.go`） |
| Submit 准入 | `internal/service`（`admission.go`） |

## 第 4 章 能力与路由

| 概念 | 位置 |
|---|---|
| Capability 定义、进程内能力、目录（白名单 × Node × MCP × Agent） | `internal/capability` |
| Node 目录、Inbox、Hub、保留 ID、内存与 PostgreSQL 实现 | `internal/node`（`pgnode`、`nodetest`） |
| WebSocket 传输 | `internal/node/wsgateway` |
| 设备 SDK：连接、重连、Executor 账本、MCP 桥接、工件上传下载 | `sdk/nodesdk` |
| 提问（ask_user）：参数校验、回答校验、`@user` | `internal/askuser` |
| 卡片：校验、动作核对、输入 ID 推导、内置图标 | `internal/card`；服务侧 `internal/service`（`cards.go`） |
| 沙箱：能力、控制器、Provider 接口、Docker 实现、共享账本 | `internal/sandbox`（`docker`、`pgsandbox`、`sandboxtest`） |
| 工件：元数据、字节（文件系统、S3）、服务 | `internal/artifact`（`fsblob`、`s3blob`、`pgartifact`、`artifacttest`） |
| 远程 MCP：登记、工具发现、注解映射、结果映射 | `internal/mcpcap` |
| 委托：派发、撤回、写回、对账、可委托 Agent 目录、派发链 | `internal/delegation` |
| AgentDef 加载与发布配置 | `internal/agentdef` |

## 第 5 章 多端与协议

| 概念 | 位置 |
|---|---|
| Connection 的会话客户端角色：订阅、请求、在场、Call 消息转交 | `internal/channel` |
| 事件流：续传、共享读者、删除时结束 | `internal/feed` |
| 在场存储 | `internal/presence`（`pgpresence`、`presencetest`） |
| 提醒：规则、设备登记、推送通道接口 | `internal/notify`（`pgnotify`、`notifytest`） |
| 界面上下文的呈现 | `internal/model`（文本呈现函数） |
| Call 中转：节拍器、转写、run_task、结果回注 | `internal/call` |
| 实时语音模型抽象、豆包适配器、脚本化实现 | `internal/realtime`（`volc`、`fake`） |
| HTTP API 与 SSE、授权矩阵测试 | `internal/httpapi` |
| 服务层：创建、提交（去重、卡片动作、回答）、中断、审批、关闭、Call 开始与转写 | `internal/service` |
| 网页 SDK：客户端、草稿、待回答提问、卡片、通话 | `sdk/web/src` |
| 移动端绑定与 Swift 冒烟程序 | `sdk/mobile` |

## 第 6 章 信任与安全

| 概念 | 位置 |
|---|---|
| 令牌验证、业务线公钥加载 | `internal/auth` |
| 内容安全接口、模拟提供商、故障切换、请求构造 | `internal/moderation` |
| Memory 与 Grant：存储、服务、嵌入、文本相似度退化 | `internal/memory`（`pgmemory`、`memorytest`） |
| 写入闸门判定 | `internal/runtime`（`memgate.go`） |
| 判断层：接口、规则、logits、守卫、熔断 | `internal/decision` |
| Session 生命周期：索引、删除记录、写后复查、关闭守卫 | `internal/lifecycle`（`pglifecycle`、`lifecycletest`） |
| Janitor：清理、保留策略、级联删除、设备清除请求、队列统计 | `internal/janitor` |
| Session 数据登记列表 | `internal/app`（`SessionData`） |

## 第 7 章 存储与部署

| 概念 | 位置 |
|---|---|
| 装配：存储集合（内存 / PostgreSQL）、核心对象图、选项 | `internal/app` |
| PostgreSQL 公共部分：连接池、两个 DSN、迁移、通知器（心跳、代、校准）、分片键登记 | `internal/pg`（`migrations`、`shards.go`、`notify.go`） |
| 计量与配额：存储、小时聚合与分片、价格表、已用金额缓存 | `internal/usage`（`pgusage`、`usagetest`） |
| 发布配置：分流、撤回、热加载 | `internal/agentdef` |
| 角色开关、就绪探针、各进程参数 | `cmd/yanshi`（`serve.go`） |
| 部署清单与镜像 | `deploy/k8s/base`、`deploy/k8s/overlays/example`、`deploy/Dockerfile`、`deploy/sandbox` |
| 指标定义 | `internal/metrics` |

## 第 8 章 质量保障

| 概念 | 位置 |
|---|---|
| 确定性模拟：世界、故障注入、不变量、覆盖统计、差分 | `internal/sim`（按 call、delegation、lifecycle、moderation、notify、rollout、usage 分文件） |
| e2e：HTTP / WebSocket 传输上的全流程、Call、委托、MCP、网页互通、移动端绑定、控制台场景 | `internal/e2e` |
| 评测运行器、报告、基线比较、用例导出 | `internal/eval` |
| 评测用例与基线 | `evals/*.yaml`、`evals/baselines` |
| 服务端故障注入 | `internal/faults`；调试接口在 `internal/httpapi`（`debug.go`） |
| 调试控制台：三个视图、模拟设备、验证场景、脚本化模型 | `internal/webui/static/console`（`device.js`、`scenarios.js`、`chat.js`、`cards.js`……）；`internal/model/script` |
| 压测客户端与编排 | `cmd/yanshi`（`bench`）、`scripts/bench.sh`、`internal/model/bench` |
| 测试用 PostgreSQL schema 隔离 | `internal/pg/pgtest` |
| 文档引用检查 | `scripts/docrefs` |
| 完成标准与各目标 | `Makefile`（`check`、`sim`、`eval`、`mobile`、`gen`） |

## 一致性套件一览

| 套件 | 位置 | 覆盖的接口 |
|---|---|---|
| eventlogtest | `internal/eventlog/eventlogtest` | 事件日志、原子唤醒 |
| workqueuetest | `internal/workqueue/workqueuetest` | 工作队列 |
| nodetest | `internal/node/nodetest` | Node 目录、Inbox |
| ledgertest | `sdk/nodesdk` | 去重账本 |
| sandboxtest | `internal/sandbox/sandboxtest` | 沙箱活跃记录 |
| artifacttest | `internal/artifact/artifacttest` | 工件元数据与字节 |
| lifecycletest | `internal/lifecycle/lifecycletest` | Session 索引、删除记录 |
| memorytest | `internal/memory/memorytest` | Memory、Grant |
| snapshottest | `internal/session/snapshottest` | 投影快照 |
| usagetest | `internal/usage/usagetest` | 用量 |
| presencetest | `internal/presence/presencetest` | 在场 |
| notifytest | `internal/notify/notifytest` | 推送设备登记 |

## 从一个问题出发

| 想弄清 | 从哪里开始读 |
|---|---|
| 一条输入进来后发生了什么 | `internal/service/service.go` 的提交 → `internal/session/store.go` 的 `CommitAndWake` → `internal/runtime/worker.go` 的 `Step` |
| 某种 Event 怎样改变状态 | `internal/session/state.go` 的 `apply` 中对应的分支 |
| 一个工具名怎样解析到执行者 | `internal/capability/catalog.go` → `internal/runtime/worker.go` 的工具解析与派发 → `internal/delegation` 的派发链 |
| 为什么某个写入被拒绝 | `internal/session/state.go` 的校验分支；`internal/lifecycle` 的复查 |
| 一个故障会怎样被处理 | `internal/sim` 中对应的注入点与不变量；`internal/faults` 的同名故障 |
| 新增一张表要做什么 | `internal/pg/migrations` 新增文件；`internal/pg/shards.go` 登记键；若存 Session 数据，在 `internal/app` 的 `SessionData` 登记并经 `lifecycle.Recheck` 复查；在 `internal/sim` 的删除不变量中检查 |
| 新增一个给模型看的文本 | 对应包；`evals/` 增加用例；运行 `make eval` 并与基线比较 |
