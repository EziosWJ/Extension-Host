# {{.Name}}

在项目根目录执行：

| 命令 | 用途 |
| --- | --- |
{{- if .Native }}
| `make build` | 编译 `backend/main.go` 到 `native/plugin`。修改后端后重新运行，`ext dev` 会自动重启插件。 |
{{- end }}
| `ext dev` | 自动安装并启用当前插件。终端会显示访问地址，保存文件后自动更新。 |
| `ext pkg` | 生成 `dist/{{.ID}}-<版本>{{ if .Native }}-{{.Platform}}{{ end }}.zip`，可在 GoDesk 的「扩展」或独立宿主的 `/host/` 安装。 |
{{- if .Native }}
| `make dev` / `make pkg` | 先编译原生后端，再执行 `ext dev` / `ext pkg`。 |
{{- end }}

## 改哪里

- `manifest.json` 记录插件标识、版本和入口。修改插件标识后，重新运行 `ext dev`。
{{- if .Native }}
- `backend/main.go` 是原生后端入口。它监听宿主提供的 Unix socket，通过 `GODESK_HOST_*` 环境变量报告就绪，示例导出 `v1.ping` 方法。修改后运行 `make build`。
- `native/plugin` 是构建生成的 Linux 可执行文件，会随插件打包；`backend/`、`go.mod` 和 `Makefile` 不进入 ZIP。
{{- end }}
{{- if .Web }}
- `web/index.html` 是网页入口。网页用相对路径引用样式、脚本和图片，资源放在 `web/` 中。
{{- end }}
{{- if .Lua }}
- `lua/init.lua` 是 Lua 入口。示例注册了 `GET /hello`，模块放在 `lua/` 中。
{{- end }}
{{- if and .Web .Lua }}
- `web/app.js` 通过 `../api/hello` 调用 Lua。改动 Lua 返回的文字，保存后再点页面上的「调用 Lua」，即可验证。
{{- else if and .Web .Native }}
- `web/app.js` 通过 `../api/hello` 调用原生后端。修改后端并运行 `make build` 后，再点页面上的「调用后端」验证。
{{- else if .Web }}
- 修改 `web/index.html` 里的欢迎文字并保存，即可在浏览器看到变化。
{{- else if .Native }}

后端已声明 `v1.ping` 方法。启用插件后，其他已启用插件可通过 Host API 的 `/v1/plugins/{{.ID}}/methods/v1.ping` 调用它。
{{- else }}

访问终端显示的宿主地址，管理页位于 `/host/`。在同一个地址下请求 `/extensions/{{.ID}}/api/hello` 即可验证 Lua 的响应。
{{- end }}

ZIP 包含 `manifest.json`、`web/`、`lua/` 和 `native/` 中实际存在的运行文件，以及 `manifest.json` 中通过 `icon` 声明的图标文件。生成的 Manifest 版本为 `0.1.0`，许可证占位为 `MIT`，发布前请按实际情况调整，并补齐 `description`、`author` 等市场元数据后运行 `ext validate --strict <ZIP 路径>`。开发用的配置、数据和日志位于项目的 `.ext/`。启动和更新失败时先看终端，插件自身的日志位于 `.ext/logs/by-plugin/`。

安装 ZIP 后，在管理页启用插件，再检查页面、Lua 或原生接口。Host API 和插件约定可查阅 [Extension Host 参考手册](https://github.com/EziosWJ/Extension-Host/blob/main/docs/host-reference.md)与 [GoDesk 插件契约](https://github.com/EziosWJ/GoDesk/blob/main/docs/extensions.md)。
