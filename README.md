# Extension-Host

GoDesk 的插件宿主。仓库提供公共 Go 模块、独立命令 `godesk-ext`、最小管理界面和示例插件。

当前完成实施 Spec 的**阶段 A（工单 01 · 独立运行闭环）**。同一份插件 ZIP 之后也用于 GoDesk 嵌入验收（工单 02）。共享 systemd、Mihomo 插件和完整开发命令分别属于工单 03–05，尚未实现。

- 共享契约：[GoDesk 插件首版契约](https://github.com/EziosWJ/GoDesk/blob/main/docs/extensions.md)
- 实施顺序：[Extension Host 实施 Spec](https://github.com/EziosWJ/GoDesk/blob/main/.scratch/extension-host/spec.md)
- 进程与仓库边界：[ADR 0009](https://github.com/EziosWJ/GoDesk/blob/main/docs/adr/0009-embed-extension-host-from-separate-module.md)

## 阶段 A 已实现

- 仅依赖本仓库，使用 `GOWORK=off CGO_ENABLED=0` 构建和运行。
- `godesk-ext` 默认监听 `127.0.0.1:5001`，提供插件管理页。
- 标准 ZIP 的 `pack`、`validate`、`run`；目录和 ZIP 都可运行。
- Web+Lua、Web-only、Lua-only 三类插件形态。
- 原生 HTTP 调用 Lua、原生 WebSocket 事件流、插件业务 SQLite。
- 安装、启用、启动、停止、禁用、卸载、重启恢复，以及保留/清除业务数据。
- 配置持久化；标记为 `secret` 的值不出现在管理响应和宿主日志中。
- 无效包、缺失入口、不兼容 Host API、未获授权的 Host 调用返回明确错误。
- `host.http`、`host.ws`、`host.services` 在阶段 A 始终返回明确的“未授权”错误；能力实现和授权配置随工单 03/04 交付。

## 构建

```sh
GOWORK=off CGO_ENABLED=0 go build -o bin/godesk-ext ./cmd/godesk-ext
```

或：

```sh
make build
```

## 快速开始

```sh
make examples
./bin/godesk-ext --data-dir ./godesk-ext-data
```

打开 <http://127.0.0.1:5001/>，安装 `dist/com.godesk.hello-0.1.0.zip`，然后点击“启用”。插件页面会通过原生 `fetch` 调用 Lua，通过 WebSocket 接收事件，并把访问次数写入自己的 SQLite。

也可以直接运行源码目录或 ZIP：

```sh
./bin/godesk-ext run ./examples/hello --data-dir ./godesk-ext-data
./bin/godesk-ext run ./dist/com.godesk.hello-0.1.0.zip --data-dir ./godesk-ext-data
```

`run` 会安装、启用并启动指定插件，然后提供管理页。若同一 ID 已安装，会先卸载再重新安装：业务数据默认保留，插件配置恢复为 Manifest 默认值。

## 命令

| 命令 | 行为 |
| --- | --- |
| `godesk-ext` / `godesk-ext serve` | 启动宿主和管理页。参数：`--listen`、`--data-dir`。 |
| `godesk-ext run <dir\|zip>` | 安装、启用并运行一个插件，然后启动宿主。 |
| `godesk-ext validate <dir\|zip>` | 校验 Manifest、入口、Host API 兼容性和包路径。 |
| `godesk-ext pack <dir> [-o file]` | 校验后打包。默认输出 `dist/<id>-<version>.zip`。 |

参数可以放在位置参数前后。默认数据目录是 `./godesk-ext-data`。

## 插件包

ZIP 根目录必须包含 `manifest.json`。入口路径相对于包根目录，且至少声明一个入口。

```text
manifest.json
web/index.html
web/assets/...
lua/init.lua
lua/proxies.lua
```

```json
{
  "schemaVersion": 1,
  "id": "com.godesk.mihomo",
  "name": "Mihomo",
  "version": "0.1.0",
  "hostApi": { "min": 1, "max": 1 },
  "entry": { "web": "web/index.html", "lua": "lua/init.lua" },
  "config": {
    "controller": { "type": "string", "required": true },
    "secret": { "type": "string", "secret": true }
  },
  "capabilities": ["http", "websocket", "storage", "services"]
}
```

| 字段 | 说明 |
| --- | --- |
| `schemaVersion` | Manifest 格式版本，当前为 `1`。 |
| `id` | 点分隔的小写稳定标识，同时用作目录名和 URL 路径。 |
| `version` | 语义化版本。 |
| `hostApi` | 兼容的 Host API 闭区间；当前宿主为 `1`。 |
| `entry.web` | 可选。构建好的 HTML 入口。 |
| `entry.lua` | 可选。Lua 启动脚本。 |
| `config` | 可选。配置字段类型为 `string`、`number`、`boolean`；`secret` 只用于字符串。 |
| `capabilities` | 可选。插件声明的能力需求；实际授权由部署配置决定。 |

包内路径必须留在包根目录内。ZIP 中的绝对路径、`..`、反斜杠、符号链接和重复路径都会被拒绝。当前限制为单文件 64 MiB、总计 256 MiB、最多 4096 个文件。

## Lua 运行时

每个运行实例拥有独立的 GopherLua `LState` 和一个专属 goroutine。所有 HTTP 处理、生命周期钩子和存储调用都串行执行。停止插件会取消当前 Lua 执行并释放订阅。

### `host.app`

```lua
host.app.route("GET", "/hello", function(request)
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode({ ok = true }),
  }
end)

host.app.on("start", function() print("started") end)
host.app.on("stop", function() print("stopping") end)
host.app.emit("visit", { count = 3 })
```

- `route(method, path, handler)`：路由相对于 `/extensions/{id}/api/`，按方法和精确路径匹配。重复路由、无效方法或路径会在加载时失败。
- `on(event, handler)`：只支持 `start` 和 `stop`。
- `emit(event, data)`：向当前插件的所有浏览器订阅发送 `{"event": ..., "data": ...}`。没有订阅者时丢弃。

请求表字段：

| 字段 | 内容 |
| --- | --- |
| `method` | 大写 HTTP 方法。 |
| `path` | API 前缀之后的路径，以 `/` 开头。 |
| `query` | 查询参数表，每个键取第一个值。 |
| `headers` | 请求头表，键为小写。 |
| `body` | 请求体字符串。 |
| `remoteAddr` | 客户端地址。 |

响应表字段：`status`（默认 200）、`headers`（字符串或字符串数组）、`body`（字符串）。

### `host.storage`

```lua
host.storage.set("visits", "3")
local visits = host.storage.get("visits")          -- string 或 nil
host.storage.delete("visits")

local rows = host.storage.query("SELECT key, value FROM kv ORDER BY key")
local affected = host.storage.exec("DELETE FROM kv WHERE key = ?", {"old"})
```

- `set` 接受字符串、数字和布尔值；`get` 返回字符串或 `nil`。
- `query` 返回行数组，每行是以列名为键的表。参数是数组。
- `exec` 返回受影响行数。
- 数据存放在插件自己的 SQLite 中，插件也可以直接建表。

### `host.config` 与 `host.json`

```lua
local controller = host.config.get("controller")
local all = host.config.all()

local payload = host.json.decode(request.body)
local text = host.json.encode(payload)
```

配置值来自管理页保存的配置与 Manifest 默认值。插件后端可以读取 `secret` 字段的原始值；管理 API 和宿主日志不会输出这些值。

### 其他字段与模块

| 名称 | 说明 |
| --- | --- |
| `host.dataDir` | 当前插件的数据目录，用于放置自有文件。 |
| `host.apiVersion` | 当前宿主 API 版本，值为 `1`。 |
| `host.http.request` | 阶段 A 返回 `host.http.request is not authorized`。 |
| `host.ws.subscribe` | 阶段 A 返回 `host.ws.subscribe is not authorized`。 |
| `host.services.list/get/restart` | 阶段 A 返回对应 `not authorized` 错误。 |

### 标准库和 `require`

保留基础函数、`string`、`table`、`math`、`coroutine`、`io`、`os` 的文件与时间操作。`os.exit`、`os.setenv`、`os.execute`、`io.popen`、`package.loadlib` 已移除。`print` 写入插件日志。

`require("name")` 先查找入口脚本所在目录，再查找包根目录：

```text
<dir>/name.lua
<dir>/name/init.lua
```

模块名形如 `a.b` 时解析为 `a/b.lua` 或 `a/b/init.lua`。加载结果缓存，循环依赖会报错。

## HTTP 路由

| 路径 | 用途 |
| --- | --- |
| `GET /api/v1/extensions` | 插件列表。 |
| `POST /api/v1/extensions` | 安装 ZIP。支持 `multipart/form-data` 的 `package`/`file` 字段，或原始 ZIP 请求体。 |
| `GET /api/v1/extensions/{id}` | 插件详情。 |
| `POST /api/v1/extensions/{id}/enable` | 持久化启用意图并启动。 |
| `POST /api/v1/extensions/{id}/start` | 启动已启用插件。 |
| `POST /api/v1/extensions/{id}/stop` | 停止实例，保留启用意图。 |
| `POST /api/v1/extensions/{id}/disable` | 清除启用意图并停止。 |
| `PUT /api/v1/extensions/{id}/config` | 合并并校验配置。 |
| `DELETE /api/v1/extensions/{id}[?purge=true]` | 卸载；`purge=true` 同时删除业务数据。 |
| `GET /extensions/{id}/web/` | 插件 Web 入口和资源。 |
| `/extensions/{id}/api/{path}` | Lua 业务路由。 |
| `GET /extensions/{id}/events` | 浏览器事件 WebSocket。 |

插件启用并启动后，Web、API 和事件路由才可用；停止、禁用或启动失败时返回 `409`。管理 API 始终可用。

## 生命周期与存储

| 操作 | 行为 |
| --- | --- |
| 安装 | 校验并发布包，记录安装信息；新插件未启用。同 ID 冲突。 |
| 启用 | 写入启用意图并启动。启动失败保留意图，状态显示失败原因。 |
| 启动 | 为已启用插件创建当前进程实例；重复启动收敛为同一实例。 |
| 停止 | 关闭订阅、Lua 和业务库连接，保留启用意图。 |
| 禁用 | 清除启用意图并停止，宿主重启后不会自动恢复。 |
| 卸载 | 先停止并释放资源，再删除包、配置和授权；默认保留业务数据。 |
| 关闭页面 | 只释放该 WebSocket 订阅，插件实例继续运行。 |
| 关闭宿主 | 停止全部实例，释放 Lua、数据库、订阅和 HTTP 连接。 |

数据目录：

```text
<data-dir>/
  host.db                    安装记录、启用意图、配置
  packages/<id>/             已发布插件包
  plugins/<id>/business.db   插件业务 SQLite
  plugins/<id>/data/         host.dataDir
  logs/<id>-<version>.log    插件日志
```

卸载默认只删除 `packages/<id>`、安装记录、配置和授权，保留 `plugins/<id>`。`purge=true` 会同时删除该目录。重新安装后继续使用原有业务库。

## 嵌入 GoDesk

公共 API 位于 `host` 包：

```go
extensionHost, err := host.Open(ctx, host.Options{DataDir: dataDir, Logger: logger})
if err != nil {
    return err
}
if err := extensionHost.Restore(ctx); err != nil {
    logger.Error("restore plugins", "error", err)
}
handler := extensionHost.Handler(host.HTTPOptions{RequireSession: requireSession})
```

`Handler` 同时提供 `/api/v1/extensions` 和 `/extensions/` 两组路由，需挂载在 GoDesk 现有 HTTP mux 的根或对应前缀上。`RequireSession` 会包裹全部扩展路由，用于嵌入模式的现有操作者会话；`AllowedOrigins` 用于开发代理下的 WebSocket 来源。

Embedding 和独立模式使用相同的 Manifest、生命周期、数据格式和 HTTP/WS 契约。GoDesk 继续使用自己的 `godesk.db`，Extension Host 在自己的数据目录维护 `host.db` 和插件业务库。

## 测试

```sh
GOWORK=off CGO_ENABLED=0 go test ./...
make check
```

集成测试覆盖安装、启用、HTTP、WebSocket、SQLite 持久化、宿主重启恢复、停止/禁用意图、启动失败重试、配置脱敏、Web-only/Lua-only 和非法包错误。

## 安全模型

首版面向部署者主动信任并安装的插件。插件 Web 页面与宿主同源；Lua 与宿主同进程，标准文件 I/O 使用宿主运行账号权限。能力授权约束通过 Host API 发起的操作，不限制插件直接使用的文件权限。

`godesk-ext` 默认只监听回环地址且没有登录。不要在没有额外访问控制的情况下把它绑定到公网接口。

## 后续阶段

| 工单 | 内容 |
| --- | --- |
| 01 | 独立运行示例插件（当前） |
| 02 | GoDesk 嵌入并通过拆分验收 |
| 03 | 抽取共享 systemd 能力 |
| 04 | 交付双模式 Mihomo 插件 |
| 05 | 交付独立开发与测试流程（`dev`、`logs`、`test` 等） |
