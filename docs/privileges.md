# Extension Host 主机权限契约

本文档说明独立 Extension Host（`ext`）如何取得和管理主机权限。决策背景见 [ADR-0003：独立宿主托管 sudoers 规则](adr/0003-managed-sudoers-rule.md)，插件权限模型见 [ADR-0001：受信任插件使用宿主账号的系统权限](adr/0001-trusted-plugins-use-host-account-permissions.md)，命令与运行方式见 [Extension Host 参考手册](host-reference.md)。

## 运行身份与信任模型

`ext` 以启动它的 Linux 账号运行。遵循 ADR-0001，部署者启用自己信任的插件，已启用插件以宿主账号的系统权限执行，宿主不再单独维护逐插件权限。

需要提权的操作目前只有 `host.services.start/stop/restart` 传入 `{ sudo = true }` 的服务启停与重启。宿主以非 root 账号运行时，通过 `sudo -n -k -u root --` 执行：`-n` 表示非交互，凭据不可用时直接失败而不会提示输入密码；`-k` 表示本次不使用缓存凭据。宿主以 root 运行时直接执行，不经过 `sudo`。

能否提权由系统的 sudoers 规则决定。独立宿主提供下列子命令安装、检查和移除托管规则；嵌入 GoDesk 时的权限来源不同，见[与嵌入模式及 Journal 权限的边界](#与嵌入模式及-journal-权限的边界)。这些命令只在 Linux + systemd + sudoers 环境生效：非 systemd 宿主走 `none` 后端，`host.services` 返回不可用，托管规则也不会带来提权能力。

## 主机命令

三个命令都是子命令，不是顶层 flag。`setup-sudo` 与 `remove-sudo` 需由 `sudo` 发起，`check-sudo` 不需要。

### `sudo ext setup-sudo`

由运行 `ext` 的普通账号通过 `sudo` 发起，安装 `/etc/sudoers.d/extension-host`：

- **识别调用者**：从 `SUDO_USER` / `SUDO_UID` 取得原始账号，并与系统账号记录核对。无法确定授权对象时（例如缺少 `SUDO_USER`）命令失败。
- **规则文件**：归属 `root:root`、权限 `0440`，内容两行——首行 `# Managed by Extension Host` 标注托管来源，第二行 `#<uid> ALL=(root) NOPASSWD: ALL` 记录被授权的 UID（`#` 前缀是 sudoers 的数字 UID 语法）。
- **幂等**：同一账号重复执行成功，不重写已有规则。
- **冲突**：规则已授权其他账号时失败；`/etc/sudoers.d` 下存在同名的手工规则时失败。两种情况都交由主机管理员处理，命令不会覆盖或合并。
- **校验与落盘**：先对临时文件执行 `visudo -c` 校验，再用 `os.Link` 原子发布到目标路径，随后确认系统已加载规则；任一步失败都会回滚，不留下半成品。
- **并发**：对规则目录加 `flock` 排他锁，避免并发安装互相覆盖。

### `ext check-sudo`

以当前账号、无副作用地探测免密 root 是否可用（不写文件、不改规则），并单独报告托管规则的状态：

- 本账号已授权
- 其他账号已授权
- 规则不存在
- 规则异常（内容或权限不符合托管格式）
- 无法核实（无法读取规则目录或缺少 `visudo` 等）

退出码只由探测结果决定：免密 root 可用时为 `0`，否则为非零。规则归属只体现在输出中，不改变退出码，便于脚本用退出码判断「能否提权」、用输出判断「规则属于谁」。

输出附带 `systemd-journal` 组提示：宿主账号不在该组时读取服务 journal 日志会失败，需要单独把账号加入该组，这与 sudoers 规则无关。

### `sudo ext remove-sudo`

由规则所属账号通过 `sudo` 发起（或 root 直接执行）移除托管规则：

- 只删除 `/etc/sudoers.d/extension-host`，不触碰其他 sudoers 文件。
- 规则不存在时幂等成功。
- 非规则所属账号发起时失败。

## 安全约束

**`NOPASSWD: ALL` 意味着宿主账号——连同全部已启用插件——可以无密码取得 root。** 这是与 GoDesk 一致的宽授权：插件本来就能以宿主账号执行任意程序，收窄规则不会改变既有信任模型。因此：

- 只在受信任的主机上首次初始化，安装前确认启用的是自己信任的插件。
- 首次初始化应通过本地终端或受控的 SSH 会话完成，不要在不可信网络下远程执行。
- `ext` 默认只监听回环地址且没有登录；不要把未加访问控制的独立控制台暴露到公网，否则任何能访问控制台的人都能借插件取得 root。
- 不再需要提权时，用 `sudo ext remove-sudo` 撤销规则。

## 与嵌入模式及 Journal 权限的边界

- **嵌入 GoDesk**：在 GoDesk 内嵌运行时，权限来源仍由 GoDesk 管理——`/etc/sudoers.d/godesk` 与 `godesk --setup-sudo`。`ext setup-sudo` 等命令只服务独立宿主，不改变嵌入模式的规则文件或授权范围。
- **Journal 权限**：读取服务 journal 日志依赖宿主账号加入 `systemd-journal` 组，与 sudoers 规则相互独立。`check-sudo` 只做提示，不会修改组。
