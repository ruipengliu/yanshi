# P4-B7：单库完整一天验收

对应 [Persona 架构](./persona-architecture.md) §6.1、§11 B7 与 Q122。此处的“单库”指每一层只有一个 PostgreSQL database：内核、Persona、应用各自独立，使用同一个 PostgreSQL 实例；不包含 C6 的主备切换。执行正确性由脚本场景判定，Ark 结果另记报告。

## 结构与隔离

驱动在 `internal/day`，仅含测试文件，不 import `persona/` 或 `apps/`。父进程用 `app.Postgres`、`app.NewCore` 装配内核、两个 Worker、Janitor，并通过真实 HTTP 暴露内核 API。选择进程内内核是为了直接注入同一 `clock.Clock`，不另建执行语义。Persona、编排器、BFF 分别运行构建好的 `xinli serve -roles <角色>` 子进程；通过 kill 和重新启动验证进程级恢复。采集发送端也使用独立进程，每次命令重新打开同一持久文件。

| 文件 | 职责 |
| --- | --- |
| `internal/day/environment_test.go` | 三库、迁移、内核装配、HTTP、角色子进程、登录与真实时间刷新令牌 |
| `internal/day/control_test.go` | 共用业务偏移、HTTP 故障代理、Persona PG TCP 代理、精确崩溃屏障、fake 发送观察 |
| `internal/day/diagnostics_test.go` | 失败时 stderr 尾部、脱敏、子进程时钟及三库状态快照 |
| `internal/day/lifecycle_test.go` | 恢复边界的真实读取、新轮 file_day 接受事实、租约接管与阶段收敛 |
| `internal/day/schedules_test.go` | 定时真实 HTTP 契约、当地钟点与 UTC 到期、唯一触发及收件推送回归 |
| `internal/day/model_test.go` | 专用脚本模型；只从模型收到的工具结果读取引用、实体 ID 与 rev，按真实工具契约提交 |
| `internal/day/model_regression_test.go` | 平台渲染、真实 Transcript 与脚本联动；防止跳过原文变更集就结束 Run |
| `internal/day/scenario_test.go` | 上午到自然到期的阶段驱动与四层断言 |
| `internal/day/faults_test.go` | Persona 故障、消费者崩溃、72 小时过期恢复、长事务 |
| `internal/day/mcp_test.go` | 擦除后旧 Session 的迟到提交 |
| `internal/day/ark_test.go` | 同一主干的真实模型运行、只含状态与计数的报告及基线比较 |
| `apps/xinli/cmd/xinli/day_enabled.go` | 仅 `daytest` 制品中的控制接缝和持久发送端命令 |

内核使用 `YANSHI_TEST_PG` 指向的 database；驱动创建或复用 `yanshi_day_persona`、`yanshi_day_app`，每次测试各建独占 schema，退出时删除 schema。需要 URL 形式的 DSN，以及创建 database/schema 的测试账号权限。不会删除这两个 database；不应将验收指向业务生产数据库。Persona 故障通过专属 TCP 代理断开现有连接，其他工作树和数据库不受影响。

## 时钟与生产保护

初始业务时间为 `2035-01-08 09:00 Asia/Shanghai`。父进程维护原子偏移，业务时间为 `time.Now()+offset`；前拨同时影响内核以及所有 xinli 角色。推进器拒绝偏移减小，子进程每次取时通过带随机 Bearer 凭据的回环 HTTP 获取同一偏移，也拒绝回退。时钟不是冻结的纳秒时间，阶段之间保留真实经过时间；断言不依赖精确时间戳或并发提交顺序。

Persona 存储、MCP 业务判断、查询、命令、编排任务/调度/到期、BFF 与出站消费者/发送 Worker 都使用该业务时钟。验收扫描 tick 和任务重试缩短为 100ms；网络超时仍按真实时间。内核 Janitor 的删除 Step 在后台运行，保留清理 Sweep 在拨钟时显式执行，便于检查 72 小时边界。

认证使用真实时间：内核 JWT、应用登录/刷新、MCP 凭据不会随拨钟过期。`mcpserver.Options.AuthNow` 分离凭据检查与业务时间，未配置时保留既有 `Now` 行为，并有独立回归测试。账号服务与账号任务租约仍按真实时间，此场景不覆盖账号注销或档位切换。

