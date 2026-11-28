# Lua Host API 最小必要改造 Spec

本文档定义面向通用 CLI 类插件（含 `ext-container-manage`）的 Extension Host 最小必要改造方案。

## 设计原则

1. **通用化与轻量化**：不引入 Docker/Podman 专属接口，所有新增能力均为通用进程、事件与文件系统原语。
2. **向后兼容**：保持 `hostApi = 1` 与现有接口签名、默认行为 100% 兼容。
3. **安全与资源有界**：遵循 [ADR 0001](adr/0001-trusted-plugins-use-host-account-permissions.md)（受信任插件使用宿主账号系统权限）；所有流式缓冲、输出上限和事件队列必须有界，防止 OOM、子进程管道阻塞及孤儿进程泄漏。
4. **零新增第三方依赖**：仅使用 Go 标准库及仓库现有依赖。

---

## P0-1：增强 `host.process.exec`

### 签名

```lua
local result, err = host.process.exec(program, args[, options])
```

### 新增 `options` 字段（全部可选）

| 字段 | 类型 | 默认值 | 校验与行为 |
| --- | --- | --- | --- |
| `cwd` | `string` | 继承宿主工作目录 | 必须是非空字符串且不含 `\0`；无效抛出参数错误 (`arg #3`)。设置 `cmd.Dir`。 |
| `env` | `table` | 继承宿主环境 | 与 `host.process.start` 规则一致：键为非空且不含 `=`/`\0` 的字符串，值为不含 `\0` 的字符串（覆盖/新增）或 `false`（移除继承变量）；无效抛出参数错误 (`arg #3`)。不影响宿主环境。 |
| `maxOutputBytes` | `number` | `65536` (64 KiB) | 必须为 `[1, 4194304]`（最高 4 MiB）区间内的整数；无效抛出参数错误 (`arg #3`)。分别作用于 `stdout` 与 `stderr`，超出部分丢弃并设置对应 `stdoutTruncated`/`stderrTruncated = true`，绝不阻塞子进程写管道。 |

原有字段 `stdin`（`string`）、`timeoutMs`（正整数，默认 `30000`）及返回表 `{ exitCode, stdout, stderr, stdoutTruncated, stderrTruncated, timedOut, error? }` 保持不变。

---

## P0-2：修复 `host.process.start` 进程组清理

### 行为变更

- `host.process.start(program, args, onExit[, env])` 签名与返回行为保持完全不变。
- 在 Linux 上同样启用 `configureExecCancellation(cmd)`（设置 `Setpgid: true`，取消时向进程组 `-pid` 发送 `SIGKILL`），确保插件停止/卸载时回收 `start` 启动的进程及其派生子进程，避免孤儿进程泄漏。

---

## P0-3：新增可控流式异步进程 `host.process.spawn`

### 签名

```lua
local handle, err = host.process.spawn(program, args[, options])
```

- `program`：`string`，必填（直接执行，不经 shell）。
- `args`：`string[]`，必填（连续字符串数组，同 `exec`/`start` 校验）。
- `options`：`table`，可选：
  - `cwd` (`string`)：工作目录，非空且不含 `\0`。
  - `env` (`table`)：环境变量覆盖/移除表，同 `exec`/`start`。
  - `stdin` (`string`)：写入子进程标准输入的初始内容，随后关闭 stdin（EOF）。
  - `timeoutMs` (`number`)：正整数毫秒数；省略表示不设超时（仅受插件停止与 `handle:stop()` 约束）。超时后自动终止进程组，并在 `onExit` 中标记 `timedOut = true`。
  - `onStdout` (`function(chunk)`)：可选，接收聚合后的 stdout 字符串分块。省略时丢弃 stdout。
  - `onStderr` (`function(chunk)`)：可选，接收聚合后的 stderr 字符串分块。省略时丢弃 stderr。
  - `onExit` (`function(result)`)：可选，进程退出且所有待投递的 `onStdout`/`onStderr` 分块交付完成后调用一次。
- 插件进入停止流程后调用 `spawn` 返回 `nil, error`；启动失败返回 `nil, error`；参数类型/取值无效抛出 Lua 参数错误。

