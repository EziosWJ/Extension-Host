# 插件命名规范与市场化准备 Spec

> 关联决策：[ADR-0002：插件命名规范与静态市场演进策略](adr/0002-plugin-naming-and-marketplace-strategy.md) · [ADR-0001：受信任插件使用宿主账号的系统权限](adr/0001-trusted-plugins-use-host-account-permissions.md)

本文档定义 Extension Host 插件体系的统一命名规范、面向市场分发的 `manifest.json` 扩展字段、静态 Registry `index.json` 契约，以及现有插件的迁移对照表与分阶段实施计划。

---

## 1. 命名规范

### 1.1 插件 ID（`manifest.json` `id`）

插件 ID 是宿主数据库主键、URL 路径（`/extensions/{id}/...`）、数据目录（`plugins/<id>/`）与 ZIP 包名（`<id>-<version>.zip`）的唯一标识，发布后不可变更。

1. **命名空间（Reverse-DNS）**：
   - **作者自有正式插件**：使用真实持有的域名 `jianwang.de`，采用三段式 **`de.jianwang.<slug>`**。
   - **教学与能力示例插件**：使用 RFC 2606 保留域 **`com.example.<slug>`**。
   - **第三方市场插件**：发布者必须使用自己拥有所有权的域名反转前缀（如 `io.github.<username>.<slug>` 或个人/企业域名）。
   - **市场准入禁区**：`com.example.*` 及未持有所有权的占位前缀（如 `com.godesk.*`）禁止上架任何公共或官方市场索引。
2. **Slug 语法与风格（Strict Mode）**：
   - 严格采用小写点分隔的 **kebab-case**：每段仅允许 `[a-z0-9-]`，首尾必须是小写字母或数字，禁止连续 `--`、禁止下划线 `_`、禁止缩写（如 `ecm`）或无连字符拼接（如 `luaonly`）。
   - 市场校验正则：
     ```text
     ^[a-z0-9]+(?:-[a-z0-9]+)*(?:\.[a-z0-9]+(?:-[a-z0-9]+)*){2,}$
     ```
     （至少三段，例如 `de.jianwang.caddy-manage`、`com.example.lua-only`。）
   - **向后兼容**：`internal/manifest/manifest.go` 的默认 `Parse` / `Validate` 暂保留对历史 `_` 与两段式 ID 的兼容；市场严格模式（`ext validate --strict` 与索引构建器）按上述严格规则校验。

### 1.2 仓库、目录与 ZIP 产物命名

| 对象 | 规范 | 示例 |
| --- | --- | --- |
| 独立插件仓库 / 本地目录 | `ext-<slug>`（与宿主生态对齐，不绑定 `godesk-`） | `ext-caddy-manage`、`ext-ops-wiki`、`ext-postgres-backup` |
| 示例插件目录 | 置于 `Extension-Host/examples/<slug>/`，根目录直接放 `manifest.json`（禁止额外嵌套子目录） | `examples/hello/`、`examples/lua-only/` |
| 平台无关包（Web / Lua） | `<id>-<version>.zip` | `de.jianwang.caddy-manage-0.1.0.zip` |
| 原生可执行包（Native） | `<id>-<version>-<platform>.zip` | `de.jianwang.worker-0.1.0-linux-amd64.zip` |

### 1.3 `examples/` 目录定位边界

- `Extension-Host/examples/` **仅保留轻量级教学与能力验收模板**：`hello`、`lua-only`、`web-only`、`services`。
- 具备完整业务复杂度与外部工具链依赖的应用插件（如 `examples/postgres-backup/plugin/`）**拆出为独立插件仓库**（`ext-postgres-backup`），根目录直接放置 `manifest.json`。

### 1.4 Config 键名与展示名（`name`）语言基准

- **`config` 键名**：强制英文 `camelCase`（与 `schemaVersion`、`hostApi`、`minHostVersion`、`webApi` 风格统一），禁止中文键名（如 `"备份目录"`）及 `snake_case`（如 `wiki_dir`）。
- **基准语言**：`manifest.json` 的主展示字段（`name`、`description`、`config[*].label`、`config[*].description`）统一使用**英文**；中文等本地化文本通过 `locales["zh-CN"]` 提供。

