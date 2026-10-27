---
status: superseded by ADR-0050
---

# Capture 正文移出事件日志，按引用擦除

> 已被 [ADR-0050](./0050-capture-text-in-the-business-line.md) 取代：旁听原文存放在业务线，本文的正文存储、展开器与擦除不再实施。

两个需求撞上了事件日志只追加（ADR-0004）与按 Session 删除（ADR-0015、ADR-0037）：

- **部分删除**：用户删除今天的某一段谈话，原文必须物理删除，不只是隐藏。现在只能删除整天的旁听 Session，而且 `captures` 工具的结果在日志中复制了一份原文。
- **容量**：按 100 万 DAU，每天约 500 GB 原文。到期删除是 Janitor 逐 Session `DELETE`，每天删除与写入同量的数据，表膨胀与 vacuum 会成为瓶颈。

## 决定

（设计见 `docs/design/m7-kernel-primitives.md` §2–§4）

1. **正文单独存储。** `capture_segments` 按 Session 创建的日期分区、按 Session 分片，与 `CaptureAppended` 在同一事务中写入；日志只保留说话人、时间等元数据。与工件同理：日志决定有哪些片段、谁说的、是否被擦除，存储只保存文字。
2. **`captures` 的结果只存引用**。Transcript、来源推导（写入闸门、注入判断）、`cite_captures` 与读取 API 都经同一个展开器按日志前缀解析文字：版本取前缀中最新的修订，前缀中已擦除的一律是占位。原文只有一份。
3. **擦除**：按引用范围，先追加 `CaptureErased` 并中断活跃 Run，再物理清空正文的所有版本。声明了流的 AgentDef 必须 `context.scope: run`，所以之后的 Run 不会从旧回复或压缩摘要中看到被擦除的内容。出站流把擦除通知 Persona，Persona 以墓碑拒绝迟到的提交。
4. **原文修订**：追加 `CaptureCorrected`，新文字作为新版本写入存储；说话人不变；依赖旧版本的注入判断重做。
5. **删除仍逐 Session 精确执行**，分区只用于回收空间：分区中的 Session 全部删除后整片删除，不靠 vacuum。声明了流的 AgentDef 必须有开放时间上限（Retention 的 `close_after_open`）与有限的保留期，分区寿命因此有上界。

## Considered Options

- **把旁听 Session 切得更细**（每小时一个）：删除粒度仍是一小时，跨 Session 读取打乱每日整理与读取位置。
- **在日志中原地改写事件**：破坏只追加与 fencing 的前提，回放与模拟测试的确定性无从保证。
- **加密销毁**：ADR-0015 已因私有化环境的密钥管理否决。
- **留在日志中，只做逻辑隐藏**：不满足个人信息的删除要求。

## Consequences

- 新存储要通过一致性套件 `capturetest`，在 `app.SessionData` 登记，接入删除不变量与差分测试。
- 擦除不改写日志中已有的模型回复与压缩摘要：`scope: run` 使它们不再进入上下文，它们随旁听 Session 删除；交给业务线的派生数据由 Persona 按依据级联删除，生效有出站流的消费延迟。产品的隐私说明须写明。
- 擦除与修订是"请求前缀稳定"的显式例外：使该 Session 之后的请求前缀变化一次。
- 物理删除分两层写进部署文档：行在 Janitor 完成时不复存在；磁盘空间在分区删除时回收。
- 不读取 P1 之前写下的旁听日志，开发数据库须清空。
