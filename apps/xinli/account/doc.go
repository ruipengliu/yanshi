// Package account 实现 Q103、Q104、Q120 的应用账号边界。
//
// 测试身份通过 -test-identities-file 或 -test-identities-env 显式配置，JSON 项为
// business_line、subject、code_env。code_env 引用至少 16 字节的测试秘密；未配置时
// 登录关闭。这里只提供固定测试身份，不包含短信、第三方登录或声纹注册。
//
// BFF 角色打开应用库并挂载 /v1/auth/。访问凭据默认 15 分钟，刷新凭据默认 30 天；
// 刷新一次性消费旧凭据，同一安装实例重新登录撤销旧会话。POST login/refresh、
// POST logout/logout_all、GET account、DELETE devices/{id}、DELETE account
// 均在此路径下。注销返回只读 deletion_token，之后通过 Authorization 请求
// GET /v1/auth/deletion；该凭据不能访问产品数据。Service.Deletion 供受信后端按
// owner 恢复进度查询，删除响应丢失时不能依赖已经撤销的登录态恢复产品访问。
//
// SetEntitlement、Disable 与 Profile 是受信应用 API，不挂载管理权益的客户端路由。
// 权益版本递增与 A8 任务入队同事务；Worker 在 owner 互斥内认领并重新检查版本和
// active 状态。400 变为 failed/configuration，由 task.Store.FailureCount 计数；
// 空档位调用内核 DELETE 回默认。客户端读取的是上一次内核确认的档位名称。
//
// 注销状态和删除任务同事务建立。任务逐步推进 Persona 屏障、Persona 分批清理、
// 内核注销请求、内核完成轮询、应用清理。最后的事务删除身份绑定、会话、设备、
// 创建意图、命令占用、编排任务和来源收据，只保留账号终态与注销记录。Persona
// 与内核步骤可重复；保存结果失败会退避并重做，应用清理与 completed 原子提交。
//
// orchestrator 角色恢复账号任务；独立 BFF 进程只生产持久任务，必须另有该角色运行。
// PG 的 owner 互斥使用连接级 advisory lock，运行连接必须直连或使用 session pooling，
// 不能使用 transaction pooling。网络调用期间没有开放 SQL 事务，但会占一个连接；
// 同 owner 回调必须顺序执行并传递 context，不能并行使用其中的 PG 连接。
//
// 边界：内核档位接口没有版本条件写入。互斥保证存活应用进程间的版本检查与调用
// 顺序，无法证明进程崩溃、连接断开后内核中的旧 HTTP 请求已经停止。若要求这种
// 迟到远端提交也绝不覆盖新版本，需要内核增加版本 fencing；本任务不修改内核。
// B1/B2 还需把 Middleware 接到全部产品路由；长流应逐次重新鉴权，不能在整个流
// 生命周期持有 owner 锁。A13 的采集操作通过 WithActive 包裹完整写入。
package account