---

## 2. 现有插件迁移对照表（一次性破坏性对齐）

本批插件均尚未公开发版，统一按一次性破坏性重命名对齐：

| 当前位置 | 当前 `id` | 目标位置 / 仓库 | 目标 `id` | 目标 `name`（主 / `zh-CN`） | Config 键重命名 |
| --- | --- | --- | --- | --- | --- |
| `examples/hello` | `com.godesk.hello` | `examples/hello` | `com.example.hello` | `Hello Extension` / `Hello 示例` | `greeting`, `token`（已合规） |
| `examples/lua-only` | `com.godesk.luaonly` | `examples/lua-only` | `com.example.lua-only` | `Lua-only Example` / `纯 Lua 示例` | 无 |
| `examples/web-only` | `com.godesk.webonly` | `examples/web-only` | `com.example.web-only` | `Web-only Example` / `纯网页示例` | 无 |
| `examples/services` | `com.godesk.services` | `examples/services` | `com.example.services` | `Services Example` / `系统服务示例` | 无 |
| `examples/postgres-backup/plugin` | `com.godesk.postgres-backup` | 独立仓库 `ext-postgres-backup`（扁平化） | `de.jianwang.postgres-backup` | `PostgreSQL Backup` / `PostgreSQL 备份` | `备份目录` → `backupDir`<br>`并行任务上限` → `maxParallelTasks` |
| `ext-caddy-manage` | `cn.ezios.ecm` | `ext-caddy-manage` | `de.jianwang.caddy-manage` | `Caddy Manager` / `Caddy 管理` | `camelCase`（已合规，补 `label`/`locales`） |
| `ext-container-manage` | `cn.ezios.container-manage` | `ext-container-manage` | `de.jianwang.container-manage` | `Container Manager` / `容器管理` | 无 |
| `godesk-ops-wiki` | `com.godesk.ops-wiki` | `ext-ops-wiki` | `de.jianwang.ops-wiki` | `Ops Wiki` / `运维知识库` | `wiki_dir` → `wikiDir` |

> **迁移注意事项**：修改 ID 与 `config` 键名属于破坏性变更，需同步更新 `Extension-Host/Makefile`、`cmd/ext` 相关集成测试及各插件内硬编码的 API 路径/存储前缀（如有），并在本地清理旧 `ext-data/` 与 `.ext/` 后重新安装验证。

---

## 3. `manifest.json` 面向市场的扩展契约（`schemaVersion: 1`）

保持 `schemaVersion: 1` 不变，新增字段全部为**可选增量字段**，未升级的旧版宿主可直接忽略；`ext validate --strict` 用于检查上架市场所需的必填元数据。

### 3.1 新增顶层市场字段

| 字段 | 类型 | 严格模式 | 说明与校验规则 |
| --- | --- | --- | --- |
| `description` | `string` | 必填 | 一句话英文简介，建议 `<= 160` 字符，不含换行。 |
| `author` | `object` | 必填 | 作者/发布者信息：`{ "name": "Jian Wang", "url"?: "https://...", "email"?: "..." }`（`name` 必填）。 |
| `license` | `string` | 必填 | SPDX 许可证标识（如 `"MIT"`、`"Apache-2.0"`、`"GPL-3.0-only"`）。 |
| `homepage` | `string` | 可选 | 项目主页 `https://` URL。 |
| `repository` | `string` | 建议 | 源码仓库 `https://` URL（便于安全审查与 Issue 跳转）。 |
| `icon` | `string` | 建议 | **强制包内相对路径**（如 `"web/icon.svg"` 或 `"assets/icon.png"`），严禁外链 URL。仅限 `.svg` / `.png` / `.webp`，文件须实际存在于包内且单文件强制 `<= 256 KiB`。 |
| `categories` | `string[]` | 建议 | 1–3 个受控分类枚举值：`database`、`proxy`、`container`、`ops`、`monitoring`、`networking`、`security`、`utility`。 |
| `keywords` | `string[]` | 可选 | 最多 8 个小写 `kebab-case` 搜索关键词。 |
| `requirements` | `object` | 可选 | **纯环境前置提示**（非强制、非权限声明，遵守 ADR-0001）：`{ "os"?: ["linux"], "commands"?: ["pg_dump", "psql"], "systemd"?: true }`。仅用于市场展示与安装前可读提醒。 |
| `locales` | `object` | 可选 | 按 BCP-47 语言标签索引的本地化覆盖（如 `"zh-CN"`），结构见 3.3。 |

