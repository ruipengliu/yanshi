#!/usr/bin/env bash
# 在 iOS 模拟器中运行移动端绑定层的冒烟程序（make mobile 的最后一步）。
#
# 前提：make mobile 已生成 build/mobile/Yanshi.xcframework、下载 build/swift-protobuf，并生成 Swift 消息类型。
# 步骤：为模拟器编译 SwiftProtobuf 静态库与冒烟程序 → 找到或创建模拟器设备 yanshi-smoke（最新的 iOS 运行时）
#       并启动 → 由 internal/e2e 的 TestMobileSwift 启动网关，以 simctl spawn 在模拟器中运行冒烟程序。
set -euo pipefail
cd "$(dirname "$0")/.."

DEVICE=${DEVICE:-yanshi-smoke}
SDK=$(xcrun --sdk iphonesimulator --show-sdk-path)
TARGET=$(uname -m)-apple-ios17.0-simulator
OUT=build/ios
mkdir -p "$OUT"

swiftc -target "$TARGET" -sdk "$SDK" -O -suppress-warnings -parse-as-library \
  -module-name SwiftProtobuf -package-name swift_protobuf \
  -emit-library -static -o "$OUT/libSwiftProtobuf.a" \
  -emit-module -emit-module-path "$OUT/SwiftProtobuf.swiftmodule" \
  build/swift-protobuf/Sources/SwiftProtobuf/*.swift

swiftc -target "$TARGET" -sdk "$SDK" -O \
  -I "$OUT" -L "$OUT" -lSwiftProtobuf \
  -F build/mobile/Yanshi.xcframework/ios-arm64_x86_64-simulator -framework Yanshi \
  -o "$OUT/Smoke" \
  sdk/mobile/smoke/Sources/Smoke/main.swift $(find sdk/mobile/smoke/Sources/Smoke/gen -name '*.swift')

if ! xcrun simctl list devices | grep -q " $DEVICE ("; then
  # 最新的 iOS 运行时，及它支持的最后一种 iPhone 机型。
  read -r RUNTIME TYPE < <(xcrun simctl list runtimes -j | python3 -c '
import json, sys
r = [r for r in json.load(sys.stdin)["runtimes"] if r["platform"] == "iOS" and r["isAvailable"]][-1]
t = [t for t in r["supportedDeviceTypes"] if t["name"].startswith("iPhone")][-1]
print(r["identifier"], t["identifier"])')
  xcrun simctl create "$DEVICE" "$TYPE" "$RUNTIME" >/dev/null
fi
xcrun simctl bootstatus "$DEVICE" -b >/dev/null

YANSHI_TEST_MOBILE_SMOKE="xcrun simctl spawn $DEVICE $PWD/$OUT/Smoke" \
  go test ./internal/e2e -run TestMobileSwift -count=1 -v