普通 `make build` 的 `bin/xinli` 不包含测试时钟、屏障或发送端命令，参数解析直接拒绝 `-test-mode`、`-test-control-url`。`make day-bin` 单独生成 `bin/xinli-day`，同时满足以下条件才启用控制：编译标签 `daytest`、`serve -test-mode -test-control-url <地址>`、控制地址为 HTTP 回环 IP、业务与健康监听均为回环 IP、环境变量 `XINLI_DAY_CONTROL_TOKEN` 至少 32 字符。控制服务由父测试创建，不是生产调试路由；子进程没有推进接口。缺少时钟或收到回退偏移即失败，不回退到本机日期继续写入。

普通构建的保护测试进入 `make check`；专用构建的参数矩阵与时钟回退测试由 `make day-bin` 执行。

## 失败现场与调度诊断

测试失败或阶段等待超时时，在 kill 子进程和关闭三库之前输出一次现场。每次启动的 Persona、编排器、BFF 都用独立 BEGIN/END 段输出 stderr 最后 200 行（单行至多 16KiB），没有日志时明确显示 empty；输出前移除当前令牌、环境中的凭据、Bearer、JWT 与 URL 用户信息。保留每一代进程的 PID、退出状态、启动时真实/业务时间及偏移，便于区分计划停机、意外退出和重启后的故障。

仅隔离测试制品的健康端口增加带同一控制凭据的 `GET /debug/day`。它返回该进程重新读取父时钟之后的当前值、首次/当前偏移、读时钟次数及调度/EOF 开关。驱动在 ready 后和每次拨钟后核对仍在运行的角色时钟；偏移不是启动时缓存一次。失败时另读 `?schedule=1`，以真实 `schedule.DayTimes`、稳定抖动和 Persona 元数据展示白天/日终到点时刻；父进程记录扫描 owner 发现的开始、完成及结果。诊断路由不进入生产制品。

数据库快照只选择状态和身份字段，包含 `accounts` 状态、调度 owner/注销屏障、`task_sources`、`orchestration_tasks` 的状态/租约/`retry_at`/错误类别/退役次数、Session 创建意图、Persona 业务日最后接收时间/固定时区/进度/原文版本/期限/绑定，以及工作绑定和命令状态。工作绑定与命令状态通过存储的 `payload.Value` 读取；此前漏掉包装层输出的 null 不代表未绑定。HTTP 按 method/path/status 聚合累计计数，最多保留 200 个组合，其余合并为 `/other`；outbox 轮询不会再挤掉创建 Session 的证据。不留 query、请求/响应体和 Authorization。每次数据库诊断读取限时 2 秒，故障库不可访问时继续输出其他现场。

工作 Session 的事件发现合并应用任务的当前身份、Persona `work_sessions` 主键及业务日 `Sessions` 历史，覆盖已经退役并清空任务 session_id 的 Run。每个事件打印种类、seq、Run/Call ID、工具名及结果 IsError。按本次合成场景的诊断要求，脚本版额外打印工具错误或业务拒收文字前 200 字、Run 失败原因前 200 字，以及脚本实际收到的工具名称列表和发出的调用参数；凭据仍脱敏。脚本观察只保留最近 200 次响应并在失败时输出。Ark 版继续只输出状态和类别，不打印工具正文或参数。

A10 没有独立持久化的“下次扫描”或“下次白天到点”表：启动立即扫描，生产每 30 秒、验收每 100ms 扫描；正常到点由业务日元数据重算，失败重试时间在任务的 `retry_at`。发现查询直接枚举 Persona `owners` 并排除 `owner_deletions`，成功采集事务已经建立 owner，不依赖 `domain_changes` 或已有任务。静默从服务端最后正式接收时刻计算，不取片段里的历史说话时间。

第一次 Mac mini `day-short` 在约 46 秒超时，`morning-upload` 的 `tasks=0` 位于编排器启动前。第二轮现场确认 A10 已建任务，任务在一次业务执行后的对账认领中变为 pending/no_progress，按 5 分钟业务时间退避。明确根因在脚本：`runtime.Transcript` 的 `model.ExpandCaptures` 已把结构化片段渲染为 `#1.0 08:30 [佩戴者] ...` 文字，但脚本仍用 `GetCapture()` 找片段，恒得空页，从而跳过全部 `file_changes`、交 `file_day` 后结束。Run completed 不代表处理位置前进；A10 按规则退役当前绑定，所以 Session 为空而历史 `Sessions` 保留 `ses_id-1`，并非 CAS 绑定或提交前命令登记失效。

