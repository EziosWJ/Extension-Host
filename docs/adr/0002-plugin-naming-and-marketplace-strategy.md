# ADR-0002：插件命名规范与静态市场演进策略

- **状态**：Phase 1–3 已实施（Phase 4 静态市场索引待实施）
- **日期**：2026-11-28
- **关联文档**：
  - [ADR-0001：受信任插件使用宿主账号的系统权限](0001-trusted-plugins-use-host-account-permissions.md)
  - [插件命名规范与市场化准备 Spec](../plugin-naming-and-marketplace-spec.md)

## 背景

目前 `Extension-Host` 的 `examples/` 目录及同级目录的独立插件仓库在命名与元数据结构上存在明显不一致：

1. **命名空间混乱**：同时存在 `com.godesk.*`（未持有 `godesk.com` 域名）、`cn.ezios.*`（早期个人域名）与 `com.example.*`（`ext init` 示例）。
2. **ID 与目录风格不统一**：存在缩写（`cn.ezios.ecm`）、无分隔拼接（`com.godesk.luaonly`）、kebab-case（`com.godesk.ops-wiki`）；仓库目录混用 `ext-*`、`godesk-*` 与嵌套目录 `examples/postgres-backup/plugin/`。
3. **Config 键名与展示名中英混杂**：配置键同时出现中文（`备份目录`）、`camelCase`（`caddyBinary`）和 `snake_case`（`wiki_dir`）。
4. **缺少市场元数据与索引契约**：当前 `manifest.json` 仅覆盖运行时必需字段，缺少简介、作者、许可证、图标、分类、i18n 以及静态分发索引标准。

## 决策

1. **反向域名（Reverse-DNS）所有权归属与示例隔离**：
   - 作者自有正式插件统一采用真实持有域名 `jianwang.de` 的三段式前缀 **`de.jianwang.<slug>`**。
   - 教学与能力示例统一采用 RFC 2606 保留域名 **`com.example.<slug>`**。
   - 市场索引准入强制校验反向域名规范，**禁止** `com.example.*` 及未持有所有权的占位前缀（如 `com.godesk.*`）上架。
2. **严格 `kebab-case` Slug 与 `ext-<slug>` 仓库命名**：
   - 市场准入与推荐规范要求 ID 每段仅使用 `[a-z0-9-]`（禁止缩写、下划线 `_` 与无连字符拼接）。
   - 独立插件仓库统一命名为 `ext-<slug>`；`Extension-Host/examples/` 仅保留精简能力示例，将 `postgres-backup` 拆出为独立仓库 `ext-postgres-backup`。
   - 对现有未正式发布的 8 个插件执行**一次性破坏性重命名对齐**，不保留旧 ID 兼容层。
3. **`schemaVersion: 1` 增量兼容演进**：
   - 不升级 `schemaVersion`，所有新增市场元数据字段（`description`、`author`、`license`、`homepage`、`repository`、`icon`、`categories`、`keywords`、`requirements`、`locales` 及 `config[*].label`/`description`）均为可选字段，保证旧宿主无缝加载。
   - `manifest.json` 主字段（`name`、`description`、`config[*].label`/`description`）统一采用英文基准，通过 `locales["zh-CN"]` 提供中文覆盖；`config` 键名统一英文 `camelCase`。
   - 本地 `ext dev` / `ext pack` 维持现有宽松校验；新增 `ext validate --strict` 用于市场准入严格检查（ID 格式、必填元数据、包内 `icon` 文件、英文 `camelCase` config 键名等）。
4. **遵循 ADR-0001 的静态市场架构**：
   - 严格遵循 [ADR-0001](0001-trusted-plugins-use-host-account-permissions.md)，**不引入逐插件 grants 或能力/权限沙箱声明**；外部命令/系统前提仅允许作为非强制的只读环境提示（`requirements`）供市场展示与安装前提示。
   - 首版市场采用**静态 Registry 索引（`index.json` + GitHub Release ZIP + SHA256）**，支持官方源与用户自定义源，不依赖中心化后端服务。
   - 图标（`icon`）强制随包内嵌，由索引构建脚本在生成 `index.json` 时提取发布；包完整性与信任依托 HTTPS 传输、`sha256` 校验及来源源显式展示，暂不引入离线公钥签名基础设施。

## 后果

- **正面**：插件 ID 与真实域名所有权绑定，避免公开市场命名冲突；旧宿主与新元数据前向兼容；静态市场零运维成本即可支撑多平台（Web/Lua 与 `linux-amd64`/`linux-arm64` 原生包）分发。
- **代价与约束**：一次性重命名会改变已有本地开发环境的 `plugins/<id>/` 数据目录和日志文件名，迁移时需清理本地 `ext-data/` / `.ext/` 或重装。
