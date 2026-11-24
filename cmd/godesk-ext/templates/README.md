# {{.Name}}

在项目根目录执行：

| 命令 | 用途 |
| --- | --- |
| `godesk-ext dev` | 自动安装并启用当前插件。终端会显示访问地址，保存文件后自动更新。 |
| `godesk-ext pkg` | 生成 `dist/{{.ID}}-<版本>.zip`，可在 GoDesk 的「扩展」或独立宿主的 `/host/` 安装。 |

## 改哪里

- `manifest.json` 记录插件标识、版本和入口。修改插件标识后，重新运行 `godesk-ext dev`。
{{- if .Web }}
- `web/index.html` 是网页入口。网页用相对路径引用样式、脚本和图片，资源放在 `web/` 中。
{{- end }}
{{- if .Lua }}
- `lua/init.lua` 是 Lua 入口。示例注册了 `GET /hello`，模块放在 `lua/` 中。
{{- end }}
{{- if and .Web .Lua }}
- `web/app.js` 通过 `../api/hello` 调用 Lua。改动 Lua 返回的文字，保存后再点页面上的「调用 Lua」，即可验证。
{{- else if .Web }}
- 修改 `web/index.html` 里的欢迎文字并保存，即可在浏览器看到变化。
{{- else }}

访问终端显示的宿主地址，管理页位于 `/host/`。在同一个地址下请求 `/extensions/{{.ID}}/api/hello` 即可验证 Lua 的响应。
{{- end }}

ZIP 只包含 `manifest.json`、`web/` 和 `lua/`。开发用的配置、数据和日志位于项目的 `.godesk-ext/`。启动和更新失败时先看终端，插件自身的日志位于 `.godesk-ext/logs/`。

安装 ZIP 后，在管理页启用插件，再检查页面或 Lua 接口。Host API 和插件约定可查阅 [Extension Host 文档](https://github.com/EziosWJ/Extension-Host#lua-运行时)。
