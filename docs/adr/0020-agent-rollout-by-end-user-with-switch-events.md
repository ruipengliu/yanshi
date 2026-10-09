---
status: accepted
---

# AgentDef 灰度按 EndUser 分流，回滚以事件切换版本

新 AgentDef 版本的发布状态（稳定版本、灰度版本与比例、撤回的版本）放在仓库文件 `agents/releases.yaml` 中，运行时定期重新加载。创建 Session 时按 `hash(Agent 名, EndUser)` 稳定分流，版本在 Session 中固定。撤回一个版本时，固定在它上面的 Session 在下一个 Run 开始时切换到稳定版本，切换记录为 `AgentSwitched` 事件，Session 当前使用的版本由日志投影得到。

我们没有选择数据库加管理接口：它能让多进程立即一致，但需要新增平台管理员身份；而版本在 Session 创建时固定，十秒级的配置不一致无害。也没有选择"Session 永远固定在原版本"：回滚时有问题的版本会继续服务已有会话直到它们结束，对长期存在的会话不可接受。切换只发生在 Run 之间，进行中的 Run 的上下文、调用与审批都基于原版本，不做中途切换。

## Consequences

- Session 的当前版本不再总是 SessionCreated 中的版本；读取版本须使用投影（`State.Agent()`）。
- 撤回版本的 AgentDef 文件须保留到其 Session 都已切换或结束。
- 发布配置随代码评审；改比例与回滚不需要重启，但需要把文件变更分发到各进程（如 ConfigMap）。
