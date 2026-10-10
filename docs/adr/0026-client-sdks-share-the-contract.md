---
status: accepted
---

# 端侧 SDK：移动端绑定 Go 核心，网页按协议实现，消息一律用契约生成的类型

章程中待决的"端侧 SDK 实现方式"（Go 共享核心 vs 协议先行、各平台原生实现）按端分别决定：

- **手机与电脑：共享 Go SDK。** `sdk/nodesdk` 已经实现了连接与重连、以 call_id 去重执行、账本落盘顺序、续订、输入去重重试。这些都是容易写错、出错后果严重的逻辑（重复执行有副作用的调用、输入生效两次）。所以移动端不重写，而是用 gomobile 绑定同一份实现（`sdk/mobile`），电脑端直接用 Go。
- **网页：TypeScript 按协议实现（`sdk/web`）。** Go 核心编译成 WebAssembly 后体积在 MB 级，网页上不划算。网页只作会话客户端，没有 Node 角色，所以不需要执行器与账本，要重写的只有连接、续订与重试。这部分语义与 Go SDK 一一对应，由互通测试对真实网关验证。
- **消息类型一律从 `proto/` 生成。** 网页用 protobuf-es 生成并提交的类型，线上用二进制帧。移动端绑定层跨语言只传 protobuf 字节，原生侧用 swift-protobuf / protobuf-javalite 从同一份 `proto/` 生成类型。我们没有为每种语言手写一套消息类型，所以契约变化不会在某个端静默漂移，`buf breaking` 对所有端同样有效。

## Consequences

- gomobile 只能跨语言传基本类型、字节、结构体指针与接口。所以移动端的 API 是"字节进、字节出"，再加原生侧实现的回调接口。各平台若要更顺手的类型化 API，在原生侧再包一层。
- 回调在 Go 的线程中调用，原生侧更新界面前要切回主线程。
- 改动 `sdk/nodesdk`、`sdk/mobile` 或 `node.proto` 时运行 `make mobile`（需要 Xcode），它会在 macOS 与 iOS 模拟器上实测绑定。Android 的 aar 需要 NDK，目前只编译 Java 绑定，真机构建列入上线检查清单。
- 网页 SDK 的类型检查、单元测试与互通测试在 `make check` 中，所以 `make check` 也需要 Node.js 与 pnpm。
