# 原生可执行插件首版契约

原生插件是由 Extension Host 启停的 Linux 后台进程。C++、Go、Rust 等语言共用 ZIP 格式和 HTTP/JSON 协议。同平台的 ZIP 可安装到独立 `ext` 和嵌入相同 Extension Host 版本的 GoDesk。

## 插件包

插件作者负责构建目标平台的可执行 ELF。每个 ZIP 声明一个平台，支持 `linux-amd64` 和 `linux-arm64`。可以在构建机交叉编译和打包。安装时宿主检查目标与实际平台是否一致。插件作者负责声明 ELF 所需的系统依赖，或将依赖随包分发并自行配置加载路径。

```json
{
  "schemaVersion": 1,
  "id": "com.example.worker",
  "name": "Worker",
  "version": "0.1.0",
  "minHostVersion": "0.3.0",
  "hostApi": {"min": 1, "max": 1},
  "entry": {"native": "native/worker", "web": "web/index.html"},
  "native": {
    "platform": "linux-arm64",
    "exports": ["v1.getStatus"],
    "webApi": true
  }
}
```

- `entry.native` 是 `native/` 下的相对路径。一个包选用一个后端入口，原生入口可以单独使用或搭配 `entry.web`。在原生包中，网页放在 `web/`。
- `native.platform` 必填。 `native.exports` 和 `native.webApi` 可省略，分别默认空列表和 `false`。方法名使用点分隔的字母数字和下划线，例如 `v1.getStatus`。
- `minHostVersion` 对原生包必填，按语义化版本比较，比较对象是 Extension Host 版本，可用 `ext version` 查看独立宿主版本。运行 GoDesk 时也使用实际嵌入的 Extension Host 版本。它与 `schemaVersion` 和 `hostApi` 分别描述发布版本、包结构与协议兼容性。
- `pkg` 收集 `manifest.json`、`native/` 和声明的网页运行文件。默认产物名是 `dist/<id>-<version>-<platform>.zip`。包内只有声明的二进制入口会被赋予执行位。常规 ZIP 路径、符号链接和大小限制同样适用。
- `ext init` 的「网页 + 原生后端」和「只有原生后端」模板提供 Go 起始代码：源码位于 `backend/`，`make build` 输出到 `native/plugin`。`backend/`、`go.mod` 和 `Makefile` 不进入 ZIP。

从包含上述 Manifest 的项目目录执行以下命令。此处以 Go 的 ARM64 构建为例，其他语言只要在同一路径产出目标 ELF 即可。

```sh
mkdir -p native
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o native/worker ./cmd/worker
ext pkg
ext validate dist/com.example.worker-0.1.0-linux-arm64.zip
```

开发机可以验证其他架构的包。运行和安装需要与目标平台匹配的宿主。显式修改 `native/` 中的编译产物后，`ext dev` 会按原有机制更新实例。

## 进程与本地连接

宿主直接执行包内入口，工作目录为解包后的根目录。以宿主账号运行。stdout 和 stderr 写入插件日志。进程应保持前台运行，收到 SIGTERM 后退出。宿主停止实例时等待短暂的退出窗口，然后结束剩余进程组。就绪超时或意外退出会记录失败，部署者可执行 Start 重试。恢复流程会重新启动仍标记为启用的插件。

| 环境变量 | 用途 |
| --- | --- |
| `GODESK_HOST_SOCKET` | 宿主共用的 Unix socket。所有 Host API 请求都走此地址。 |
| `GODESK_HOST_TOKEN` | 当前进程实例的令牌。请求使用 `Authorization: Bearer <token>`。重启换发，停止失效。 |
| `GODESK_PLUGIN_SOCKET` | 提供公开方法或网页 API 的插件才会收到。插件应在此地址启动 HTTP 服务。 |

实例令牌用于宿主识别调用实例。插件应由部署者信任，原生进程拥有宿主账号的操作系统权限。

初始化完成后调用 `POST /v1/ready`。提供端应先监听 `GODESK_PLUGIN_SOCKET`，再报告就绪。就绪返回 HTTP 204，可重复调用。进程应尽快完成本地初始化。宿主恢复已启用的插件时，依赖方可能先启动，可在报告就绪后等待其他插件上线。宿主默认在 10 秒内等待就绪。

