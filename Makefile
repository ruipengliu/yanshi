GOBIN := $(shell go env GOPATH)/bin
# 与 docker-compose.yml 一致的开发数据库。
export YANSHI_TEST_PG ?= postgres://yanshi:yanshi@127.0.0.1:54329/yanshi?sslmode=disable
# 沙箱测试使用本机 Docker；对象存储测试使用 docker compose 中的 S3。
export YANSHI_TEST_DOCKER ?= 1
export YANSHI_TEST_S3 ?= 127.0.0.1:58333
# TypeScript SDK 与真实网关的互通测试（internal/e2e TestWebSDK）；需要 Node.js 与 pnpm。
export YANSHI_TEST_NODE ?= 1

.PHONY: check gen lint check-deps test sim sim-quick verify looptest-adapters eval eval-kernel eval-xinli eval-loop build build-yanshi build-xinli tools deps-up deps-down sandbox-image web web-deps mobile

check: lint test

tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.50.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
	# 编辑 .go 文件后由 .claude/settings.json 的 hook 整理 import（与 go.mod 中的 x/tools 同版本）
	go install golang.org/x/tools/cmd/goimports@v0.50.0

# 开发依赖（PostgreSQL、S3 兼容对象存储；沙箱测试所用的基础镜像）
deps-up:
	docker compose up -d --wait
	docker image inspect python:3.12-slim >/dev/null 2>&1 || docker pull -q python:3.12-slim

deps-down:
	docker compose down

# Go 与 TypeScript（sdk/web）的消息类型；TypeScript 的生成插件随 sdk/web 的开发依赖安装。
gen: web-deps
	PATH=$(GOBIN):$$PATH buf generate

lint: web-deps check-deps
	test -z "$$(gofmt -l internal cmd sdk scripts persona apps)" || (gofmt -l internal cmd sdk scripts persona apps; exit 1)
	go vet ./...
	cd persona && go vet ./...
	cd apps/xinli && go vet ./...
	go run ./scripts/docrefs
	PATH=$(GOBIN):$$PATH buf lint
	cd sdk/web && pnpm lint

# ADR-0043：独立模块守住 internal 边界，传递依赖检查同时阻止内核反向依赖业务层。
# 先检查 go list 的退出码，避免包加载失败被管道吞掉。
check-deps:
	@for dir in persona apps/xinli; do \
		deps=$$(go -C "$$dir" list -deps ./...) || exit 1; \
		printf '%s\n' "$$deps" | awk '/^yanshi\/internal\// { print "依赖方向违规: " $$0; bad = 1 } END { exit bad }' || exit 1; \
	done
	@deps=$$(go list -deps ./...) || exit 1; \
		printf '%s\n' "$$deps" | awk '/^(persona|xinli)(\/|$$)/ { print "依赖方向违规: " $$0; bad = 1 } END { exit bad }'

# internal/sim 的全部种子不在竞争检测下跑：World 由测试确定性地逐步推进，其中的并发部分（如 Worker 操作期间的监视
# goroutine）由所在包的单元测试与 e2e 在 -race 下覆盖；竞争检测在这里却让它慢十几倍（种子并行后仍需约 2.5 分钟，
# 不带 -race 约 40 秒）。竞争检测在 sim 上真正能查的是并行的 World 之间是否共享了可变状态：确定性测试同时运行
# 几个 World，留在 -race 下检查这一点（同时也查到这几个 World 内部的监视 goroutine）。
test: deps-up web day-bin
	cd persona && go test -race ./...
	cd apps/xinli && go test -race ./...
	go test -race $$(go list ./... | grep -v '^yanshi/internal/sim$$')
	# 缩短版一天与双分片断连恢复一起守住跨进程装配；双分片用例记录实际耗时，完整版在 make day。
	YANSHI_DAY_BIN=$(CURDIR)/bin/xinli-day go test -race ./internal/day -run '^TestDay(Short|Shards)$$' -count=1
	go test ./internal/sim
	go test -race ./internal/sim -run '^Test(Simulation|ExternalLoop)IsDeterministic$$'

web-deps:
	cd sdk/web && pnpm install --frozen-lockfile --silent

# TypeScript SDK：类型检查、单元测试，并构建 internal/e2e 互通测试运行的脚本；网页通话页所用的打包产物须与源码一致。
web: web-deps
	cd sdk/web && pnpm test && pnpm bundle:check

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

