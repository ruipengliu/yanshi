---
status: accepted（2026-11-26 定稿）
---

# 业务线声明身份域，限制跨业务线 Memory Grant

> **已定稿（2026-11-26），随本次提交实施。** 来源：[架构评估评审](../review/2026-11-22-architecture-evaluation-review.md) §8 第 1 条。补充约束 [ADR-0003](0003-memory-owned-by-business-line-shared-by-grant.md) 的跨业务线 Grant 条件与 [ADR-0014](0014-business-line-signed-tokens.md) 的跨业务线身份边界；同步修订 [部署](../design/deployment.md) §1、[M4 Memory 与 Grant](../design/m4-memory-grant.md) §1 与 §4、[鉴权](../design/auth.md) §2。

部署单元允许承载多条业务线，其中 EndUser 的 Memory 与 Grant 按 EndUser 分片以便跨业务线共享（[部署](../design/deployment.md) §1、ADR-0003）。但令牌由各业务线分别签发（ADR-0014），内核直接取 JWT 的 `sub` 作为 EndUser（`internal/auth/auth.go`）。其他按 EndUser 存储的表（consent、usage、tier、push、session）主键都包含业务线，不跨线；唯有 Memory Grant 跨业务线生效。如果第二条业务线使用独立的本地用户 ID 且与第一条撞号，跨业务线 Grant 就会把业务线 A 的 EndUser 关联到业务线 B 的另一个人的 Memory，而且 `CreateGrant` 此前只检查 `from` 与 `to` 非空且不同，未登记的业务线名也会被接受。

## 决定

1. **业务线配置声明 `identity_domain`，默认独占。** `businesslines/<业务线>.yaml`（`auth.BusinessLine`）新增可选字段 `identity_domain`。非空时不得包含空白或控制字符（在 `auth.LoadDir` 与 `auth.NewJWT` 校验）。为空表示该业务线独占一个身份域，不与任何其他业务线共享；不采用"为空时等于业务线名"的字符串相等规则，防止业务线 A 省略声明、业务线 B 显式写 `identity_domain: A` 时被误判同域。失败即收紧：未显式声明相同非空身份域的两条业务线之间不能互相 Grant。
2. **判定放在 `memory.Service`，覆盖全部入口。** `memory.Service` 新增注入字段 `IdentityDomain func(businessLine string) (domain string, known bool)`，返回业务线声明的身份域与是否已登记（`domain == ""` 即独占）。两条业务线同域，当且仅当双方均已登记（`known == true`）且声明了相同的非空身份域。不把判定只放在 `httpapi`，使 Unrestricted 主体与其他内部入口同样受约束。`IdentityDomain` 为 `nil` 时不做检查（`-auth none` 开发模式、评测与模拟测试维持现状），装配沿用 `HealthAllowed` 的模式经 `app.Options.IdentityDomain` 由 `cmd/yanshi/serve.go` 注入。
3. **创建时严格拒绝。** `CreateGrant` 要求 `from` 与 `to` 均已登记且同域；任一方未登记或不在同一非空身份域时返回 `memory.ErrRejected`（HTTP 400）。错误信息说明须同一身份域、只包含业务线名，不泄露配置的身份域名或 EndUser。
4. **读取与列举按"只收紧不放宽"实时过滤。** `Scopes`（召回与 `memory_search` 共用）与 `ListGrants` 跳过任一方未登记或不在同一非空身份域的授权：配置变更使两条业务线不再同域后，既有授权立即停止生效且不再列出。`RevokeGrant` 不做身份域检查，便于用户或运维清理旧授权；`DeleteEndUser` 照旧删除涉及本业务线的全部授权。
5. **节点唯一键不改，列为残留风险。** 节点表的唯一键 `(end_user, node_id)` 不含业务线（`internal/pg/migrations/0035_nodes_end_user_key.sql`）。不同身份域的同名 EndUser 若恰好使用相同 `node_id`，后注册方只会被拒绝注册，不会读取到对方数据，本 ADR 不修改节点表结构。

## Considered Options

- **引入统一身份服务（OIDC 或平台签发全局用户 ID）**：违背 ADR-0014 与私有化部署基线（ADR-0001），各业务线的登录体系不同，私有化环境未必存在统一身份服务。
- **空 `identity_domain` 按等于业务线名做字符串比较**：实现看似简洁，但存在单方伪造同域漏洞——若业务线 A 省略配置，业务线 B 显式配置 `identity_domain: A`，两者会被判同域。
- **只在 `httpapi` 的 `createGrant` 中检查**：Unrestricted 主体或将来新增的内部调用方会绕过，且无法在召回 `Scopes` 与 `ListGrants` 路径拦截配置变更后的旧授权。
- **配置拆域时自动删除旧 Grant**：需要引入后台迁移或把配置态耦合进写路径，且运维误配拆域后无法恢复；读时与列举时实时过滤已经收紧权限，彻底清除交由撤销路径完成。

## Consequences

- 共享 Memory 的业务线必须在上线前声明相同的 `identity_domain` 并确认共用同一套 EndUser 账号体系（列入 [上线检查清单](../launch-checklist.md)）；不共享的业务线留空即可，默认隔离。
- 配置改动把原同域的两条业务线拆开后，旧 Grant 在 `Scopes` 与 `ListGrants` 中立即不可见、不生效；**若之后配置改回同域，未被撤销的旧 Grant 会恢复可见**。由于授权只能在同域时创建，接受这一后果；运维若要彻底清除，须显式撤销旧授权。
- 残留风险：
  1. `identity_domain` 是运维声明的信任锚，内核不验证两条业务线后端是否真正共享账号体系；
  2. 节点唯一键 `(end_user, node_id)` 不含业务线，不同身份域的同名 EndUser 若使用相同的 `node_id`，后注册方会被拒绝（不会跨线泄露数据）。

## 重审条件

任一出现即重新评估：

- 同一单元内承载不同账号体系的业务线出现跨身份域的节点 ID 碰撞（重审节点唯一键 `(end_user, node_id)`，改为包含业务线或身份域）；
- 账号体系不同的业务线之间需要共享 Memory，必须引入账号关联映射而非身份域声明；
- 私有化客户要求平台直接验证统一身份服务（OIDC 等，与 ADR-0014 一并重审）；
- 运维拆域后需要不可逆地立即清除全部旧 Grant，而读时过滤加显式撤销不够用。
