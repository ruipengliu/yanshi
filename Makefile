GOBIN := $(shell go env GOPATH)/bin

.PHONY: check gen lint test sim build tools

check: lint test

tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.50.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6

gen:
	PATH=$(GOBIN):$$PATH buf generate

lint:
	test -z "$$(gofmt -l internal cmd sdk)" || (gofmt -l internal cmd sdk; exit 1)
	go vet ./...
	PATH=$(GOBIN):$$PATH buf lint

test:
	go test -race ./...

# 更多种子的确定性模拟测试
sim:
	go test ./internal/sim -sim.seeds=2000

build:
	go build -o bin/yanshi ./cmd/yanshi