脚本现从模型真实收到的文字解析引用和片段，工具错误及无法识别的非空页直接失败，不能再当空页整理。回归测试经平台 Render、真实 Reduce/Transcript 验证两个时段分别交 `file_changes` 后才交 `file_day`。此前的环境继承缺口仍保留修复：启动固定 `-auto-schedule=true -exit-on-stdin-eof=false`，缩短场景设置相反环境值验证覆盖；它不是第二轮 no_progress 的根因。

第三轮远端反馈：缩短版通过（1.93 秒），长事务用例通过；完整版在数据库恢复后的首个业务日 GET 收到 503。源码摘要 `db310a3f45245f7e` 的现场显示，14:00 阶段开始仅约 36ms，Persona/BFF 均存活，业务日仍是版本 2、进度 2.0。`tcpProxy.cut(false)` 只恢复连接准入，不代表两个进程的 PG 池已清理被断开的旧连接；PG Store 每次请求重新借连接，BFF/Persona 没有负缓存。故障在场景把“撤除故障”误当“已恢复可服务”，并未证明实现不能自愈。

恢复边界现在分别等待 BFF `GET /v1/days/{date}`（Persona 进程的 PG 池）和 `GET /v1/captures/sync`（BFF 进程自己的 PG 池）返回 200，再校验原文版本和处理位置未前进。只读恢复轮询仅重试网络不可达/503，每次请求限时 2 秒、总等待 45 秒；其他状态立即失败。回滚、采集、擦除等提交仍不自动重试。重启收到地址后也要轮询 `/readyz`，进程退出立即失败；Persona/BFF 重启后再验证真实 BFF 查询链路。

阶段完成不再仅看“processed + 非空总览”：总览可能来自上轮。驱动检查当前 active 接受事实，要求 summary 的 Session/FiledAt 与事实一致、接受时间不早于最后批次接收、CommandID 能关联本日 day/rebuild 编排任务。通知前崩溃时无需 app task 已成功来证明事实接受。强杀后先在编排器停止时把业务时钟拨过持久 retry_at/lease_until，再启动接管；不靠真实 45 秒等待两分钟业务租约。进入下一故障或普通重启前，等待非终态任务、未确认领域变化、待处理接收记录、相关工作 Session 的活跃 Run、出站追赶及发送任务收敛。擦除后的重处理也必须完成本轮 file_day 和归集；Ark 报告单独记录该结果，再比较自然到期前后的账本。

第四轮远端反馈：源码摘要 `0b9171e8d262c9b8` 已通过数据库恢复、通知前退出恢复、用户修改与午夜日终，随后创建定时返回 400。此时各进程业务时间均为当地 01-09 04:31，09:00 尚未过去；根因是剧本将 RFC3339 `2035-01-09T09:00:00+08:00` 用作 `at`。内核要求 `at` 是不带偏移的当地钟点，`time_zone` 单独提供 IANA 时区；HTTP 的 `input` 则是字符串，由内核转换为 Text 块。BFF 原样转发符合契约，不做格式转换。脚本版与 Ark 版现共用当地钟点声明，并在拨钟前检查创建响应、列表、ScheduleCreated 的 UTC 到期为 01-09 01:00Z。脚本版还检查唯一 ScheduleFired 与 RunRequested 配对。B2 单测改正带 Z 的 at 和缺 BYHOUR/BYMINUTE 的 RRULE，核对完整转发字段；独立三库 `TestDayScheduleHTTP` 经真实 BFF 和内核校验非法声明仍返回 400、无持久有效定时，合法声明触发并完成收件与 fake 推送。

