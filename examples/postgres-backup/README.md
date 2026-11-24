# PostgreSQL 备份示例

插件提供「立即备份」和「备份历史」。Lua 通过 `host.process.start` 直接在后台运行 PostgreSQL 官方的 `pg_dump`，并在完成后更新插件的 SQLite。GoDesk 用于日常运行，独立 `godesk-ext` 用于开发联调。两个宿主使用不同的数据目录时，各自保留备份历史和文件。

## 安装和配置

1. 在宿主的 `PATH` 中安装能连接源库的 `pg_dump`。在 Extension Host 仓库根目录运行 `make examples`。
2. 在 GoDesk 的「扩展」或 `godesk-ext` 的管理页安装 `dist/com.godesk.postgres-backup-0.3.0.zip`，再启用插件。升级旧版时，卸载时保留插件数据，再安装并启用新版。
3. 打开插件的「数据库连接」，填写主机、端口、数据库、用户和可选密码，点击「保存连接」。保存后可直接点击「立即备份」。
4. 在历史中查看开始时间、状态、备份文件名或失败原因。运行期间可以刷新或关闭窗口，插件会在完成时更新记录。

连接设置保存在当前插件的 `business.db`。密码以明文保存在这个本地 SQLite 中，网页只显示是否已保存密码。编辑时留空会保留之前保存的密码，勾选「清除已保存的密码」后保存可以清除。

已有 `PGSERVICE`、`PGSERVICEFILE` 和 `PGPASSFILE` 的部署可以继续使用原连接。第一次从页面保存连接后，新任务使用页面配置。

成功任务产生自定义格式的 `.dump` 文件。GoDesk 的文件在 `<data-dir>/extensions/plugins/com.godesk.postgres-backup/data/`，独立宿主的文件在 `<data-dir>/plugins/com.godesk.postgres-backup/data/`。

## 验收

1. 点击「立即备份」，确认页面先显示「运行中」，刷新后仍能查询任务。重复提交应返回已有任务。
2. 用成功任务的备份文件恢复到一个事先创建的空测试库，再查询一条源库中已知的数据。

   ```sh
   pg_restore --exit-on-error --dbname=backup_restore_test /path/to/backup.dump
   ```

3. 刷新页面，确认连接信息仍在，密码框留空。将数据库名改为不存在的名称并保存，再次备份，确认历史中出现失败原因。
