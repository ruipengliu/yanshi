# M2-S2 工件存储

> 状态：已实现 · 依赖：[M2 沙箱](./m2-sandbox.md) · ADR-0011 · 契约：`ContentBlock.Media`

## 1. 定位

Artifact 是 Run 产出或用户、设备提供的、可独立引用的文件，例如录音、图表、表格。字节内容不进入 Event 日志，日志中只以 `Media{uri: "artifact://<id>", mime_type, name, size}` 引用。

## 2. 模型

- **Artifact 不可变**：一次写入，带有 `sha256`、大小、MIME 类型和文件名。
- **归属**：每个 Artifact 属于一个 Session，Session 决定其 BusinessLine 与 EndUser。跨 Session 共享留待 Memory 一起设计。
- **存储分两部分**：元数据存 `artifact.MetaStore`（内存或 PG），字节内容存 `artifact.BlobStore`（本地文件系统或 S3）。

## 3. 流转

| 路径 | 方式 |
|---|---|
| 用户上传 | `POST /v1/sessions/{id}/artifacts`，然后以 Media 内容块作为输入提交 |
| 设备 → 云 | Capability 处理函数调用 SDK 的 `UploadArtifact`，结果中返回 Media 引用 |
| 云 → 设备 | Capability 参数中携带 Artifact ID，处理函数调用 SDK 的 `DownloadArtifact` |
| 沙箱 ↔ 工件 | 沙箱能力 `import_file`（工件 → 工作区）与 `export_file`（工作区 → 工件），由控制器在沙箱外完成 |
| 下载 | `GET /v1/artifacts/{id}` |

沙箱本身仍然没有网络，也不持有凭证（ADR-0008）。文件由控制器经容器运行时复制进出沙箱。

## 4. 对模型的呈现

模型看到的 Artifact 是一行文本，例如 `[artifact art_x: chart.png, image/png, 7.6 KB]`。模型把其中的 ID 传给下一个工具，就能在设备与沙箱之间接力处理文件。

用户消息中的图片 Artifact（≤ 4MB）会内联给模型，以支持视觉理解。工具结果中的图片只以文本呈现，因为多数模型接口不支持在工具结果里放图片。

## 5. 限制（S2）

- 单个 Artifact 最大 512MB。
- M2 的 HTTP API 仍无鉴权（同 M0），Artifact 下载不做访问控制。鉴权方案确定后再补。
- 删除随 Session 一起进行，保留期由业务线配置（[Session 生命周期](./m2-session-lifecycle.md)）。
