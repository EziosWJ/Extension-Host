BIN := bin/ext
DIST := dist
DEV_LISTEN ?= 127.0.0.1:5001
DEV_DATA_DIR ?= ./ext-data

VERSION ?= v0.1.0
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || true)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GO_LDFLAGS := -s -w -X github.com/EziosWJ/Extension-Host/internal/buildinfo.Version=$(VERSION) -X github.com/EziosWJ/Extension-Host/internal/buildinfo.Commit=$(COMMIT) -X github.com/EziosWJ/Extension-Host/internal/buildinfo.Date=$(BUILD_DATE)

.PHONY: build test vet check examples run dev clean

build: ## 构建 ext
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="$(GO_LDFLAGS)" -o $(BIN) ./cmd/ext

test: ## 运行全部测试
	GOWORK=off CGO_ENABLED=0 go test ./...

vet: ## 运行 go vet
	GOWORK=off go vet ./...

check: vet test build ## 格式检查、测试与构建
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

examples: build ## 打包示例插件
	mkdir -p $(DIST)
	$(BIN) pack ./examples/hello -o $(DIST)/com.example.hello-0.1.0.zip
	$(BIN) pack ./examples/web-only -o $(DIST)/com.example.web-only-0.1.0.zip
	$(BIN) pack ./examples/lua-only -o $(DIST)/com.example.lua-only-0.1.0.zip
	$(BIN) pack ./examples/services -o $(DIST)/com.example.services-0.1.0.zip

run: build ## 运行 Hello 示例
	$(BIN) run ./examples/hello --data-dir ./ext-data

dev: ## 启动宿主后端和管理前端
	GOWORK=off CGO_ENABLED=0 go run -ldflags="$(GO_LDFLAGS)" ./cmd/ext serve --listen "$(DEV_LISTEN)" --data-dir "$(DEV_DATA_DIR)"

clean: ## 清理构建产物
	rm -rf bin $(DIST)
