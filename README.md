# yanshi

承载多条 C 端业务线的分布式 Agent 运行时基础设施。见 [项目章程](docs/charter.md) 与 [术语表](CONTEXT.md)。

## 快速开始

```sh
make build
./bin/yanshi serve                       # 内存存储 + 4 个 Worker，监听 127.0.0.1:8080
make deps-up                             # 用 docker 启动 PostgreSQL
./bin/yanshi serve -storage postgres -blob s3   # 持久化（PostgreSQL + S3 兼容对象存储）；多个进程可共享水平扩展
./bin/yanshi chat -agent echo            # 离线：复述输入
ARK_API_KEY=... ./bin/yanshi serve       # 启用火山方舟，然后 chat -agent assistant
# 火山方舟 coding plan：另设 ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/coding/v3
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
