GOBIN := $(shell go env GOPATH)/bin
# 与 docker-compose.yml 一致的开发数据库。
export YANSHI_TEST_PG ?= postgres://yanshi:yanshi@127.0.0.1:54329/yanshi?sslmode=disable
# 沙箱测试使用本机 Docker。
export YANSHI_TEST_DOCKER ?= 1

.PHONY: check gen lint test sim build tools deps-up deps-down sandbox-image

check: lint test

tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.50.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6

# 开发依赖（PostgreSQL；沙箱测试所用的基础镜像）
deps-up:
	docker compose up -d --wait
	docker image inspect python:3.12-slim >/dev/null 2>&1 || docker pull -q python:3.12-slim

deps-down:
	docker compose down

gen:
	PATH=$(GOBIN):$$PATH buf generate

lint:
	test -z "$$(gofmt -l internal cmd sdk)" || (gofmt -l internal cmd sdk; exit 1)
	go vet ./...
	PATH=$(GOBIN):$$PATH buf lint

test: deps-up
	go test -race ./...

# 更多种子的确定性模拟测试，含 PostgreSQL 差分测试
sim: deps-up
	go test ./internal/sim -sim.seeds=2000 -sim.pgseeds=100

build:
	go build -o bin/yanshi ./cmd/yanshi

# 代码解释器沙箱镜像（serve -sandbox docker 的默认镜像）
sandbox-image:
	docker build -t yanshi-sandbox:dev deploy/sandbox
