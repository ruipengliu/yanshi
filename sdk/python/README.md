# 外部循环的参考适配（Python）

业务线用自己的框架写 Agent、由平台驱动每一步时，照这里接入（契约见 [ADR-0062](../../docs/adr/0062-external-loop-contract.md)，模型经内核调用见 [ADR-0063](../../docs/adr/0063-loop-model-gateway-and-step-token.md)）。两个适配都通过一致性套件 `yanshi looptest -step-token` 的全部场景：

- `src/yanshi_loop/langgraph_loop.py`：LangGraph。
- `src/yanshi_loop/pydantic_loop.py`：Pydantic AI（平台工具声明为 deferred 工具）。

`wire.py` 解析每步请求、编码回答（不依赖 protobuf 库），`server.py` 是 HTTP 服务；换一个框架只需写一个 `decide(step, cfg) -> Reply`。

## 运行

```
cd sdk/python
uv sync --frozen
.venv/bin/python -m yanshi_loop --framework pydantic --port 8001 --model-base-url http://<yanshi>/v1/loop
```

在 `loops/*.yaml` 登记这个地址，配上 `token_keys`；AgentDef 的 `loop` 写 `server` 与 `models`。评测：`yanshi eval -loop <名称>=http://127.0.0.1:8001`，此时 `--model-base-url` 用评测的端点 `http://127.0.0.1:18472/v1/loop`（`docs/design/m4-eval.md` §3）。

一致性：`make looptest-adapters`（需要 uv 与联网安装 `uv.lock` 锁定的依赖，不在 `make check` 中）。它对两个适配依次运行 `yanshi looptest -step-token`，假模型在 `127.0.0.1:18471`。改动 `loop.proto`、回答校验或 looptest 的场景之后运行。

## 适配必须做到的

- **无状态。** 每一步从请求的 `messages` 重建框架的输入，不在进程里保存跨步的东西；不按 `(run_id, attempt, turn)` 去重或缓存回答，同一步可能以相同或递增的 attempt 再来，两次可以回答不同（ADR-0062 第 2、3 条）。
- **调用交给平台。** 模型要调用工具时结束这一步，把调用放进回答；结果在下一步的 `messages` 中以平台改写过的调用 ID 带回，按 ID 配对即可。被拒、失败的结果也是一条 tool 消息。最后一条不一定是调用结果：插话排在结果之后。
- **模型与凭证来自请求。** 模型用请求 `models` 的第一个（评测换模型时它会变），API key 用请求的 `step_token`，base URL 指向内核的模型端点（ADR-0063）。客户端不重试：端点已经重试过，配额错误（429 `quota_exceeded`）在这一步内重试也没用。
- **系统指令照转。** 平台的指令只在请求的 `system` 里，框架不能把它丢掉或换掉。
- **失败返回非 200。** 模型或框架失败时返回 502，平台按模型错误处理。日志只记 ID，不记内容。

## 状态

两个适配都不用 `state`：对话每步都由平台给出。需要停在图中途的 LangGraph 图（例如工具存根里 `interrupt()`）把 checkpoint 去掉消息后放进 `Reply.state`、下一步从 `Step.state` 恢复，`thread_id` 只用 `run_id`；消息留在 checkpoint 里会让状态随轮数增长，超过 64 KiB 的上限（ADR-0062 的原型结论）。

## 版本

`uv.lock` 锁定依赖（2026-11-25：Python 3.13、langgraph 1.2.24、langchain-openai 1.8.5、pydantic-ai-slim 2.74.0）。升级用 `uv lock --upgrade`，之后跑 `make looptest-adapters`。