第五轮反馈：定时契约测试通过；整日场景通过助手定时、两处消费者崩溃恢复与角色重启，在擦除后的迟到 MCP 拒收断言失败。请求的服务凭据和 `_meta` 使用真实旧 Session，擦除也已清空当前绑定。实际调用在 handler 前就被拒绝，但 `CheckWorkSession` 先读到 pending 删除待办而返回“前一命令尚未结束”；MCP 又丢掉资格错误状态，以 `IsError=false` 返回文字。没有发现迟到事实被接受；缺陷属于资格拒收的错误分类和 MCP 表达，旧测试的模糊关键词也不足以验证这条保证。现在先判断日绑定已停用，再查询目标可用性；前置与六个写 handler 的资格错误保留结构化原因并返回 `IsError=true`。整日驱动先核对历史绑定/登记命令身份，要求明确 inactive/retired 错误，并比较业务日与承诺未变化。新增内存/PG 的真实认证 MCP HTTP 回归：相同连接先合法接受，再覆盖擦除 pending、删除完成、重建换绑、直接 CAS 退役、未登记和命令完成；六个写工具均拒收，完整 Book 与通知计数不变，新绑定与下一条合法命令仍可成功。

## 阶段与断言

每个阶段重新比较 Persona 状态与 BFF：业务日处理位置、`capture_revision`、原文页、交谈内容，以及承诺 ID、内容、期限与 rev；比较应用任务、收件箱与 PG；已发送的通知任务必须有 fake 发送观察；内核出站信封必须对应已经提交的 Event，完成信封的 Run ID 必须一致。跨库没有共同读事务，观察等待顺序读取收敛。常规输出只有阶段、ID、状态和计数；合成脚本失败诊断的额外内容范围见上一节。

| 阶段 | 操作与执行断言 |
| --- | --- |
| 上午采集 | 08:30 报价、08:50 预算；提交前断网不入库；先传第二批只暂存、不分配引用；首批提交后丢响应，发送端重开文件补传；同 ID 同引用，确认后本地正文清除，异文返回 `capture_conflict`；上传不推进处理位置，原文版本为 2 |
| 白天静默 | 前拨至 09:10，A10 自动提交“整理今天”；脚本 `captures → find_related → file_changes → day_ledger → file_day`；处理完成且版本为 2，产生两项承诺、日总览；领域变化扇出至少两个成功归集任务，项目现状与下一步通过 BFF 可见 |
| 下午 Persona 故障与改期 | 14:00 上传报价改期；断库、回滚均不推进版本/处理位置；恢复后版本为 3；冻结领域变化扫描，在 `file_day` 提交后、通知前 kill Persona；持久领域变化仍在，重启后继续归集；报价改到 01-10 且 rev 增长 |
| 用户修改 | BFF 按 rev 将报价改为 01-12，rev 再增长，后续重启、擦除及到期均保留该用户修改 |
| 午夜与日终 | 23:58 与次日 00:01 追加闲聊；跨日引用从 `1.0` 开始；前拨到 04:31，两个业务日分别处理到版本 4/1，`schedule_final` 任务成功，旧日仍有总览 |
| 助手 | 真实查询工具后提问、BFF 回答、真实内核审批与 BFF 批准；高风险 `approval_probe` 是仅测试注册的无副作用本地能力；纯查询重答生成新 Run，`RedoOf` 指向原 Run，旧 expected Run 返回 409；Event 中查询成功且含账本 ID/用户改后的期限 |
| 定时 | 在问答线程用当地钟点创建 09:00 定时；提交前检查仍在未来，响应/列表/事件的到期为 01:00Z；前拨至 09:01，唯一 ScheduleFired 与 RunRequested 的 Run/到期一致并完成；应用收件箱一项、通知任务 sent、fake 恰好发送一次 |
| 消费者崩溃 | 接收后落库前、持久接收后处理前分别 kill 编排器；验证游标、receipt、待处理状态，重启后任务与通知不丢 |
| 三角色重启 | 分别 kill 并重启 Persona、BFF、编排器；使用 PG 中登录态、账本、任务继续，用户修改不丢 |
| 擦除 | 擦除预算原文 `#2.0`；版本增长、处理位置失效；删除待办未执行时 BFF 显示删除中；旧批次与旧 Session 的迟到变更集拒收；完成后原文删除、内核工作 Session 返回 404 且日志为空，剩余原文重新处理 |
| 消费者超过 72 小时 | 消费者停止期间产生并解决审批、再完成查询；确认信封可发布后建立保留观察点，前拨到 01-13 并 Sweep；旧游标明确返回 410 `outbox_cursor_expired`；重启后 B5 新恢复轮对账至 follow，完成通知补齐，过时审批任务取消且未推送 |
| 自然到期 | 前拨到 01-18；Persona 所有原文清除、旧工作 Session 日志清空、BFF 原文页 410/业务日 expired；报价 ID 与用户期限保留；重投过期批次被拒收且不复活原文 |