### 3.2 `ConfigField` 扩展字段

在保留现有 `type`、`required`、`secret`、`default`、`format`、`min`、`max` 的基础上新增：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `label` | `string` | 配置项英文显示标题（如 `"Backup Directory"`）。缺失时宿主回退显示键名。 |
| `description` | `string` | 配置项英文帮助说明，展示于表单输入框下方。 |

`ConfigField` 的键名在 `--strict` 模式下必须匹配 `^[a-z][a-zA-Z0-9]*$`（英文 `camelCase`）。

### 3.3 `locales` 结构

```json
"locales": {
  "zh-CN": {
    "name": "PostgreSQL 备份",
    "description": "支持多数据源、定时备份、恢复与切回的 PostgreSQL 运维插件。",
    "config": {
      "backupDir": {
        "label": "备份目录",
        "description": "宿主机上的绝对路径；留空时写入插件自身数据目录。"
      },
      "maxParallelTasks": {
        "label": "并行任务上限",
        "description": "同时运行的备份或恢复任务数量上限（1-16）。"
      }
    }
  }
}
```

### 3.4 完整示例：`de.jianwang.postgres-backup`

```json
{
  "schemaVersion": 1,
  "id": "de.jianwang.postgres-backup",
  "name": "PostgreSQL Backup",
  "version": "0.1.0",
  "minHostVersion": "0.1.0",
  "hostApi": { "min": 1, "max": 1 },
  "description": "Backup, browse schemas, and restore PostgreSQL databases with scheduled jobs.",
  "author": {
    "name": "Jian Wang",
    "url": "https://jianwang.de"
  },
  "license": "MIT",
  "repository": "https://github.com/EziosWJ/ext-postgres-backup",
  "icon": "web/icon.svg",
  "categories": ["database", "ops"],
  "keywords": ["postgresql", "backup", "restore", "pg-dump"],
  "requirements": {
    "os": ["linux"],
    "commands": ["psql", "pg_dump", "pg_restore", "install"]
  },
  "entry": {
    "lua": "lua/init.lua",
    "web": "web/index.html"
  },
  "config": {
    "backupDir": {
      "type": "string",
      "format": "writable-directory",
      "default": "",
      "label": "Backup Directory",
      "description": "Absolute directory path on host; defaults to plugin data directory when empty."
    },
    "maxParallelTasks": {
      "type": "number",
      "format": "integer",
      "required": true,
      "min": 1,
      "max": 16,
      "default": 2,
      "label": "Max Parallel Tasks",
      "description": "Maximum concurrent backup or restore tasks (1-16)."
    }
  },
  "locales": {
    "zh-CN": {
      "name": "PostgreSQL 备份",
      "description": "配置多数据源、浏览库表结构并执行 PostgreSQL 定时备份与恢复。",
      "config": {
        "backupDir": {
          "label": "备份目录",
          "description": "宿主机绝对路径；留空时写入插件数据目录。"
        },
        "maxParallelTasks": {
          "label": "并行任务上限",
          "description": "同时执行的备份或恢复任务数量上限（1-16）。"
        }
      }
    }
  }
}
```

### 3.5 `ext pack` / `ext pkg` 与 `ext validate --strict` 行为约定

