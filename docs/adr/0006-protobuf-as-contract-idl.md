---
status: accepted
---

# 以 Protobuf 作为跨端与存储契约

Event 及其多模态内容块用 Protobuf 定义（`proto/yanshi/v1`），同时作为日志存储格式和跨端传输格式（对外 HTTP 用 protojson）。选择它而非手写 JSON Schema，是因为端侧 SDK 将覆盖 iOS、Android、桌面多种语言，需要可生成代码、可机器检查向后兼容（`buf breaking`）的契约；这也让 Agent 开发者修改契约时有工具兜底。生成代码随仓库提交，构建不依赖 protoc。