这里只检查执行契约，不检查总结文笔、抽取质量或模型选择。脚本实现独立于控制台通用 `script.Provider`，助手使用通用脚本命令；未修改生产提示词或 AgentDef。

## 三类故障的边界

| 类别 | 注入点 | 必须保持 |
| --- | --- | --- |
| 断网补传 | BFF 前 HTTP 代理返回 503；上游成功提交后丢掉响应；发送端进程重新打开文件；乱序、重复、跨日、擦除后与到期后重投 | 未提交不落库；已提交安全重试；引用稳定；异文冲突；已删/过期正文不复活 |
| Persona 故障 | 仅 Persona DSN 的 TCP 代理断连接；`store.Update` 回调末尾注入错误使真实 PG 事务回滚；`Notifier.DayFiled` 前屏障，另冻结 `PendingChangeOwners` 扫描，再 kill Persona | 未接受不推进；已接受的 `file_day` 与领域变化同事务持久化；恢复扫描接管未通知事实；擦除后的旧 Session 不再交回事实 |
| 消费者落后 | `recovery.Consumer.Commit` 前；`delivery.Store.Process` 前；停止消费者并拨钟/Sweep；独立 PG 事务取得 xid 后保持未结束 | 游标不能跳过未持久接收的信封；receipt 与游标一起提交；持久 receipt 待处理可重放；过期游标显式失败后由 B5 对账；过时审批不推送；长事务阻挡安全水位，未发布信封不会按创建时间误删 |

`TestDayLongTransaction` 在真实事务取得 xid 后通过普通输入产生 Run，不伪造 outbox 行。卡住水位超过 72 小时仍保留未发布信封；释放后可以读取；首次可发布观察后 71h59m 仍保留，72h01m 后清理，期间消费者能收到通知。

fake 是 B6 的真实 `delivery.Fake`。测试包装器只把通知 ID/种类传给父进程，所以子进程重启后仍能检查发送次数；应用 PG 的 `delivery_state.Jobs` 保存持久发送状态与尝试次数。父进程观察记录不是新的生产发送历史表，也不模拟 APNs 或其他真实渠道。

## 运行

按本机开发约定，在本工作树通过已有 `dev` 入口同步到 Mac mini。首次运行先格式化本任务的改动；此任务交付时未在无 Go 的编码机执行格式化或测试。

```bash
dev fmt
dev check
dev test ./internal/day -run TestDayModel -count=1
dev run 'make day-short'
dev run 'make day'
```

在具备 Go 与 Docker 的机器直接运行对应 `make day-short` / `make day`。两个目标准备 Compose 依赖、构建独立制品并运行保护测试。默认 `make day` 执行完整 `TestDay`（含三类故障）、定时契约 `TestDayScheduleHTTP` 及独立 `TestDayLongTransaction`。每次均创建独立 schema，不复用历史运行数据。

缩短版覆盖上午持久发送、断网/响应丢失/缺批/重复/冲突、白天自动整理与项目扇出。它在 `make check` 中运行（`make test` 先构建 `bin/xinli-day`；Mac mini 上场景本身约 2 秒）。普通 `go test ./internal/day` 未设置 `YANSHI_DAY_BIN` 时显式跳过场景，不自动构建业务二进制。

单独复现故障：

```bash
dev run 'make day DAY_RUN=TestDayPersonaFaults'
dev run 'make day DAY_RUN=TestDayConsumerFaults'
dev run 'make day DAY_RUN=TestDayLongTransaction'
dev run 'make day DAY_RUN=TestDayScheduleHTTP'
```

迟到写入的 Persona 回归可独立运行（内存与 PG、真实 MCP HTTP）：

```bash
dev run 'cd persona && go test -race ./mcpserver -run TestProductionWork -count=1'
```