# 更多种子的确定性模拟测试，含 PostgreSQL 单库与双库差分测试。
# 2026-11-22 在 Mac mini（12 核）上实测 623 秒（此前 1092 秒）：PG 差分主要在等数据库往返，与吃 CPU 的内存模拟
# 在顶层并行、重叠运行；PG 种子至多 16 个、双库至多 16 对同时运行（开发库 max_connections 300）；GOGC=400 让内存
# 模拟的 CPU 时间少约三分之一，峰值内存约 1.5 GB。关键路径仍是双库差分（每对 10–80 秒）。
SIM_SEEDS ?= 2000
SIM_PGSEEDS ?= 100
SIM_SHARDSEEDS ?= 100
sim: deps-up
	GOGC=400 go test ./internal/sim -sim.seeds=$(SIM_SEEDS) -sim.pgseeds=$(SIM_PGSEEDS) -sim.shardseeds=$(SIM_SHARDSEEDS) -timeout 30m

# 中途验收：改动执行语义的过程中快速回归，种子数为 make sim 的五分之一（实测约 2.5 分钟）。罕见路径在这个规模下只命中
# 个位数次，合入前仍须跑完整的 make sim。
sim-quick:
	$(MAKE) sim SIM_SEEDS=400 SIM_PGSEEDS=20 SIM_SHARDSEEDS=20

# 形式化验证（docs/formal-verification）：lifecycle.Recheck 的 Quint 规约做类型检查、单元测试、随机模拟与 Apalache
# 有界验证，两个变异规约须被查出违例，再导出真实 Go 实现的删查交错轨迹在规约上回放；A5 迁槽即换代的规约检验
# ADR-0046 的步骤顺序（工具未实现，暂无轨迹对照，含分片合并、以同一编号重新加入、xinli 切单库、过期的单库进度与
# 非分片内核），八个违反顺序的变体、xinli 的 SeenRound 缺陷与单库按回答槽位补读的假想缺陷须被查出，xinli 的
# identity 核对须兜住其中三个，摘要 ABA 的变体须被查出；外部循环的规约检验 ADR-0062 一步的提交、重试与接管（未实现，
# 暂无轨迹对照），八个违反契约的变体须被查出。不在 check 中：
# 需要 Node.js、pnpm 与 JDK 17 或更新（Apalache，首次运行时 Quint 下载约 117 MB 到 ~/.quint），且只覆盖已建模的协议。
# 2026-11-23 在 Mac mini 上实测约 355 秒（前两部分各约 42 秒、315 秒）；外部循环约 45 秒（2026-11-24）。
verify:
	@java -version >/dev/null 2>&1 || { echo 'make verify 需要 JDK 17 或更新（Apalache）：java -version 失败'; exit 1; }
	# 构建脚本的放行以各目录的 pnpm-workspace.yaml 为准；环境变量会整体覆盖它，pnpm exec 运行前的依赖检查也读它，整段去掉。
	cd docs/formal-verification/poc1-lifecycle-recheck && env -u PNPM_CONFIG_ALLOW_BUILDS sh -c 'pnpm install --frozen-lockfile --silent && bash run.sh'
	cd docs/formal-verification/a5-slot-migration && env -u PNPM_CONFIG_ALLOW_BUILDS sh -c 'pnpm install --frozen-lockfile --silent && bash run.sh'
	cd docs/formal-verification/external-loop && env -u PNPM_CONFIG_ALLOW_BUILDS sh -c 'pnpm install --frozen-lockfile --silent && bash run.sh'

# 外部循环的参考适配（sdk/python，ADR-0062 第 8 条）跑一致性套件：需要 uv 与联网安装锁定的依赖，不在 check 中。
# 改动 loop.proto、回答校验或 looptest 的场景之后运行。
looptest-adapters:
	bash sdk/python/scripts/looptest-adapters.sh

