// swift-tools-version: 6.0
// 移动端绑定层的冒烟测试：链接 gomobile 生成的 Yanshi.xcframework（macOS 切片），用 swift-protobuf 编解码契约中的消息，
// 对 internal/e2e 启动的网关跑一遍（make mobile）。
import PackageDescription

let package = Package(
    name: "Smoke",
    platforms: [.macOS(.v13)],
    dependencies: [
        // make mobile 下载并校验发布包（见 Makefile 的 SWIFT_PROTOBUF）。
        .package(path: "../../../build/swift-protobuf")
    ],
    targets: [
        .binaryTarget(name: "Yanshi", path: "../../../build/mobile/Yanshi.xcframework"),
        .executableTarget(
            name: "Smoke",
            dependencies: ["Yanshi", .product(name: "SwiftProtobuf", package: "swift-protobuf")]
        ),
    ]
)