Ark 使用相同三库、HTTP、角色、录音夹具与主要阶段（断网补传、自动整理、改期和用户修改、跨午夜、助手查询/重答、定时收件箱、重启、擦除、到期）。真实模型提问/审批选择不是固定的，精确屏障与问答/审批契约由脚本版验证。需在远端配置 `ARK_API_KEY`，可选 `ARK_BASE_URL`；模型沿用仓库 `apps/xinli/agents/xinli-*.yaml`。

```bash
dev run 'make day-ark'
# 与仓库中经审阅的基线比较；有意的效果变化经人工审阅后，取回新报告覆盖 apps/xinli/evals/baselines/day-ark.json 并随代码提交。
dev run 'YANSHI_DAY_BASELINE=apps/xinli/evals/baselines/day-ark.json make day-ark'
```

报告默认写入 `eval-results/day-ark.json`，可用 `YANSHI_DAY_REPORT` 覆盖路径；相对路径按仓库根目录解释。报告包含阶段处理/归档状态、事实与原文计数、Run 完成/失败数量、收件箱/发送数量、各步骤 outcome，以及与上一份同格式报告的差异字段。差异只报告、不判脚本执行失败；模型超时、未抽取事实、无法重答也只记结果。构建、配置、HTTP/PG 等基础设施错误仍会使该入口失败。报告不保存原文、模型输出或工具参数。

报告新增 `diagnostics`，与既有 `stages/outcomes/differences` 分开，不参与基线比较，版本仍为 1。每个已观察阶段保存工具结果总数、`is_error` 总数及新增数、按工具名统计的错误；父驱动跨子进程重启累计测试制品发出的存储错误及编排告警类别（timeout/canceled/unavailable/sql_canceled/lock_timeout/operation_failed），并保存 BFF/Persona/内核 HTTP 状态计数。另读当前任务状态、最后错误分类、认领次数总和、退役数及至多 50 个未终态任务的 ID、操作、错误分类和 retry_at。认领包含正常终态轮询，不冒称失败重试次数；最后错误是当前快照，不冒称持久失败历史。告警信号为测试控制面的尽力传送，零计数不能证明从未发生错误；同一失败可同时出现于 store 与 orchestrator 类别，不将两者相加当独立故障数。HTTP 状态只统计正常结束的请求，尚未结束的 MCP/SSE 及有意丢响应不计入；MCP 工具错误可能封在 HTTP 200 内，必须结合工具与存储计数看。

`diagnostics.afternoon-review` 在下午 organized 返回后、用户编辑之前采集，即使 outcome=false 也记录，不再依赖 t.Failed 的诊断。其余阶段随既有 observe 采样；累积和阶段新增错误分开，已观察事件按 Session/seq 去重，擦除删除日志不会让之前观察到的错误计数归零。诊断只输出类型、工具名和计数；不保存工具参数、结果正文或错误原文。诊断读取失败用固定 `read_errors` 标记，不能把缺数据解释为零错误。工具/HTTP/任务读数非同一事务，作为执行诊断而非新 outcome 判据。

诊断字段的来由：Persona 正常操作曾被统一限制为 2 秒，Ark 的 `afternoon_review` 两次为 false（main 对照为 true）而 `failed_runs=0`；仅凭 outcome 无法区分模型效果与执行层错误。修正后的预算分层见 deployment §10.2，Ark 基线、outcome 与三分钟阶段观察均未改变。

首次没有基线时明确写 `not_recorded`。首份基线（2026-10-29）已经审阅，现存于 `apps/xinli/evals/baselines/day-ark.json`：执行相关的 outcome 均为真；`final_review` 与 `erasure_reprocessed` 为假，是真实模型在午夜日终与擦除后重整理时没有覆盖剩余的闲聊原文，处理进度未走到底（效果问题，按 Q122 只记录）；`ledger_retained` 依赖 `erasure_reprocessed` 因而同为假，但到期前后承诺数一致（1 → 1）；`settled_before_restart` 为假是重启前仍有在途工作，不影响之后各阶段。`dev run` 的产物留在远端，按本地开发文档取回或检查。

## 验收记录与边界