# 评测（docs/design/m4-eval.md）：消耗真实 token，不在 check 中；需要 ARK_API_KEY（火山方舟）。
# 改动模型、提示词、AgentDef 或召回、压缩、Memory、MCP 中给模型的文本时必须运行。
# Memory 用例的召回依赖嵌入模型：默认用火山方舟的多模态嵌入，与基线一致（YANSHI_EMBEDDING_MODEL 可覆盖）。
EVAL_FLAGS ?= -sandbox docker
YANSHI_EMBEDDING_MODEL ?= ark/doubao-embedding-vision-251215
EVAL_KERNEL_CMD = ./bin/yanshi eval -suite evals -embedding-model $(YANSHI_EMBEDDING_MODEL) $(EVAL_FLAGS)
EVAL_XINLI_CMD = ./bin/yanshi eval -suite apps/xinli/evals -xinli-bin ./bin/xinli -embedding-model $(YANSHI_EMBEDDING_MODEL) $(EVAL_FLAGS)

eval: build
	@status=0; \
	$(EVAL_KERNEL_CMD) || status=$$?; \
	$(EVAL_XINLI_CMD) || status=$$?; \
	exit $$status

eval-kernel: build-yanshi
	$(EVAL_KERNEL_CMD)

eval-xinli: build
	$(EVAL_XINLI_CMD)

# 外部循环的冒烟评测（evals/loop，ADR-0063）：参考适配经内核模型端点调用真实模型，消耗真实 token。
# LOOP_FRAMEWORK 选 pydantic 或 langgraph；需要 uv。改动内核模型端点或外部循环的上下文组装后运行。
LOOP_FRAMEWORK ?= pydantic
eval-loop: build
	bash sdk/python/scripts/eval-loop.sh $(LOOP_FRAMEWORK) $(EVAL_LOOP_FLAGS)

build: build-xinli build-yanshi

build-xinli:
	cd apps/xinli && go build -o ../../bin/xinli ./cmd/xinli

build-yanshi:
	go build -o bin/yanshi ./cmd/yanshi

# 代码解释器沙箱镜像（serve -sandbox docker 的默认镜像）
sandbox-image:
	docker build -t yanshi-sandbox:dev deploy/sandbox

# Q122 / P4-B7：专用制品才包含父进程时钟与崩溃边界；不覆盖生产 bin/xinli。
# 默认 check 不构建或运行整日驱动，三个数据库与子进程的成本单独验收。
DAY_RUN ?= ^TestDay$$|^TestDayScheduleHTTP$$|^TestDayLongTransaction$$
.PHONY: day-bin day day-short day-ark

day-bin:
	cd apps/xinli && go build -tags daytest -o ../../bin/xinli-day ./cmd/xinli
	cd apps/xinli && go test -race -tags daytest ./cmd/xinli -run '^TestDay' -count=1

day: deps-up day-bin
	YANSHI_DAY_BIN=$(CURDIR)/bin/xinli-day go test -race ./internal/day -run '$(DAY_RUN)' -count=1 -timeout 20m -v

day-short: deps-up day-bin
	YANSHI_DAY_BIN=$(CURDIR)/bin/xinli-day go test -race ./internal/day -run '^TestDay(Short|Shards)$$' -count=1 -timeout 5m -v

# 与前次真实模型报告比较；首轮不冒充已有基线，报告只含状态、计数与差异。
day-ark: deps-up day-bin
	YANSHI_DAY_BIN=$(CURDIR)/bin/xinli-day YANSHI_DAY_ARK=1 go test ./internal/day -run '^TestDayArk$$' -count=1 -timeout 45m -v

# C6 只操作独立 yanshi-failover 项目；不能依赖 deps-up（dev 会将它指向共享项目）。
.PHONY: failover failover-down shard-capacity shard-move day-shards
failover: day-bin
	go build -o bin/yanshi ./cmd/yanshi
	scripts/failover.sh

failover-down:
	scripts/failover.sh down

# 整片搬迁演练（docs/design/deployment.md §10.5）：默认 1GB 与 10GB 两个数据量，YANSHI_MOVE_GB 可改。
shard-move: day-bin
	go build -o bin/yanshi ./cmd/yanshi
	scripts/failover.sh move

shard-capacity: day-bin
	scripts/shard-capacity.sh

# 现有双库专项包含真实断片、恢复、委托和删除；完整 TestDay 仍按单库游标验收。
day-shards: deps-up day-bin
	YANSHI_DAY_BIN=$(CURDIR)/bin/xinli-day go test -race ./internal/day -run '^TestDayShards$$' -count=1 -timeout 5m -v
