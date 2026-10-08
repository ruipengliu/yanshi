# yanshi

承载多条 C 端业务线的分布式 Agent 运行时基础设施。见 [项目章程](docs/charter.md) 与 [术语表](CONTEXT.md)。

## 快速开始

```sh
make build
./bin/yanshi serve                       # 内存存储 + 4 个 Worker，监听 127.0.0.1:8080
make deps-up                             # 用 docker 启动 PostgreSQL
./bin/yanshi serve -storage postgres     # 持久化；多个进程可共享同一数据库水平扩展
./bin/yanshi chat -agent echo            # 离线：复述输入
ARK_API_KEY=... ./bin/yanshi serve       # 启用火山方舟，然后 chat -agent assistant
```

接入一台"电脑"（把某个目录作为 Node 暴露，`write_file` 需审批）：

```sh
./bin/yanshi node -root ~/Documents/share -label macbook
./bin/yanshi chat                         # 例如：读一下电脑上 notes.txt 并总结
```

启用云端代码沙箱（代码解释器，默认无网络）：

```sh
make sandbox-image
./bin/yanshi serve -sandbox docker       # 可加 -sandbox-runtime runsc 使用 gVisor
```

模型通过 AgentDef（`agents/*.yaml`）的 `model: provider/model` 选择：`ark/…`（火山方舟）、`local/…`（私有化 OpenAI 兼容服务，`YANSHI_LOCAL_BASE_URL`）、`echo/any`。

## HTTP API

见 `internal/httpapi/httpapi.go` 包注释。最小示例：

```sh
curl -XPOST localhost:8080/v1/sessions -d '{"business_line":"demo","end_user":"u1","agent":"echo"}'
curl -XPOST localhost:8080/v1/sessions/$SID/inputs -d '{"text":"你好"}'
curl -N localhost:8080/v1/sessions/$SID/stream          # SSE，支持 Last-Event-ID 续传
```

## 开发

`make check`（格式、vet、buf lint、全部测试含确定性模拟）。面向 Agent 的开发约定见 [AGENTS.md](AGENTS.md)。