| 环境/源码摘要 | 检查 | 结果 | 耗时/报告 |
| --- | --- | --- | --- |
| 编码机，无 Go 工具链 | 静态检查 | 已检查装配/API/故障边界；未编译、未运行 | 不作为完成验收 |
| Mac mini，修订前 | `dev check` | 通过 | 未提供源码摘要 |
| Mac mini，修订前 | `make day-short` | 上午补传通过；整理等待超时 | 46.92 秒；失败不能作为 <60 秒通过证据 |
| Mac mini，加入首轮诊断后 | `make day-short` | 已建计划；脚本跳过 file_changes，终态对账后 no_progress/退役 | 约 47 秒；已据真实 Transcript 边界修脚本，待重跑 |
| Mac mini，修正脚本后 | `make day-short` | 通过 | 1.93 秒 |
| Mac mini，修正脚本后 | `TestDayLongTransaction` | 通过 | 未提供耗时 |
| Mac mini，`db310a3f45245f7e` | `TestDay` | 白天阶段通过；撤除 DB 故障后立即 GET 返回 503 | 1.50 秒；已补恢复和阶段等待，待重跑 |
| Mac mini，`0b9171e8d262c9b8` | `TestDay` | Persona 故障恢复、用户修改与午夜日终通过；非法带偏移 at 被拒绝 | 5.44 秒；已修剧本与 B2 契约测试，待重跑 |
| Mac mini，修正定时后 | `TestDayScheduleHTTP` / `TestDay` | 定时契约通过；整日通过助手、消费者崩溃与重启，迟到拒收错误表达未满足断言 | 9.20 秒；已修 Persona 资格错误通道并加强回归，待重跑 |
| Mac mini，`06803c5ad7357ce6` | `dev check`、`make day-short` | 通过 | 缩短版约 2 秒 |
| Mac mini，`06803c5ad7357ce6` | `make day`（`TestDay`、`TestDayLongTransaction`、`TestDayScheduleHTTP`） | 全部阶段与三类故障演练通过 | `TestDay` 12.46 秒，合计 19.2 秒 |
| Mac mini，`06803c5ad7357ce6` | `make day-ark` | 通过（基础设施无错误）；首份基线经审阅后提交，现存于 `apps/xinli/evals/baselines/day-ark.json` | 612.6 秒 |

本次不改 runtime/session/service 执行语义，不新增生产数据库表，不把测试屏障接入生产故障 API。工件正文使用内存实现，此剧本不验收 S3 或沙箱；这些仍由完整 `make check` 覆盖。真实多库固定槽位、故障提升、旧主隔离属于 C6；B7 通过也不关闭 P4。

## P4 关闭前的最终回归