### `handle` 方法（userdata）

| 方法 | 返回值 | 行为 |
| --- | --- | --- |
| `handle:pid()` | `number` | 返回子进程 OS PID。 |
| `handle:running()` | `boolean` | 进程退出事件尚未结算时返回 `true`，否则 `false`。 |
| `handle:stop([graceMs])` | `boolean` | 幂等停止该进程。若已退出或已请求停止返回 `false`，否则发起终止并立即返回 `true`（不阻塞 Lua VM）。`graceMs` 为可选整数，范围 `[0, 30000]`，默认 `2000`：`graceMs == 0` 立即向进程组发 `SIGKILL`；否则先发 `SIGTERM`，超时未退出再发 `SIGKILL`。 |

### `onExit(result)` 结果表

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `pid` | `number` | 子进程 PID。 |
| `exitCode` | `number` | 退出码；被信号终止时为 `-1`。 |
| `timedOut` | `boolean` | 是否因 `timeoutMs` 超时被终止。 |
| `stopped` | `boolean` | 是否由 `handle:stop()` 主动停止。 |
| `stdoutDroppedBytes` | `number` | 因背压丢弃的 stdout 字节数（无丢弃为 `0`）。 |
| `stderrDroppedBytes` | `number` | 因背压丢弃的 stderr 字节数（无丢弃为 `0`）。 |
| `error` | `string?` | 非 `ExitError` 的等待/IO 错误信息（无错误时省略）。 |

### 流式合批、排序与背压约束

1. **防阻塞子进程**：每个启用的输出流使用自定义 `io.Writer` 接入 `cmd.Stdout`/`cmd.Stderr`（复用 `os/exec` 内部管道与 `WaitDelay` 保护），`Write()` 立即返回 `len(p), nil`，绝不因 Lua 消费慢而阻塞子进程写管道。
2. **有界内存**：每路流缓冲区硬上限 `256 KiB`；缓冲区满时丢弃新字节并累加 `stdoutDroppedBytes`/`stderrDroppedBytes`。
3. **时间片与块大小合批**：收到数据时按 `50ms` 窗口或 `16 KiB` 阈值合批通知 Lua 主循环；Lua 每次回调消费最多 `16 KiB`，若仍有余量则继续调度，保证 HTTP 请求可在分块间插队执行而不被饿死。
4. **严格退出顺序**：`cmd.Wait()` 返回后不再有新写入；Lua 主循环处理退出事件前会先排空该进程剩余缓冲并交付 `onStdout`/`onStderr`，最后才调用 `onExit`。
5. **全生命周期回收**：Linux 下启用 `Setpgid: true`；超时、主动 `stop()` 或插件停止/卸载均清理整个进程组，且 `Runtime.loop()` 退出前等待所有受管进程 goroutine 收敛。

---

## P1-1：WebSocket 订阅可观测性与事件缓冲扩大

### 改动点

1. **事件缓冲扩大**：将 `internal/lifecycle/instance.go` 的 `eventBuffer` 从 `32` 调整为 `128`。
2. **`host.app.subscriberCount()`**：返回当前插件活跃 WebSocket 订阅数（非负整数）。
3. **`host.app.on("subscribers", handler)`**：
   - `host.app.on` 新增支持 `"subscribers"` 事件（仍仅允许注册一次，重复注册报错）。
   - 当浏览器建立或断开 `/extensions/{id}/events` 连接导致活跃订阅数变化时，在 Lua 主循环串行调用 `handler(count)`，`count` 为当前权威订阅数。
   - 通知链路完全非阻塞（使用 1 槽合并信号通道），且在 `Instance` 释放内部互斥锁后触发，杜绝死锁与陈旧计数乱序。
   - 回调报错记录错误日志，不终止插件实例。

---

## P1-2：轻量通用文件操作模块 `host.fs`

所有接口遵循宿主账号系统权限，参数类型/路径非法（如空字符串或含 `\0`）抛出参数错误，文件系统错误返回 `nil, err`。

### 接口列表

