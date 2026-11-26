# Extension-Host

GoDesk 的插件宿主。仓库提供公共 Go 模块、独立命令 `ext`、最小管理界面和示例插件。

当前完成实施 Spec 的工单 01（独立运行闭环）、工单 02（GoDesk 嵌入），以及工单 03 的宿主侧共享 systemd 能力与 `host.services` 接口。GoDesk 原生服务管理的接入随 v0.2.0 发布。`ext` 支持从模板创建、保存后自动更新和打包。Mihomo 插件与通用自动测试仍由工单 04–05 跟踪。另提供 [PostgreSQL 备份示例](examples/postgres-backup/README.md)，用于验收 Lua 命令执行和同一 ZIP 的双模式行为。

- 共享契约：[GoDesk 插件首版契约](https://github.com/EziosWJ/GoDesk/blob/main/docs/extensions.md)
- 原生插件：[打包、运行和调用契约](docs/native-plugins.md)
- 前端设计：[Extension Host 与插件前端设计规范](docs/frontend-design.md)
- 实施顺序：[Extension Host 实施 Spec](https://github.com/EziosWJ/GoDesk/blob/main/.scratch/extension-host/spec.md)
- 进程与仓库边界：[ADR 0009](https://github.com/EziosWJ/GoDesk/blob/main/docs/adr/0009-embed-extension-host-from-separate-module.md)

## 已实现

- 仅依赖本仓库，使用 `GOWORK=off CGO_ENABLED=0` 构建和运行。
- `ext` 默认监听 `127.0.0.1:5001`，提供插件管理页。
- 标准 ZIP 的 `pack`、`validate`、`run`；目录和 ZIP 都可运行。
- Web、Lua，以及支持 Linux amd64/arm64 的原生可执行插件。原生插件可经本地 Host API 调用其他原生插件和 systemd 服务接口。
- 原生 HTTP 调用 Lua、原生 WebSocket 事件流、插件业务 SQLite。
- 安装、启用、启动、停止、禁用、卸载、重启恢复，以及保留/清除业务数据。
- 配置持久化；标记为 `secret` 的值不出现在管理响应和宿主日志中。
- 无效包、缺失入口、不兼容 Host API 和失败的服务调用返回明确错误。
- `host.services` 通过共享的 `hostsys/services` 后端提供 `list`、`get`、`start`、`stop`、`restart`、`logs`。操作受宿主账号的系统权限约束。
- 插件可通过 `host.schedule` 按 cron 表达式或 `@every` 注册定时任务，回调在插件实例的 Lua goroutine 上串行执行。
- `host.http`、`host.ws` 尚未实现，调用会返回“尚未实现”错误。

## 构建

```sh
GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/ext ./cmd/ext
```

或：

```sh
make build
```

## 快速开始

```sh
make examples
./bin/ext --data-dir ./ext-data
```

打开 <http://127.0.0.1:5001/host/>，安装 `dist/com.godesk.hello-0.1.0.zip`，然后点击“启用”。插件页面会通过原生 `fetch` 调用 Lua，通过 WebSocket 接收事件，并把访问次数写入自己的 SQLite。

也可以直接运行源码目录或 ZIP：

```sh
./bin/ext run ./examples/hello --data-dir ./ext-data
./bin/ext run ./dist/com.godesk.hello-0.1.0.zip --data-dir ./ext-data
```

`run` 会安装、启用并启动指定插件，然后提供管理页。若同一 ID 已安装，会先卸载再重新安装：业务数据默认保留，插件配置恢复为 Manifest 默认值。

## 开发宿主

在仓库根目录运行 `make dev`，启动宿主后端及内嵌的管理前端，然后访问 <http://127.0.0.1:5001/host/>。按 Ctrl+C 停止。可通过 `make dev DEV_LISTEN=127.0.0.1:5002 DEV_DATA_DIR=./ext-data` 调整监听地址和数据目录。

管理前端由 Go 嵌入，没有独立的前端开发服务器；修改 Go 或 `web/` 文件后需要重启 `make dev`，再刷新浏览器。

## 开发一个插件

安装已发布版本时，将下面的 `<版本标签>` 换成包含这些命令的标签。开发当前源码时，可在本仓库运行 `GOWORK=off CGO_ENABLED=0 go install ./cmd/ext`。确保 Go 的二进制安装目录在 `PATH` 中。

```sh
CGO_ENABLED=0 go install github.com/EziosWJ/Extension-Host/cmd/ext@<版本标签>
```

```sh
ext init  # 插件名称填写 my-plugin
cd my-plugin
ext dev
```

`init` 提供「网页 + Lua」「只有网页」「只有 Lua」「网页 + 原生后端」「只有原生后端」五种模板。原生模板附带 Go 后端源码、`go.mod` 和 `Makefile`，先运行 `make build`（或 `make dev`）生成 `native/plugin`，再运行 `ext dev` 或 `ext pkg`。`backend/`、`go.mod` 和 `Makefile` 不会进入 ZIP。填写插件名称和标识后，工具会在当前目录下创建以插件名称命名的文件夹。生成的 `README.md` 说明各文件的用途和验证方法。

打开 `dev` 输出的地址，插件会自动安装并启用。访问 `/` 时，有网页就打开插件，否则打开 `/host/`。保存网页资源后浏览器自动刷新。Lua、`native/` 下的运行产物或 `manifest.json` 变动后，宿主检查文件并重启插件。更新失败会在终端显示原因，之前能运行的版本会继续提供服务或被重新启动。修改插件标识后重新运行 `dev`。

开发用的设置、业务数据和日志保存在项目的 `.ext/`。它们在重新运行 `dev` 后仍可使用，日志在 `.ext/logs/`。普通运行时，可在 `/host/` 的「宿主首页」中选择已启用的网页插件。宿主重启后会保留选择，插件暂时不可用时访问 `/` 会打开管理页。

在项目根目录另开终端打包：

```sh
ext pkg
```

ZIP 位于 `dist/<插件标识>-<版本>.zip`，内容来自项目根目录的 `manifest.json`、`web/` 和 `lua/`。原生插件收集 `native/`，ZIP 名附加目标平台，详见 [原生可执行插件首版契约](docs/native-plugins.md)。在 GoDesk 的「扩展」或独立宿主的 `/host/` 中安装同一个 ZIP，启用后验证插件。

## 命令

| 命令 | 行为 |
| --- | --- |
| `ext` / `ext serve` | 启动宿主和管理页。参数：`--listen`、`--data-dir`。 |
| `ext run <dir\|zip>` | 安装、启用并运行一个插件，然后启动宿主。参数同上。 |
| `ext version` | 查看 Extension Host 版本。 |
| `ext validate <dir\|zip>` | 校验 Manifest、入口、Host API 兼容性和包路径。 |
| `ext pack <dir> [-o file]` | 校验后打包。默认输出 `dist/<id>-<version>.zip`。 |
| `ext init` | 选择模板，并在当前目录下创建以插件名称命名的项目文件夹。 |
| `ext dev` | 自动安装并启用当前目录的插件，保存文件后更新。支持 `--listen`。 |
| `ext pkg` | 校验当前项目并把运行文件打包到 `dist/<id>-<version>.zip`。 |

参数可以放在位置参数前后。`serve` 和 `run` 默认使用 `./ext-data`，`dev` 使用项目的 `.ext/`。

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
  }
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
| `entry.native` | 可选。`native/` 下的 Linux ELF 可执行入口。 |
| `native` | 原生插件的目标平台、公开方法和网页 API 声明。 |
| `minHostVersion` | 最低 Extension Host 版本，原生插件必填。 |
| `config` | 可选。配置字段类型为 `string`、`number`、`boolean`；`secret` 只用于字符串。 |

数字配置可声明 `min`、`max` 和 `format: "integer"`。非密钥字符串可声明 `format: "writable-directory"`，填写时须为宿主机绝对路径。保存配置时，宿主会创建缺失的目录并创建、试写和移除临时文件。校验通过后才保存配置。

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

### `host.process`

```lua
local pid, err = host.process.start("pg_dump", {"--format=custom", "--file", host.dataDir .. "/backup.dump"}, function(result)
  print(result.exitCode, result.stderr)
end)
```

- `start(program, args, onExit[, env])` 按独立参数启动命令，继承宿主账号和环境，立即返回子进程 PID。可选的 `env` 表用字符串设置环境变量，用 `false` 移除继承的变量。修改仅对这个子进程生效。启动失败时返回 `nil, error`。
- 完成回调和 HTTP 处理器在同一个 Lua 实例中串行运行。结果包含 `pid`、`exitCode`、`stderr`，等待出错时还包含 `error`。标准输出默认丢弃，标准错误最多保留开头 64 KiB，截断时提供 `stderrTruncated`。
- 停止插件会终止其子进程。退出后由插件在下次启动时恢复自己的业务状态。

```lua
local result, err = host.process.exec("sort", {}, {
  stdin = "pear\napple\n",
  timeoutMs = 2000,
})
if result and result.exitCode == 0 then
  print(result.stdout)
end
```

- `exec(program, args[, options])` 以宿主账号执行命令，逐项传入字符串参数。它会等命令退出，占用当前插件的 Lua goroutine。其他插件和宿主可继续运行。
- `options.stdin` 为送入标准输入的字符串，省略时子进程读取到 EOF。`options.timeoutMs` 为正整数毫秒数，默认 30000。超时或停止插件会取消正在执行的命令。插件进入停止流程后，`exec` 返回启动失败，包括在停止钩子里发起的调用。
- 结果表包含 `exitCode`、`stdout`、`stderr`、`stdoutTruncated`、`stderrTruncated` 和 `timedOut`。标准输出和标准错误各保留开头 64 KiB。命令以非零状态退出时仍返回结果表，因信号终止时 `exitCode` 为 `-1`。
- 启动失败返回 `nil, error`，等待或读取输出失败时在结果表中提供 `error`。参数类型或取值无效时抛出 Lua 参数错误。

### `host.schedule`

```lua
local ok, err = host.schedule.register("nightly-backup", "0 3 * * *", function()
  host.process.start("pg_dump", {"--format=custom", "--file", host.dataDir .. "/backup.dump"}, function(result)
    print(result.exitCode, result.stderr)
  end)
end)

host.schedule.unregister("nightly-backup")  -- 已移除时为 true，否则为 false
```

- `register(name, expression, fn)` 注册或替换同名任务，成功返回 `true`；名称留空、参数类型错误或表达式无效时返回 `nil, error`，不会抛出错误。名称按插件实例隔离，同名注册会替换旧任务。
- `expression` 使用标准 5 段 cron（分 时 日 月 周）或 `@every <duration>`（例如 `@every 30m`），按宿主进程的本地时区解释。
- 回调在插件实例的串行 Lua goroutine 上执行，与 HTTP 处理器、生命周期钩子和进程回调不会并发。回调必须尽快返回；耗时工作应在 `host.process` 中启动。
- 同一任务上一次调用仍在排队或运行时，新的触发会被跳过并写入警告日志。
- 回调出错时写入错误日志，不重试。
- `unregister(name)` 移除任务，存在并移除时返回 `true`，否则返回 `false`。
- `validate(expression)` 检查表达式，返回 `true` 或 `nil, error`。
- `next(name)` 返回任务下次触发的 UTC RFC 3339 时间。没有下次触发时间时返回 `nil`。
- 注册只在插件实例运行期间有效。宿主重启后 Lua 状态会重建，插件需要在启动钩子中按自己持久化的设置重新注册；宿主停止期间错过的执行不会补跑。

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
| `host.http.request` | 尚未实现，调用返回 `host.http.request is not implemented`。 |
| `host.ws.subscribe` | 尚未实现，调用返回 `host.ws.subscribe is not implemented`。 |
| `host.services.list/get/start/stop/restart/logs` | 访问 systemd 服务。详见 [systemd 服务](#systemd-服务)。 |
| `host.schedule.register/unregister` | 注册和移除实例内的定时任务；详见上文 `host.schedule` 一节。 |

### 标准库和 `require`

两种宿主默认提供 GopherLua 的 `os` 和 `io`，文件访问和子进程使用宿主运行账号。`os.execute` 同步等待命令完成，插件在等待期间暂停处理其余 Lua 请求。`io.popen` 提供进程管道，`os.setenv` 修改宿主进程环境。`os.exit([code])` 停止当前插件实例，保留启用设置，并记录退出码。已注册的停止钩子随实例清理执行。`print` 写入插件日志。

`require("name")` 先查找入口脚本所在目录，再查找包根目录：

```text
<dir>/name.lua
<dir>/name/init.lua
```

模块名形如 `a.b` 时解析为 `a/b.lua` 或 `a/b/init.lua`。加载结果缓存，循环依赖会报错。

## systemd 服务

已启用的插件可以通过 `host.services` 管理 systemd 服务。查询与日志读取以宿主运行账号执行。启停和重启默认也使用该账号，传入 `{ sudo = true }` 时，非 root 宿主直接通过非交互 sudo 执行，由系统的 sudoers 授权决定能否执行。

```lua
local items = host.services.list()
local detail = host.services.get("caddy.service")
host.services.start("caddy.service", { sudo = true })
local logs = host.services.logs("caddy.service", 50)
```

- `list()` 返回服务数组，包含名称、说明、状态和可用动作。
- `get(unit)` 返回单元详情，包括进程 ID、内存和 CPU 用量、启动时间及 unit 文件路径。
- `start(unit[, options])`、`stop(unit[, options])` 和 `restart(unit[, options])` 成功时返回 `true`。
- `logs(unit[, lines])` 返回最近的 journal 日志行。默认 200 行，接受 1 到 2000 之间的整数。
- 单元名必须是合法的 `.service` 名称。调用错误保留机器可读错误码，例如 `invalid_unit`、`not_found`、`permission` 和 `logs_unavailable`。

试用服务示例时，运行 `./bin/ext run ./examples/services --data-dir ./ext-data`。

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
| `/extensions/{id}/api/{path}` | Lua 业务路由或已声明 `native.webApi` 的原生后端。 |
| `GET /extensions/{id}/events` | Lua 浏览器事件 WebSocket。 |

插件启用并启动后，Web、API 和事件路由才可用；停止、禁用或启动失败时返回 `409`。管理 API 始终可用。

## 生命周期与存储

| 操作 | 行为 |
| --- | --- |
| 安装 | 校验并发布包，记录安装信息；新插件未启用。同 ID 冲突。 |
| 启用 | 写入启用意图并启动。启动失败保留意图，状态显示失败原因。 |
| 启动 | 为已启用插件创建当前进程实例；重复启动收敛为同一实例。 |
| 停止 | 停止后端，释放订阅和数据库连接，保留启用意图。 |
| 禁用 | 清除启用意图并停止，宿主重启后不会自动恢复。 |
| 卸载 | 先停止并释放资源，再删除包和配置；默认保留业务数据。 |
| 关闭页面 | 只释放该 WebSocket 订阅，插件实例继续运行。 |
| 关闭宿主 | 停止全部实例，释放数据库、订阅和 HTTP 连接。 |

数据目录：

```text
<data-dir>/
  host.db                    安装记录、启用意图、配置
  packages/<id>/             已发布插件包
  plugins/<id>/business.db   插件业务 SQLite
  plugins/<id>/data/         host.dataDir
  logs/<id>-<version>.log    插件日志
```

卸载默认只删除 `packages/<id>`、安装记录及配置，保留 `plugins/<id>`。`purge=true` 会同时删除该目录。重新安装后继续使用原有业务库。

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

集成测试覆盖安装、启用、HTTP、WebSocket、SQLite 持久化、宿主重启恢复、停止/禁用意图、启动失败重试、配置脱敏、Web-only/Lua-only、非法包错误，服务接口的单元校验、sudo 选项和后端错误传播，以及原生进程的就绪、插件互调、超时、停机与恢复。

## 安全模型

插件 Web 页面与宿主同源；Lua 与宿主同进程，原生插件以同一宿主账号作为子进程运行。部署者启用自己信任的插件。宿主账号的系统权限决定插件能执行的操作。

`ext` 默认只监听回环地址且没有登录。不要在没有额外访问控制的情况下把它绑定到公网接口。

## 后续阶段

| 工单 | 内容 |
| --- | --- |
| 01 | 独立运行示例插件（已完成） |
| 02 | GoDesk 嵌入并通过拆分验收（已完成） |
| 03 | 抽取共享 systemd 能力（宿主侧已完成，GoDesk 接入随 v0.2.0） |
| 04 | 交付双模式 Mihomo 插件 |
| 05 | 独立开发与测试流程（已支持 `dev`，`logs` 与 `test` 待交付） |
