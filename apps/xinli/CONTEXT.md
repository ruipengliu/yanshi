# xinli 应用层术语

这是 xinli 应用层的术语表；内核术语见根 [CONTEXT.md](../../CONTEXT.md)，知识领域术语见 [Persona 术语](../../persona/CONTEXT.md)。职责依据见 [Persona 架构](../../docs/design/persona-architecture.md) §2.2、§4。

**Orchestrator（编排器）**:
消费内核出站流、完成跨 Session 扇出的应用层服务：归集项目、推进每周回顾、通知 Persona 按依据级联删除、把回复与提问交给渠道或推送。只读出站流中的 ID，再按需读事件；按事件 ID 去重，重复处理幂等。Session 内的触发与定时由内核负责（设计 §4.5）。
_Avoid_: Worker、调度器（指内核调度时）

**BFF（前端专用后端）**:
面向 xinli App 的应用层后端，组织产品页面所需的查询与用户操作，调用 Persona 的查询与改动 API 及内核公开 API；知识语义由 Persona 负责，Agent 执行由内核负责（设计 §2.2）。
_Avoid_: 内核网关、Persona

**Capture Service（采集服务）**:
接收 HostApp 上传的音频分片，完成流式识别、说话人区分、声纹比对与个人词库热词处理，再以服务令牌向内核提交 Capture 的应用层服务。内核只收文字；音频默认只在识别完成前缓存，故障时由 HostApp 缓冲（设计 §4.1）。
_Avoid_: Capture（那是提交的旁听文字）、Node（那是能力执行端点）