| API | 返回值 | 说明 |
| --- | --- | --- |
| `host.fs.stat(path)` | `info` 或 `nil, err` | 基于 `os.Stat` + `os.Lstat` 返回 `{ name, size, mode, modTime, isDir, isFile, isSymlink }`；`mode` 为四位八进制字符串（如 `"0644"`），`modTime` 为 UTC RFC 3339。 |
| `host.fs.listDir(path)` | `entries` 或 `nil, err` | 按文件名升序返回数组表（空目录 JSON 编码为 `[]`），每项包含 `{ name, isDir, isFile, isSymlink, size?, modTime? }`。 |
| `host.fs.mkdirAll(path[, mode])` | `true` 或 `nil, err` | 递归创建目录。`mode` 可省略（默认 `0755`），支持整数（`0..511`）或八进制字符串（如 `"0700"`）。 |
| `host.fs.removeAll(path)` | `true` 或 `nil, err` | 递归删除文件或目录；拒绝根目录 `"/"` 等危险空/根路径。 |
| `host.fs.writeFile(path, data[, options])` | `true` 或 `nil, err` | 写入文件（支持二进制内容）。`options` 可选：`mode`（默认 `"0644"`，支持整数或八进制字符串）、`atomic`（布尔，默认 `true`，在同目录写临时文件 -> `chmod` -> `sync` -> `rename` 原子替换）、`mkdir`（布尔，默认 `false`，为 `true` 时自动创建父目录）。 |

---

## 实施拆分与验证

1. **Subagent 1（进程运行时）**：实现 P0-1、P0-2、P0-3，并补充 `host/lua_process_test.go` 集成测试（`exec` cwd/env/maxOutputBytes、`start` 进程组回收、`spawn` 流式 stdout/stderr、有序退出、主动 stop/grace、超时、背压丢弃计数、插件停止回收）。
2. **Subagent 2（事件与订阅）**：实现 P1-1 的 `internal/lifecycle/instance.go` 扩展（`eventBuffer = 128`、`SubscriberCount`、无锁死通知）及单元测试 `internal/lifecycle/instance_test.go`。
3. **Subagent 3（通用文件系统）**：实现 P1-2 的 `internal/lua/fs.go` 及单元测试 `internal/lua/fs_test.go`。
4. **主协调器集成与验收**：完成 `internal/lua/hostapi.go` 与 `internal/lua/runtime.go` 的统一接线，补充端到端集成测试 `host/lua_events_test.go` 与 `host/lua_fs_test.go`，更新 `README.md`，最终执行 `make check`（`go vet`、`go test ./...`、`go build`、`gofmt`）全部通过。

## v0.1.0 增补能力

- `host.http.request(options)` 发起 HTTP 或 HTTPS 请求，支持请求头、请求体、超时、重定向和 TLS 验证设置。返回 `status`、`headers`、`body`、`bodyTruncated` 和 `latencyMs`。响应体默认限制为 1 MiB，最高 8 MiB，超限返回截断内容并设置标记。
- `host.fs.readFile(path[, options])` 读取文本或二进制文件。`maxBytes` 默认 1 MiB，最高 8 MiB，超限返回 `nil, err`。
- `host.json.array([table])` 构造 JSON 数组表。空数组编码为 `[]`。
- `host.process.lookPath(name)` 查找可执行文件并返回绝对路径。`host.user` 只读地提供宿主进程的 `os`、`arch`、`uid`、`gid`、`homeDir` 和 `username`。
- `host.app.route` 支持整段路径参数 `{param}`，通过 `request.params` 读取解码后的值，精确路径优先。
- HTTP 处理器在可挂起的调用位置执行 `host.process.exec` 或 `host.http.request` 时，等待期间让出当前协程，其他请求和回调可继续串行执行 Lua 代码。生命周期钩子、其他回调和无法挂起的调用位置同步等待。
- `ext logs [plugin-id]` 查看插件日志，支持 `--data-dir`、`-n` / `--lines` 和 `-f` / `--follow`。

新增能力沿用 Host API 版本 1。完整参数与示例见 [README](../README.md#lua-运行时)。
