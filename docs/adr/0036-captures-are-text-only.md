---
status: accepted
---

# 旁听只收文字，信任按说话人区分

全天佩戴的录音外设（领夹）经手机上的 HostApp 接入，把 EndUser 与他人的日常对话交给 yanshi 做整理、项目归集与复盘。yanshi 只接收转写后的文字片段（Capture），每段带 HostApp 给出的说话人标签（佩戴者、他人）；音频、转写与声纹识别都在 yanshi 之外完成。外设本身不是 Device，也不是 Node：yanshi 只看到那个 HostApp。

声纹是不属于任何 Session 的生物特征，Janitor 无法按 Session 删除它（ADR-0015）；全天的音频流也远大于 Call 的规模（ADR-0027 中继的是一段通话）。不进 yanshi，"只留文字、不留原声"由边界保证，而不是靠"转写完即删"。

Capture 作为 Event 进入 Session，不触发 Run：它不是对 Agent 说的话，每段都开 Run 在语义与成本上都不成立。Run 由明确的输入（按键提问、口述笔记）或业务线服务端按时间表提交的整理输入开启。

信任按说话人区分：佩戴者的话算 EndUser 本人的输入，他人的话按外部内容对待（与界面上下文同等）。读过 Capture 的 Run 一律经过注入判断（ADR-0030），高风险 Capability 一律需要 Approval，不论说话人是谁；Memory 写入闸门（ADR-0023）把他人的话算作外部内容。

## Considered Options

- **音频经网关中转，由 yanshi 转写**（同 Call）：能统一转写质量，但 yanshi 要承载全天音频并保管声纹，删除与合规边界都要扩大。
- **所有 Capture 都算外部内容**：最安全，但"承诺""偏好"这类从佩戴者原话中得出的记忆每条都要审批，产品不可用。
- **每段 Capture 是一条输入**：沿用现有路径，但全天成百上千个 Run，且模型会把旁听当作指令。

## Consequences

- 说话人标签由 HostApp 给出，yanshi 无法验证：标错时他人的话会被当作用户原话。注入判断与高风险审批不依赖标签，是这一风险的兜底。
- Capture 不自动进入 Transcript，由内置工具按时间段分页读取：一天的转写可能超出上下文窗口，也会破坏请求前缀的稳定。
- 未通过 Moderation 的片段只写不含文字的占位，不拒绝整批；Moderation 不可用时拒绝写入（ADR-0021），由 HostApp 缓冲后按 `capture_id` 去重重试。
