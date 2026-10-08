---
status: accepted
---

# Memory 默认归属业务线，经 EndUser Grant 跨业务线共享

EndUser 的 Memory 默认只对产生它的 BusinessLine 可见；其他 BusinessLine 读取它必须有 EndUser 显式给出的 Grant。我们没有选择"全局统一画像"（合规风险高，用户不可控），也没有选择"完全隔离"（无法发挥多业务线的个性化价值）。

## Consequences

- 每条 Memory 必须记录来源 BusinessLine，Grant 可撤销，撤销后立即生效。
- EndUser 必须能查看、删除自己的 Memory 及已给出的 Grant。
