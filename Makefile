GOBIN := $(shell go env GOPATH)/bin
# 与 docker-compose.yml 一致的开发数据库。
export YANSHI_TEST_PG ?= postgres://yanshi:yanshi@127.0.0.1:54329/yanshi?sslmode=disable
# 沙箱测试使用本机 Docker；对象存储测试使用 docker compose 中的 S3。
export YANSHI_TEST_DOCKER ?= 1
export YANSHI_TEST_S3 ?= 127.0.0.1:58333
# TypeScript SDK 与真实网关的互通测试（internal/e2e TestWebSDK）；需要 Node.js 与 pnpm。
export YANSHI_TEST_NODE ?= 1

.PHONY: check gen lint test sim build tools deps-up deps-down sandbox-image web web-deps mobile

check: lint test

tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.50.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6

# 开发依赖（PostgreSQL、S3 兼容对象存储；沙箱测试所用的基础镜像）
deps-up:
	docker compose up -d --wait
	docker image inspect python:3.12-slim >/dev/null 2>&1 || docker pull -q python:3.12-slim

deps-down:
	docker compose down

# Go 与 TypeScript（sdk/web）的消息类型；TypeScript 的生成插件随 sdk/web 的开发依赖安装。
gen: web-deps
	PATH=$(GOBIN):$$PATH buf generate

lint: web-deps
	test -z "$$(gofmt -l internal cmd sdk)" || (gofmt -l internal cmd sdk; exit 1)
	go vet ./...
	PATH=$(GOBIN):$$PATH buf lint
	cd sdk/web && pnpm lint

test: deps-up web
	go test -race ./...

web-deps:
	cd sdk/web && pnpm install --frozen-lockfile --silent

# TypeScript SDK：类型检查、单元测试，并构建 internal/e2e 互通测试运行的脚本。
web: web-deps
	cd sdk/web && pnpm test

# 移动端绑定（sdk/mobile，需要 Xcode 与 JDK，不在 check 中）。改动 sdk/mobile、sdk/nodesdk 或 node.proto 后运行：
#   - gomobile 生成 iOS、模拟器与 macOS 的 Yanshi.xcframework；
#   - Swift 冒烟程序链接它，对 internal/e2e 启动的网关跑一遍（TestMobileSwift）：先在本机（macOS 切片），
#     再在 iOS 模拟器中（scripts/mobile-smoke-ios.sh）；
#   - Android 只编译 gobind 生成的 Java 绑定（本机没有 NDK，aar 的构建列入上线检查清单）。
# 冒烟程序所用的 swift-protobuf：取发布包而不是 git 克隆（完整历史在慢网络上拉不下来），按校验和固定。
SWIFT_PROTOBUF := 1.38.1
SWIFT_PROTOBUF_SHA256 := 7e35c119afe8f16fe4de45c2143b0f50a205db83738092336562d610469283ac

build/swift-protobuf/Package.swift:
	rm -rf build/swift-protobuf build/swift-protobuf.tar.gz && mkdir -p build
	curl -sSLf -o build/swift-protobuf.tar.gz https://codeload.github.com/apple/swift-protobuf/tar.gz/refs/tags/$(SWIFT_PROTOBUF)
	echo "$(SWIFT_PROTOBUF_SHA256)  build/swift-protobuf.tar.gz" | shasum -a 256 -c -
	tar xzf build/swift-protobuf.tar.gz -C build && mv build/swift-protobuf-$(SWIFT_PROTOBUF) build/swift-protobuf

mobile: build/swift-protobuf/Package.swift
	go build -o bin/ golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
	PATH=$(CURDIR)/bin:$$PATH gomobile bind -target=ios,iossimulator,macos -o build/mobile/Yanshi.xcframework ./sdk/mobile
	rm -rf build/mobile/java && PATH=$(CURDIR)/bin:$$PATH gobind -lang=java -outdir build/mobile/java ./sdk/mobile
	mkdir -p build/mobile/java/stub/android/content
	echo 'package android.content; public class Context {}' > build/mobile/java/stub/android/content/Context.java
	javac -d build/mobile/java/classes $$(find build/mobile/java -name '*.java')
	cd sdk/mobile/smoke && swift build -c release --product protoc-gen-swift
	PATH=$(GOBIN):$$PATH buf generate --template sdk/mobile/smoke/buf.gen.yaml
	cd sdk/mobile/smoke && swift build -c release --product Smoke
	YANSHI_TEST_MOBILE_SMOKE=$(CURDIR)/sdk/mobile/smoke/.build/release/Smoke go test ./internal/e2e -run TestMobileSwift -count=1 -v
	scripts/mobile-smoke-ios.sh

# 更多种子的确定性模拟测试，含 PostgreSQL 差分测试
sim: deps-up
	go test ./internal/sim -sim.seeds=2000 -sim.pgseeds=100

# 评测（docs/design/m4-eval.md）：消耗真实 token，不在 check 中；需要 TOKENHUB_API_KEY。
# 改动模型、提示词、AgentDef 或召回、压缩、Memory、MCP 中给模型的文本时必须运行。
EVAL_FLAGS ?= -sandbox docker
eval: build
	./bin/yanshi eval -suite evals $(EVAL_FLAGS)

build:
	go build -o bin/yanshi ./cmd/yanshi

# 代码解释器沙箱镜像（serve -sandbox docker 的默认镜像）
sandbox-image:
	docker build -t yanshi-sandbox:dev deploy/sandbox
