# M2-S2 工件存储

> 状态：已实现 · 依赖：[M2 沙箱](./m2-sandbox.md) · ADR-0011 · 契约：`ContentBlock.Media`

## 1. 定位

Artifact 是 Run 产出或用户、设备提供的、可独立引用的文件，例如录音、图表、表格。字节内容不进入 Event 日志，日志中只以 `Media{uri: "artifact://<id>", mime_type, name, size}` 引用。

## 2. 模型

- **Artifact 不可变**：一次写入，带有 `sha256`、大小、MIME 类型和文件名。
- **归属**：每个 Artifact 属于一个 Session，Session 决定其 BusinessLine 与 EndUser。不跨 Session 共享；委托的父子之间经显式复制交接（下表，ADR-0055），副本是目标 Session 的新工件。
- **存储分两部分**：元数据存 `artifact.MetaStore`（内存或 PG），字节内容存 `artifact.BlobStore`（本地文件系统或 S3）。

## 3. 流转

| 路径 | 方式 |
|---|---|
| 用户上传 | `POST /v1/sessions/{id}/artifacts`，然后以 Media 内容块作为输入提交 |
| 设备 → 云 | Capability 处理函数调用 SDK 的 `UploadArtifact`，结果中返回 Media 引用 |
| 云 → 设备 | Capability 参数中携带 Artifact ID，处理函数调用 SDK 的 `DownloadArtifact` |
| 沙箱 ↔ 工件 | 沙箱能力 `import_file`（工件 → 工作区）与 `export_file`（工作区 → 工件），由控制器在沙箱外完成 |
| 委托交接 | 父 Run 在委托参数 `artifacts` 中点名的工件复制进子 Session；子 Run 最后一条回复提到的子 Session 工件复制回父 Session。`artifact.Service.Copy`（`BlobStore.Copy`，S3 服务端复制），日志记 `ArtifactCopied`（[委托设计](./m5-multi-agent.md) §9） |
| 下载 | `GET /v1/artifacts/{id}` |

沙箱本身仍然没有网络，也不持有凭证（ADR-0008）。文件由控制器经容器运行时复制进出沙箱。

## 4. 对模型的呈现

模型看到的 Artifact 是一行文本，例如 `[artifact art_x: chart.png, image/png, 7.6 KB]`。模型把其中的 ID 传给下一个工具，就能在设备与沙箱之间接力处理文件。

用户输入中的 Media 只能引用本 Session 的 Artifact（`artifact://<id>`）：带内联字节、外部链接或别的 Session 的工件的输入被拒绝（`invalid`），类型、大小与名称以工件元数据为准（`service.normalizeMedia`）。这样内容安全检查的对象就是模型看到的内容，文件内容也不会进入日志。

用户消息中的图片 Artifact（≤ 4MB）会内联给模型，以支持视觉理解。工具结果中的图片只以文本呈现，因为多数模型接口不支持在工具结果里放图片。

## 5. 限制（S2）

- 单个 Artifact 最大 512MB。
- M2 的 HTTP API 仍无鉴权（同 M0），Artifact 下载不做访问控制。鉴权方案确定后再补。
- 删除随 Session 一起进行，保留期由业务线配置（[Session 生命周期](./m2-session-lifecycle.md)）。

## 6. 调用结果不外置（2026-11-22，评审 A6）

调用结果（`ToolResult`）的全文存放在 Event 日志里，`max_tool_result` 只截断模型看到的部分（[长 Run](./m2-long-runs.md)）。[架构评估评审](../review/2026-11-22-architecture-evaluation-review.md) §5 A6 问：大结果是否应外置为所属 Session 的工件？**决定：现在不外置。**

**各来源的上限**（写日志之前）：远程 MCP 的文字结果累计 64 KiB（`nodesdk.MaxMCPText`），二进制内容本来就存为工件；沙箱 `run_python`、`exec` 的输出每路 16 KiB，`read_file` 64 KiB；进程内能力各自有小上限。**没有独立上限的**：设备 Node 的结果（只受 Connection 帧 16 MiB 限制）、后台结果（`BackgroundResult`，同设备）、委托子 Session 交回的末条回复（实际受模型输出长度限制）。

**压测**（`internal/eventlog/pglog/size_bench_test.go`，`YANSHI_BENCH_TOOLRESULT=1` 打开；Mac mini 上 OrbStack 的 PostgreSQL，逐条提交；高熵文本 / 可压缩 JSON）：

| 单条结果 | 提交 p50（ms） | 每条 WAL | 每条存储（含 TOAST） | 每条冷加载（ms） | 快照中每条 |
|---|---|---|---|---|---|
| 1 KiB | 0.76 / 0.76 | 2.5 / 4.1 KiB | 2.6 / 2.6 KiB | 0.01 / 0.02 | 1.5 KiB |
| 16 KiB | 1.05 / 0.81 | 18.7 / 4.2 KiB | 18.6 / 4.6 KiB | 0.05 / 0.04 | 16.5 KiB |
| 64 KiB | 1.52 / 1.21 | 70 / 11.6 KiB | 68 / 11.8 KiB | 0.08 / 0.11 | 64.5 KiB |
| 256 KiB | 3.74 / 2.43 | 276 / 42 KiB | 271 / 44 KiB | 0.22 / 0.21 | 256.5 KiB |
| 1 MiB | 10.9 / 4.7 | 1098 / 162 KiB | 1068 / 162 KiB | 0.80 / 1.22 | 1024.5 KiB |

删除一个含 20 条 1 MiB 结果的 Session 的日志约 5 ms、WAL 约 0.6 MiB。

**理由**：

- 有上限的来源（≤ 64 KiB）每条结果的提交延迟比 1 KiB 多不到 1 ms，WAL 与存储与结果大小同量级，冷加载与删除可以忽略；与一次模型调用的秒级耗时相比不是瓶颈。
- 外置要改的契约面大：`ToolResult` 的形状与重放确定性（ADR-0004）、`Transcript` 与压缩如何取全文（目前是纯内存计算）、来源推导与注入判断读取的文字、`feed` 与三个 SDK 的事件消费、委托交还的工件数上限；换来的节省只在罕见的大结果上。
- 真正的放大点是投影快照：快照存的是未压缩的历史（压缩之前），每 200 条事件整段重写一次；可压缩的结果在日志中经 TOAST 压到约六分之一，在快照中仍是原大小。即便如此，快照写入 20 条 1 MiB 结果约 0.1–0.24 s，属于偶发成本。

**重审条件**（任一出现）：

- 线上单条结果的 p95 超过 64 KiB，或设备、后台、委托结果出现 MiB 级的常态；
- `events` 的 WAL 中调用结果占主要部分，或快照重写成为 PG 写入的主要来源；
- 端侧（`feed`、SDK）因整条下发大结果出现延迟或内存问题。

届时先考虑的较小步骤：为没有上限的来源加与 MCP 一致的写入前上限（截断并注明，需写 ADR，属于行为变化）；快照只保存模型视图需要的截断结果、全文按需从日志读取。
