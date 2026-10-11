---
status: accepted
---

# 服务端故障注入只在调试部署中开启

调试控制台原先只有设备侧的故障（断网、下线、执行中崩溃、不回应），它们经公开协议就能模拟。执行语义里更重要的部分在服务端：模型调用失败与重试、流中途中断、Worker 崩溃后租约过期被接管、内容安全不可用时不放行。这些只有 `internal/sim` 在确定性模拟中覆盖，在真实进程上看不见，也无法手工复现（[控制台设计](../design/console.md) §7）。

## 决定

1. **`serve -faults` 开启服务端故障注入，默认关闭。** 开启时装配 `internal/faults`：模型提供商（在 `model.Guard` 之内）、内容安全提供商、设备目录的查询各包一层，Worker 在每一步之后检查是否"崩溃"。没有规则时这些包装不改变任何行为。
2. **故障类型与 sim 对应**：`model_error`、`model_rate_limit`、`model_stream_cut`、`model_delay`、`worker_crash`、`moderation_down`、`catalog_error`。注入的是与真实故障相同的错误（`ProviderError`、`ErrStalled`、`moderation.ErrUnavailable`），走与真实故障相同的处理路径。
3. **规则经 `/v1/debug/faults` 管理，只在 `-faults` 时注册。** 规则可以限定 Session 与次数。用户令牌只能对自己的 Session 注入、只看到这些规则；不限 Session 的规则只有服务令牌与 `-auth none`（只允许回环地址）可以添加。`/v1/debug/stats` 返回本进程的 `yanshi_*` 指标。
4. **规则只在进程内存中，只影响本进程。** 注入的每一次记录规则、类型与 Session ID，不含任何内容（ADR-0015），并计入 `yanshi_faults_injected_total`。

## Considered Options

- **只做设备侧故障**：保持"控制台没有调试专用的服务端接口"，但服务端的执行语义只能在 sim 中看到。
- **按环境变量或配置文件注入**：要重启进程，不能在观察一次交互的过程中按需注入。
- **注入存储故障（追加生效但报告失败）**：sim 已经覆盖；在真实数据库上注入需要包装日志与队列的事务接口，代价与风险都更高，暂不做。

## Consequences

- 调试控制台的原则修订为：只使用公开协议与 HTTP API；**故障注入与进程指标**是唯一的调试专用接口，且只在 `-faults` 时存在。
- 上线检查清单要求生产部署不带 `-faults`。
- 注入的崩溃要等租约过期才被接管（`serve -lease-ttl`，默认 10 秒）；控制台据此提示等待时间。
