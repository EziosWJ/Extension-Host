BIN := bin/ext
DIST := dist
DEV_LISTEN ?= 127.0.0.1:5001
DEV_DATA_DIR ?= ./ext-data

.PHONY: build test vet check examples run dev clean

build: ## 构建 ext
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/ext

test: ## 运行全部测试
	GOWORK=off CGO_ENABLED=0 go test ./...

vet: ## 运行 go vet
	GOWORK=off go vet ./...

check: vet test build ## 格式检查、测试与构建
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

examples: build ## 打包示例插件
	mkdir -p $(DIST)
	$(BIN) pack ./examples/hello -o $(DIST)/com.godesk.hello-0.1.0.zip
	$(BIN) pack ./examples/web-only -o $(DIST)/com.godesk.webonly-0.1.0.zip
	$(BIN) pack ./examples/lua-only -o $(DIST)/com.godesk.luaonly-0.1.0.zip
	$(BIN) pack ./examples/services -o $(DIST)/com.godesk.services-0.1.0.zip
	cd ./examples/postgres-backup/plugin && "$(abspath $(BIN))" pkg
	mv ./examples/postgres-backup/plugin/dist/com.godesk.postgres-backup-0.1.0.zip $(DIST)/

run: build ## 运行 Hello 示例
	$(BIN) run ./examples/hello --data-dir ./ext-data

dev: ## 启动宿主后端和管理前端
	GOWORK=off CGO_ENABLED=0 go run ./cmd/ext serve --listen "$(DEV_LISTEN)" --data-dir "$(DEV_DATA_DIR)"

clean: ## 清理构建产物
	rm -rf bin $(DIST)
