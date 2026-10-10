# yanshi

承载多条 C 端业务线的分布式 Agent 运行时基础设施。见 [项目章程](docs/charter.md) 与 [术语表](CONTEXT.md)。

## 快速开始

```sh
make build
./bin/yanshi serve                       # 内存存储 + 4 个 Worker，监听 127.0.0.1:8080
make deps-up                             # 用 docker 启动 PostgreSQL
./bin/yanshi serve -storage postgres -blob s3   # 持久化（PostgreSQL + S3 兼容对象存储）；多个进程可共享水平扩展
./bin/yanshi chat -agent echo            # 离线：复述输入
TOKENHUB_API_KEY=... ./bin/yanshi serve  # 启用腾讯云 TokenHub（assistant 默认使用 tokenhub/deepseek-v4.1-flash），然后 chat
# Memory 的向量检索：另设 YANSHI_EMBEDDING_MODEL=tokenhub/kinfra-text-embedding-0.6b（不设时按文本相似度检索）
# 也支持火山方舟（ARK_API_KEY）与私有化 OpenAI 兼容服务（YANSHI_LOCAL_BASE_URL）
```

接入一台"电脑"（把某个目录作为 Node 暴露，`write_file` 需审批）：

```sh
./bin/yanshi node -root ~/Documents/share -label macbook
./bin/yanshi chat                         # 例如：读一下电脑上 notes.txt 并总结；/upload <文件> 上传给助手
```

启用云端代码沙箱（代码解释器，默认无网络）：

```sh
make sandbox-image
./bin/yanshi serve -sandbox docker       # 可加 -sandbox-runtime runsc 使用 gVisor
```

鉴权（[设计](docs/design/auth.md)）：默认 `-auth none` 信任客户端自报的身份，只允许监听回环地址。开启业务线签发的令牌：

```sh
./bin/yanshi keygen -business-line demo  # 生成 demo.key（私钥，扮演业务线服务端）与 businesslines/demo.yaml（公钥）
./bin/yanshi serve -auth jwt             # 只加载 businesslines/ 中的公钥
./bin/yanshi chat -key demo.key -user u1 # 用私钥自行签发用户令牌；node 命令同样支持 -key
./bin/yanshi token -key demo.key -user u1   # 打印令牌，供 curl 使用：-H "Authorization: Bearer $TOKEN"
```

MCP（[设计](docs/design/m5-mcp.md)）：远程 MCP Server 登记在 `mcp/*.yaml`（`name`、`business_line`、`url`、`headers` 中的 `${VAR}` 从环境变量注入），AgentDef 用 `mcp:<server>/<glob>` 开放；电脑上的本机 MCP Server 由设备 SDK 桥接，例如：

```sh
./bin/yanshi node -label laptop -mcp "fs=npx -y @modelcontextprotocol/server-filesystem ~/Documents"
```

未声明只读的 MCP 工具默认需要审批。

模型通过 AgentDef（`agents/*.yaml`）的 `model: provider/model` 选择：`ark/…`（火山方舟）、`local/…`（私有化 OpenAI 兼容服务，`YANSHI_LOCAL_BASE_URL`）、`echo/any`。

## HTTP API

见 `internal/httpapi/httpapi.go` 包注释。最小示例：

```sh
curl -XPOST localhost:8080/v1/sessions -d '{"business_line":"demo","end_user":"u1","agent":"echo"}'
curl -XPOST localhost:8080/v1/sessions/$SID/inputs -d '{"text":"你好"}'
curl -N localhost:8080/v1/sessions/$SID/stream          # SSE，支持 Last-Event-ID 续传
```

## 配额与用量

业务线配置（`businesslines/*.yaml`）中的 `quota` 声明业务线每月、EndUser 每日的金额上限，价格表在 `pricing.yaml`（[设计](docs/design/m4-quota-usage.md)）。超额时新 Run 返回 429，进行中的 Run 挂起到额度恢复；`GET /v1/quota`、`GET /v1/usage` 查询配额与用量。

## 内容安全

用户输入与模型输出在写日志之前检查（[设计](docs/design/m4-moderation.md)）。当前提供商是模拟实现（`serve -moderation mock`，关键词列表），**上线前须接入真实提供商**，见[上线检查清单](docs/launch-checklist.md)。

## AgentDef 灰度

`agents/releases.yaml` 声明每个 Agent 的稳定版本、灰度版本与比例、撤回的版本，`serve` 每 10 秒重新加载（[设计](docs/design/m4-agent-rollout.md)）。新版本先评测：`make eval EVAL_FLAGS="-sandbox docker -agent-version 2"`；按版本比较：指标的 `agent` 标签与 `GET /v1/usage?group_by=agent`。

## 压测与指标

`scripts/bench.sh` 在全新数据库上启动多个进程并压测（见[设计与结果](docs/design/m2-scale-test.md)），例如 `POOL=20 P=4 W=32 S=256 scripts/bench.sh`。Prometheus 指标在内部监听地址（`-peer-addr`）的 `/metrics` 上。

## 评测

`make eval` 用真实模型运行 `evals/` 中的用例并与基线比较，出现回归时失败（需要 `TOKENHUB_API_KEY`，见[设计](docs/design/m4-eval.md)）。比较其他模型：`make eval EVAL_FLAGS="-sandbox docker -model tokenhub/<模型ID>"`。

## 开发

`make check`（格式、vet、buf lint、全部测试含确定性模拟）。面向 Agent 的开发约定见 [AGENTS.md](AGENTS.md)。