- **打包资源收集**：除现有的 `manifest.json`、`web/`、`lua/`、`native/` 外，若 `manifest.json` 声明了 `icon`（且位于如 `assets/icon.png` 等独立目录），`ext pack` 与 `ext pkg` 须自动将该图标文件打入 ZIP（受原有相对路径、防穿越、大小和文件数限制约束）。
- **`ext validate --strict` 检查项**：
  1. `id` 符合三段式纯 kebab-case 且不属于保留/禁用前缀（`com.example.*`、`com.godesk.*` 等）；
  2. `description`、`author.name`、`license` 非空，且 `license` 为合法 SPDX 常见标识或非空规范字符串；
  3. 若声明了 `icon`，校验扩展名（`.svg`/`.png`/`.webp`）、文件存在性及大小上限；
  4. `config` 所有键名为英文 `camelCase`；
  5. `locales` 中引用的 `config` 键必须存在于顶层 `config` 中；
  6. 若存在 `requirements`，校验仅允许白名单结构（`os`、`commands`、`systemd`），杜绝权限 grants 类字段。

---

## 4. 静态市场 Registry 架构与 `index.json` 契约

首版市场不建设中心化服务端，采用类似 Homebrew Tap / Helm Repo 的**静态索引 + GitHub Release ZIP** 模式，支持官方源与用户自定义源并存。

### 4.1 索引仓库目录结构

```text
ext-registry/
  index.json                     # 全量市场索引（由 CI / 构建脚本生成）
  icons/
    de.jianwang.caddy-manage.svg # 从 Release ZIP 提取出的图标（与 index.json 同源）
    de.jianwang.ops-wiki.png
```

### 4.2 `index.json` Schema

```json
{
  "schemaVersion": 1,
  "name": "Jian Wang Official Extension Registry",
  "updatedAt": "2026-11-28T00:00:00Z",
  "plugins": [
    {
      "id": "de.jianwang.caddy-manage",
      "name": "Caddy Manager",
      "description": "Manage Caddy sites, Caddyfile blocks, TLS certificates, and reload safety.",
      "author": {
        "name": "Jian Wang",
        "url": "https://jianwang.de"
      },
      "license": "MIT",
      "homepage": "https://github.com/EziosWJ/ext-caddy-manage",
      "repository": "https://github.com/EziosWJ/ext-caddy-manage",
      "icon": "icons/de.jianwang.caddy-manage.svg",
      "categories": ["proxy", "ops"],
      "keywords": ["caddy", "reverse-proxy", "tls"],
      "locales": {
        "zh-CN": {
          "name": "Caddy 管理",
          "description": "自托管 Caddy 运维面板，支持站点表单、配置校验与安全热重载。"
        }
      },
      "latest": "0.1.0",
      "versions": [
        {
          "version": "0.1.0",
          "releasedAt": "2026-11-25T15:00:00Z",
          "minHostVersion": "0.1.0",
          "hostApi": { "min": 1, "max": 1 },
          "requirements": {
            "os": ["linux"],
            "commands": ["caddy", "openssl"],
            "systemd": true
          },
          "artifacts": {
            "any": {
              "url": "https://github.com/EziosWJ/ext-caddy-manage/releases/download/v0.1.0/de.jianwang.caddy-manage-0.1.0.zip",
              "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
              "size": 148290
            }
          }
        }
      ]
    }
  ]
}
```

### 4.3 多平台原生插件 `artifacts` 约定

- **Web / Lua 插件**：`artifacts` 使用固定键 `"any"`。
- **Native 插件**：`artifacts` 按平台键组织，支持 `"linux-amd64"` 与 `"linux-arm64"`（同一版本号下可同时挂载多个架构的 ZIP 包）：
  ```json
  "artifacts": {
    "linux-amd64": { "url": "...", "sha256": "...", "size": 4194304 },
    "linux-arm64": { "url": "...", "sha256": "...", "size": 4012000 }
  }
  ```
- **宿主安装匹配逻辑**：安装时若目标版本不存在 `"any"`，宿主根据当前 `runtime.GOOS + "-" + runtime.GOARCH` 匹配对应平台产物；下载后必须校验 `sha256` 与 `size`，再交由现有包校验与安装流程处理。

