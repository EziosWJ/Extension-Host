# ADR-0003：独立宿主托管 sudoers 规则

- **状态**：已实施
- **日期**：2026-11-28
- **关联文档**：
  - [ADR-0001：受信任插件使用宿主账号的系统权限](0001-trusted-plugins-use-host-account-permissions.md)
  - [Extension Host 参考](../host-reference.md)
  - [GoDesk 主机权限契约](https://github.com/EziosWJ/GoDesk/blob/main/docs/privileges.md)

## 背景

独立运行的 `ext` 宿主已经具备提权**执行**能力：插件的 `host.services.start/stop/restart` 传入 `{ sudo = true }` 后，非 root 宿主通过 `sudo -n -k -u root -- /usr/bin/systemctl …` 执行（`hostsys/services/sudo.go`、`hostsys/services/plugin.go`）。但仓库没有任何**配置**该能力的手段：

- CLI 没有安装、检查或移除 sudoers 规则的命令（`cmd/ext/main.go` 的分发与 `usage` 均无相关入口）。
- 没有安装脚本，也没有 `ext` 自身的 systemd unit 模板。
- 文档把责任完全交给系统管理员（`docs/host-reference.md`「systemd 服务」一节），只说明「由系统的 sudoers 授权决定能否执行」。
- 实际部署中，插件仓库各自提供手工规则示例，例如 `ext-caddy-manage/deploy/sudoers.example` 要求规则逐字匹配 `sudo` 命令的参数顺序、`--` 与单元名；写错只会得到运行期的 `permission` 错误。

GoDesk 已经解决了同一问题：`godesk --setup-sudo | --check-sudo | --remove-sudo` 以原子方式维护 `/etc/sudoers.d/godesk`，并在 `docs/privileges.md` 中固化了调用者识别、校验、回滚、并发与撤销语义。独立宿主缺少对等能力，导致同一套插件在两种运行模式下部署成本不一致。

## 决策

1. **新增托管规则能力，语义与 GoDesk 对齐。**
   - 新增 `internal/privileges` 包，提供规则生成、读取校验、加锁、`visudo` 校验、加载确认、`Setup`、`Check`、`Remove`，以及全仓库唯一的提权封装 `Command` / `Probe` / `Denied`。
   - 规则文件为 `/etc/sudoers.d/extension-host`，归属 `root:root`、权限 `0440`，首行标注由 Extension Host 托管，第二行记录授权 UID。
   - `Setup` 要求有效 UID 为 `0`，从 `SUDO_USER` / `SUDO_UID` 识别原始调用者并用账号记录核对；同一账号重复执行幂等，其他账号已持有时失败；发布前校验临时文件，原子落盘后确认系统已加载，失败即回滚。`Remove` 只允许规则所属账号发起，规则不存在时幂等成功。
   - 路径（`ruleDir`、`rulePath`、`visudo`、`sudo`）做成包级变量，默认值不变，测试用临时目录覆盖。
2. **CLI 使用子命令，而不是顶层 flag。**
   - 提供 `ext setup-sudo`、`ext check-sudo`、`ext remove-sudo`。
   - 原因是 `run()` 会把「无参数或以 `-` 开头」的调用直接交给 `serve`；若照搬 GoDesk 的 `--setup-sudo` 形式，必须额外改分发优先级。子命令与 `validate`、`pack` 等现有命令风格一致，也不影响 `ext --help` 之外的旧用法。
3. **首版规则沿用宽授权（`NOPASSWD: ALL`），与 GoDesk 一致。**
   - 插件实际只需要 `systemctl start/stop/restart`，但单元名来自请求，收窄必须依赖 sudoers 通配参数匹配，边界需要单独验证。
   - ADR-0001 已明确「由系统权限决定操作能否执行」；已启用插件本来就能以宿主账号执行任意程序，宽规则不扩大既有信任模型。
   - 若后续需要收窄，作为独立选项（如 `--scope services`）引入，并在本 ADR 追加决策，不改变默认行为。
4. **运行期行为不变。**
   - 仍使用 `sudo -n -k -u root --`；插件通道继续不做事先探测，只在 `sudo:` 诊断出现时返回 `permission`。
   - `hostsys/services/sudo.go` 收敛为对新包的转调，消除与 GoDesk 的重复实现，避免两边探测与拒绝判定漂移。
5. **嵌入模式保持现状。**
   - GoDesk 继续使用自己的 `godesk --setup-sudo` 与 `/etc/sudoers.d/godesk`；在 GoDesk 内嵌的宿主不因本仓库新增命令而改变权限来源。
   - 两边共用同一实现是后续可选项，不作为本决策的前置条件。
6. **Journal 读取权限不在本决策范围。**
   - 插件的日志读取依赖宿主账号加入 `systemd-journal` 组，与 sudoers 是两件事。可在 `check-sudo` 输出中提示，或另设安装命令，不并入接管提权规则。

## 后果

- **正面**：独立宿主获得与 GoDesk 一致的初始化、检查与撤销入口；部署者不必再为每个插件手写逐字匹配的规则；`check` 能区分「探测可用」与「规则归属」，便于排障；提权实现只保留一份。
- **代价与约束**：`NOPASSWD: ALL` 意味着该宿主账号（含全部已启用插件）可无密码取得 root，必须按 ADR-0001 在受信任环境中完成首次初始化，并在文档中显著说明；托管文件与手工规则同名冲突时直接失败，交由主机管理员处理；命令只能在 Linux + systemd + sudoers 环境生效，非 systemd 宿主仍走 `none` 后端，不因本能力受益。