C6 的隔离同步主备项目、内核 / Persona 提升驱动与双库容量入口见 [deployment §10](./deployment.md#10-p4-c6隔离同步主备切换与容量)。下表是关闭条件；本次结果见其后的记录。

| 检查 | 关闭条件 |
| --- | --- |
| `make check` | 全量 Go/Persona/xinli、存储一致性、Node/pnpm 互通与 Docker/S3 沙箱检查全绿；不以开关缺失的 skip 替代 |
| `make sim` | 单库、双库差分与删除不变量全绿；故障种子可复现 |
| `make day` | 完整一天、断网补传、Persona 故障、消费者落后三类故障及定时/长事务全部通过 |
| `make day-shards` | 现有双真实 database 专项 `TestDayShards`：部分可服务、恢复、跨片委托/删除、两片消费；它不是完整一天的双片版本（完整剧本仍有单游标假设） |
| `make failover` | 第四类故障：内核片 1 与 Persona 分别真实提升同步备库、旧主隔离、确认写入/引用/seq/游标不丢、未知结果安全重试；JSON 报告的每项 passed=true |
| `make shard-capacity` | 固定配方正确性通过，记录单/双库吞吐、p50/p95、每库写入与连接；无性能硬门槛，但不可省略实测报告 |
| `make day-ark` | 同一整日驱动运行 Ark，与 `apps/xinli/evals/baselines/day-ark.json` 比较并审阅差异；该入口的效果差异只记录，不自动判退出失败，不能仅凭退出码判无回归 |
| 相关 / 全量 `make eval` | `capture-*` 与 `memory-*` 分别对 xinli 与内核的基线比较，全量评测各套件使用各自基线；所有执行契约通过、无未解释效果回归。Q122 的效果记录不要求逐字相同；不自动更新基线来掩盖回归 |

在当前工作树依次执行；`dev check` 等价于远端完整 `make check`，`dev sim` 等价于 `make sim`。普通回归沿用已配置的共享依赖；C6 的启动/停止/配置变更仅操作 `yanshi-failover`，不对共享 Compose 项目执行 down、stop 或修改配置。

```bash
dev fmt
dev check
dev sim
dev run 'make day'
dev run 'make day-shards'
dev run 'make failover'
dev run 'make shard-capacity'
dev run 'YANSHI_DAY_BASELINE=apps/xinli/evals/baselines/day-ark.json make day-ark'
dev run 'make eval-xinli EVAL_FLAGS="-sandbox docker -case capture-* -xinli-store pg"'
dev run 'make eval-kernel EVAL_FLAGS="-sandbox docker -case memory-*"'
dev run 'make eval'
dev run 'cat eval-results/failover/kernel.json'
dev run 'cat eval-results/failover/persona.json'
dev run 'cat eval-results/failover/capacity-1.json'
dev run 'cat eval-results/failover/capacity-2.json'
dev run 'make failover-down'
```

Ark/eval 需要远端已配置的模型凭据；全量 suite 中的实时语音用例另按既有要求配置 `VOLC_SPEECH_API_KEY`。不要将缺凭据而跳过的用例写成通过。记录每条命令的源码摘要、退出状态、耗时和报告路径；有意效果变更需先审阅再另行更新基线。固定容量报告与两个切换报告填回 deployment 的表格后，才具备关闭 P4 的证据。

### 最终回归记录（2026-10-30，Mac mini）

源码摘要以 `dev` 同步时记录的为准；`b3d49d144a99b0a2` 为 C6 最终代码（含 Persona 超时分层）。

| 检查 | 源码摘要 | 结果 |
| --- | --- | --- |
| `make check` | `b3d49d144a99b0a2` | 通过（含 `TestDayShort`、`TestDayShards`） |
| `make sim` | `b3d49d144a99b0a2` | 通过（2000 内存种子、100 PG 种子、100 双库种子） |
| `make day` | `b3d49d144a99b0a2` | 通过：`TestDay` 12.41 秒、`TestDayLongTransaction` 2.69 秒、`TestDayScheduleHTTP` 2.08 秒 |
| `make day-shards` | `b3d49d144a99b0a2` | 通过，双库一天 7.17 秒 |
| `make failover` | `b3d49d144a99b0a2` | 内核、Persona writes、Persona deletions 均 passed；数值见 [deployment §10.2](./deployment.md#102-演练与结果记录) |
| `make shard-capacity` | `419431b4bdce0442`（容量路径与之后的 Persona 超时改动无关） | 通过；单 / 双库数值见 [deployment §10.3](./deployment.md#103-双库容量配方与报告) |
| `make day-ark` | `b3d49d144a99b0a2` | 通过，与 `apps/xinli/evals/baselines/day-ark.json` 比较 0 处差异，614 秒。此前一版把 Persona 正常操作限制为 2 秒，两次运行下午整理均未完成（以 main 对照确认），已按 deployment §10.2 的分层修正 |
| 全量 `make eval` | `b3d49d144a99b0a2` | 执行契约全部通过；报告的 7 项效果回归经对照判定为模型噪声（下表） |
| `capture-*`（xinli PG 存储） | `b3d49d144a99b0a2` | 执行契约全部通过；效果差异落在 multiday / workday 的既有 0/3 用例与噪声范围内 |

全量 eval 回归项的对照（每格为通过 / 3）：

| 用例 | C6 首次 | C6 复跑 | 当日 main 对照 | 判定 |
| --- | --- | --- | --- | --- |
| `capture-health-category` | 0 | 2（PG 存储） | 1 | 噪声 |
| `capture-project-grouping` | 0 | 0（PG）、2 | 1 | 噪声 |
| `device-ambiguous-ask` | 1 | 3 | 3 | 噪声（C6 不涉及设备路由） |
| `capture-multiday`、`capture-multiday-noisy` | 0 | 0 | 0（基线亦为 0） | 既有失败；逐轮指标在 main 对照的波动范围内 |

基线未更新。