### 4.4 信任与安全边界

1. **传输与完整性**：索引 URL 与制品下载 URL 在生产环境中应使用 `https://`；安装前强制校验 `sha256`，不匹配立即拒绝并清理临时文件。
2. **来源显式展示**：宿主 UI 在浏览与确认安装市场插件时，须显式展示插件来自哪个**源名称与域名**（以及 `author`、`repository`、`requirements`），配合 [ADR-0001](adr/0001-trusted-plugins-use-host-account-permissions.md) 让部署者基于来源信任做出启用决策。
3. **离线签名**：首版不上 GPG/Minisign/Cosign 离线签名体系；未来如引入，可在 `artifacts[*]` 下增量增加可选的 `signature` 字段，不破坏 `index.json` `schemaVersion: 1`。

---

## 5. 分阶段实施计划

### Phase 1：`Extension-Host` Schema、打包与校验改造（已完成）
1. 更新 `internal/manifest`：新增可选市场字段（`Description`、`Author`、`License`、`Homepage`、`Repository`、`Icon`、`Categories`、`Keywords`、`Requirements`、`Locales`）及 `ConfigField` 的 `Label`/`Description`；保持 `schemaVersion = 1` 向后兼容。
2. 更新 `internal/bundle` 与 `cmd/ext`：支持打包收集声明的 `icon` 文件；为 `ext validate` 增加 `--strict` 市场准入模式；更新 `ext init` 模板提示（提示使用自有反向域名，示例保留 `com.example.*`）。
3. 补齐 `internal/manifest` 与 `cmd/ext` 单元测试。

### Phase 2：宿主管理页与 GoDesk UI 适配（已完成）
1. `internal/lifecycle` 视图模型透出插件元数据（`description`、`author`、`icon`、`locales` 等）与 `configSchema` 的 `label`/`description`。
2. `web/` 管理前端配置表单按当前语言优先读取 `locales[lang].config[key].label`/`description`，回退 `configSchema[key].label`，最后回退字段键名；插件列表展示图标、作者与简介（遵循 [前端设计规范](frontend-design.md)）。

### Phase 3：现有 8 个插件一次性对齐与仓库整理（已完成）
1. 将 `Extension-Host/examples/` 下 4 个教学示例迁移至 `com.example.{hello,lua-only,web-only,services}`，同步更新 `Makefile`、`README.md` 与集成测试。
2. 将 `examples/postgres-backup/plugin/` 拆出为独立仓库 `ext-postgres-backup`（根目录扁平化），ID 改为 `de.jianwang.postgres-backup`，配置键改为 `backupDir` / `maxParallelTasks` 并补全市场元数据与 `locales["zh-CN"]`。
3. 将同级目录 3 个独立插件对齐规范：
   - `ext-caddy-manage`：`cn.ezios.ecm` → `de.jianwang.caddy-manage`，补全市场元数据与 `locales["zh-CN"]`；
   - `ext-container-manage`：`cn.ezios.container-manage` → `de.jianwang.container-manage`，补全市场元数据与 `locales["zh-CN"]`；
   - `godesk-ops-wiki` → 重命名仓库为 `ext-ops-wiki`，ID 改为 `de.jianwang.ops-wiki`，配置键 `wiki_dir` → `wikiDir`，补全市场元数据与 `locales["zh-CN"]`。

### Phase 4：静态市场索引工具链与宿主安装接入（待实施）
1. 新建 `ext-registry` 索引仓库及自动构建脚本/GitHub Action：抓取各插件 Release ZIP → 运行 `ext validate --strict` → 提取 `icon` 至 `icons/` → 计算 `sha256` 与 `size` → 生成 `index.json`。
2. 在 `Extension-Host`（及嵌入模式 GoDesk）增加可配置的市场源列表与远程安装入口：拉取 `index.json`、展示分类/搜索/前置依赖、按平台筛选 artifact、下载并校验 `sha256` 后触发安装或升级。
