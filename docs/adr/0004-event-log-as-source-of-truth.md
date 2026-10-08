---
status: accepted
---

# Event 日志是 Run 状态的唯一事实源

Session 内所有输入、模型输出、Capability 调用与结果都作为 Event 只追加地写入日志，Run 的全部状态可由日志重建；执行 Run 的 Worker 不持有状态，以租约认领 Session，故障后由其他 Worker 重放恢复。多端同步即多端订阅同一 Event 流。选择此方案是为了同时获得线性扩容、小时级任务的故障恢复、多端一致与可回放评测，代价是需要自行处理日志压缩、快照与 Capability 调用的幂等。