## Host API

使用 HTTP/1.1 和 JSON。已定义操作的宿主错误以适当的 HTTP 状态码和 `{"code":"…","message":"…"}` 返回。超时后由调用方根据业务语义决定是否重试。

| 方法和路径 | 返回 |
| --- | --- |
| `POST /v1/ready` | 204 |
| `GET /v1/context` | JSON 对象包含 `id`、`dataDir`、`config`、`hostApiVersion` 和 `hostVersion`。配置包含插件自己的 secret 值。 |
| `GET /v1/services` | 宿主账号可查询的 systemd 服务数组。 |
| `GET /v1/services/{unit}` | 服务详情。 |
| `POST /v1/services/{unit}/start` | 204。启动单元。 |
| `POST /v1/services/{unit}/stop` | 204。停止单元。 |
| `POST /v1/services/{unit}/restart` | 204。重启单元。 |
| `GET /v1/services/{unit}/logs?lines=N` | 200，返回单元的最近 journal 日志行，格式为 JSON 字符串数组。`lines` 省略时为 200，必须是 1 到 2000 之间的整数，无效时返回 400。 |
| `POST /v1/plugins/{id}/methods/{method}` | 请求体和成功响应体各为一个 JSON 值。 |

启停和重启可通过查询参数 `?sudo=true` 请求非交互 sudo，例如 `POST /v1/services/caddy.service/restart?sudo=true`。默认以宿主账号执行，查询和日志始终以宿主账号读取。所有已启用的原生插件均可调用提供方公开的方法，提供方应按这个范围设计接口。

### 提供公开方法

插件在接收 socket 上实现 `POST /rpc/{method}`，读取一个 JSON 参数值，成功时返回 2xx 和 JSON。宿主填写 `X-Godesk-Caller` 为已鉴别的调用插件 ID。提供方可验证宿主附带的 Bearer 令牌。插件可以用 4xx 或 5xx 和 JSON 错误响应表示业务失败。

目标插件缺失、停止或尚未就绪时宿主返回 503。方法未声明时返回 404。提供端无法连接或返回无效响应时返回 502，调用超时返回 504。宿主对每次调用设置 5 秒超时，JSON 请求和响应上限均为 1 MiB。

### 提供网页 API

当同时声明 `entry.web` 和 `native.webApi: true` 时，浏览器可以经由现有 `/extensions/{id}/api/…` 访问该插件。宿主在既有 GoDesk 会话检查之后，将请求转发到同一接收 socket 的 `/api/…`，并附带提供方实例令牌。公开 RPC 使用独立的 `/rpc/` 路径。Lua 与原生互调、GoDesk 业务模块直接调用原生方法均在后续版本接入。

### 连接验证

插件进程可用以下命令验证宿主连接。提供业务接口的进程先启动自己的监听，再发送就绪通知。

```sh
curl --unix-socket "$GODESK_HOST_SOCKET" \
  -H "Authorization: Bearer $GODESK_HOST_TOKEN" \
  http://localhost/v1/context

curl --unix-socket "$GODESK_HOST_SOCKET" \
  -H "Authorization: Bearer $GODESK_HOST_TOKEN" \
  -X POST http://localhost/v1/ready

curl --unix-socket "$GODESK_HOST_SOCKET" \
  -H "Authorization: Bearer $GODESK_HOST_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{}' \
  http://localhost/v1/plugins/com.example.worker/methods/v1.getStatus
```

## 验收

1. 为目标平台构建两个原生插件，分别打包。A 声明 `v1.getStatus`，B 通过 Host API 调用它。
2. 在独立宿主安装两包，分别启用并检查就绪、日志和调用结果。停掉 A，确认 B 收到 503。重新启动 A，确认原调用地址恢复。
3. 用测试 systemd 单元验证列表、详情、启停、重启和日志。本地开发可注入测试后端验证 sudo 选项和错误响应。
4. 用同一 ZIP 在 GoDesk 中重复安装、启动和调用。带网页的插件验证其 API 仍受 GoDesk 会话保护。
5. 用较新 `minHostVersion` 或其他架构的包验证安装错误。检查宿主重启恢复启用插件，禁用和卸载会结束插件进程。
