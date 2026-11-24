BIN := bin/godesk-ext
DIST := dist

.PHONY: build test vet check examples run clean

build: ## 构建 godesk-ext
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/godesk-ext

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
	$(BIN) pack ./examples/postgres-backup/plugin -o $(DIST)/com.godesk.postgres-backup-0.4.0.zip

run: build ## 运行 Hello 示例
	$(BIN) run ./examples/hello --data-dir ./godesk-ext-data

clean: ## 清理构建产物
	rm -rf bin $(DIST)
