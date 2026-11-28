package privileges

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	ruleDir     = "/etc/sudoers.d"
	rulePath    = "/etc/sudoers.d/extension-host"
	sudoersPath = "/etc/sudoers"
	visudo      = "/usr/sbin/visudo"

	getenv       = os.Getenv
	chown        = os.Chown
	lookupUser   = user.Lookup
	lookupUserID = user.LookupId
	lookupGroup  = user.LookupGroup

	expectedOwnerUID uint32 = 0
	expectedOwnerGID uint32 = 0
)

const (
	ruleHeader = "# Managed by Extension Host\n"
	ruleGrant  = " ALL=(root) NOPASSWD: ALL\n"

	maxRuleSize = 1024
)

// invalidRule reports a managed rule whose content, ownership or permissions
// cannot be trusted.
var invalidRule = errors.New("托管规则的内容、归属或权限无效")

// ruleFor renders the managed sudoers rule for the given account UID.
func ruleFor(uid string) string {
	return ruleHeader + "#" + uid + ruleGrant
}

// ordinaryUID accepts a canonical decimal unsigned 32-bit UID that is neither
// root (0) nor the reserved all-ones value.
func ordinaryUID(uid string) bool {
	if uid == "" || uid[0] == '0' {
		return false
	}
	for _, r := range uid {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return false
	}
	if n == 0 || n >= 1<<32-1 {
		return false
	}
	return strconv.FormatUint(n, 10) == uid
}

// sudoCaller identifies the ordinary account that invoked the host through
// sudo. It refuses an empty, root or malformed identity and cross-checks the
// recorded UID against the account database.
func sudoCaller() (string, error) {
	name := getenv("SUDO_USER")
	uid := getenv("SUDO_UID")
	if name == "" || uid == "" {
		return "", errors.New("缺少 SUDO_USER 或 SUDO_UID，无法识别调用账号")
	}
	if name == "root" {
		return "", errors.New("拒绝 root 账号作为托管规则的调用者")
	}
	if !ordinaryUID(uid) {
		return "", fmt.Errorf("SUDO_UID %q 不是有效的普通账号 UID", uid)
	}
	account, err := lookupUser(name)
	if err != nil {
		return "", fmt.Errorf("无法查找账号 %q: %w", name, err)
	}
	if account.Username != name || account.Uid != uid {
		return "", fmt.Errorf("账号 %q 与 SUDO_UID %q 不一致", name, uid)
	}
	return uid, nil
}

// readRule reads and validates the managed rule, returning the UID it grants.
func readRule() (string, error) {
	fd, err := unix.Open(rulePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return "", invalidRule
		}
		return "", err
	}
	defer unix.Close(fd)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	if stat.Mode != unix.S_IFREG|0440 || stat.Uid != expectedOwnerUID || stat.Gid != expectedOwnerGID {
		return "", invalidRule
	}

	buf := make([]byte, maxRuleSize)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return "", err
	}
	if n == len(buf) {
		return "", invalidRule
	}
	content := string(buf[:n])
	uid, ok := strings.CutPrefix(content, ruleHeader+"#")
	if !ok {
		return "", invalidRule
	}
	uid, ok = strings.CutSuffix(uid, ruleGrant)
	if !ok || !ordinaryUID(uid) {
		return "", invalidRule
	}
	return uid, nil
}

// withRuleLock runs change while holding an exclusive lock on the sudoers
// drop-in directory. The directory itself must be root-owned and not
// group/world writable.
func withRuleLock(change func() error) error {
	if geteuid() != 0 {
		return errors.New("操作托管 sudoers 规则需要 root 权限")
	}
	fd, err := unix.Open(ruleDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", ruleDir, err)
	}
	defer unix.Close(fd)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("检查 %s: %w", ruleDir, err)
	}
	if stat.Uid != expectedOwnerUID || stat.Mode&0022 != 0 {
		return fmt.Errorf("%s 的归属或权限不安全: %w", ruleDir, invalidRule)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("锁定 %s: %w", ruleDir, err)
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return change()
}

// validate runs visudo with a stable locale and returns its combined output.
func validate(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, visudo, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// validateLoaded confirms that the installed rule grants uid and that the
// system has actually loaded it.
func validateLoaded(ctx context.Context, uid string) error {
	installed, err := readRule()
	if err != nil {
		return err
	}
	if installed != uid {
		return fmt.Errorf("托管规则授权给 UID %s，而非 UID %s: %w", installed, uid, invalidRule)
	}

	output, validateErr := validate(ctx, "-c")
	for line := range strings.Lines(output) {
		if strings.TrimSpace(line) == rulePath+": parsed OK" {
			return nil
		}
	}
	// visudo-rs and similar parsers may only report the main sudoers file.
	// Fall back to proving the drop-in directory is actually included.
	included, err := directlyIncludesRule()
	if err != nil {
		return fmt.Errorf("读取 %s: %w", sudoersPath, err)
	}
	if included {
		return nil
	}
	if validateErr != nil {
		return fmt.Errorf("托管规则尚未被 sudoers 加载: %w", validateErr)
	}
	return errors.New("托管规则尚未被 sudoers 加载")
}

// directlyIncludesRule reports whether the main sudoers file includes the
// managed drop-in directory or the rule file itself.
func directlyIncludesRule() (bool, error) {
	content, err := os.ReadFile(sudoersPath)
	if err != nil {
		return false, err
	}
	return parseIncludesRule(string(content)), nil
}

// parseIncludesRule understands @include/@includedir and #include/#includedir
// directives, including trailing comments.
func parseIncludesRule(content string) bool {
	wantDir := strings.TrimRight(ruleDir, "/")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		directive := fields[0]
		rest := strings.TrimSpace(strings.TrimPrefix(line, directive))
		if i := strings.Index(rest, "#"); i >= 0 {
			rest = rest[:i]
		}
		arg := strings.Trim(strings.TrimSpace(rest), `"`)
		switch directive {
		case "@includedir", "#includedir":
			if strings.TrimRight(arg, "/") == wantDir {
				return true
			}
		case "@include", "#include":
			if arg == rulePath {
				return true
			}
		}
	}
	return false
}

// Setup installs the managed sudoers rule for the account that invoked the
// host through sudo. It is idempotent for that account and refuses to overwrite
// a rule owned by anyone else.
func Setup(ctx context.Context) error {
	return withRuleLock(func() error {
		uid, err := sudoCaller()
		if err != nil {
			return err
		}

		installed, err := readRule()
		switch {
		case err == nil && installed == uid:
			return validateLoaded(ctx, uid)
		case err == nil:
			return fmt.Errorf("托管规则已授权给 UID %s，不能再授权给 UID %s", installed, uid)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}

		file, err := os.CreateTemp(ruleDir, ".extension-host-*")
		if err != nil {
			return fmt.Errorf("创建临时规则: %w", err)
		}
		tempPath := file.Name()
		defer os.Remove(tempPath)

		if _, err := file.WriteString(ruleFor(uid)); err != nil {
			file.Close()
			return fmt.Errorf("写入临时规则: %w", err)
		}
		if err := chown(tempPath, int(expectedOwnerUID), int(expectedOwnerGID)); err != nil {
			file.Close()
			return fmt.Errorf("设置临时规则归属: %w", err)
		}
		if err := file.Chmod(0o440); err != nil {
			file.Close()
			return fmt.Errorf("设置临时规则权限: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("关闭临时规则: %w", err)
		}

		if output, err := validate(ctx, "-c", "-f", tempPath); err != nil {
			return fmt.Errorf("校验临时规则失败: %w: %s", err, strings.TrimSpace(output))
		}

		// Hard-link publication is atomic and never replaces an existing rule.
		if err := os.Link(tempPath, rulePath); err != nil {
			return fmt.Errorf("发布托管规则: %w", err)
		}

		if err := validateLoaded(ctx, uid); err != nil {
			return errors.Join(err, os.Remove(rulePath))
		}
		return nil
	})
}

// Remove deletes the managed rule. Only the account the rule grants may remove
// it; an invoker without SUDO_* (a root shell) is allowed. A missing rule is
// treated as already removed.
func Remove() error {
	caller := ""
	if getenv("SUDO_USER") != "" || getenv("SUDO_UID") != "" {
		uid, err := sudoCaller()
		if err != nil {
			return err
		}
		caller = uid
	}

	return withRuleLock(func() error {
		uid, err := readRule()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if caller != "" && caller != uid {
			return fmt.Errorf("托管规则属于 UID %s，UID %s 不能移除", uid, caller)
		}
		if err := os.Remove(rulePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// Check reports the current privilege state to out. The returned error reflects
// only the passwordless-root probe; rule state never changes the exit status.
func Check(ctx context.Context, out io.Writer) error {
	probe := Probe(ctx)
	switch {
	case geteuid() == 0:
		fmt.Fprintln(out, "提权探测：当前已是 root")
	case probe == nil:
		fmt.Fprintln(out, "提权探测：免密 root 命令探测成功")
	default:
		fmt.Fprintf(out, "提权探测：免密 root 命令探测失败（%v）\n", probe)
	}

	installed, err := readRule()
	switch {
	case err == nil:
		switch {
		case geteuid() == 0:
			if err := validateLoaded(ctx, installed); err != nil {
				fmt.Fprintln(out, "Extension Host 托管规则：无法核实")
			} else {
				fmt.Fprintf(out, "Extension Host 托管规则：授权给 UID %s\n", installed)
			}
		case installed == strconv.Itoa(os.Getuid()):
			fmt.Fprintln(out, "Extension Host 托管规则：本账号已授权")
		default:
			fmt.Fprintln(out, "Extension Host 托管规则：其他账号已授权")
		}
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintln(out, "Extension Host 托管规则：不存在")
	case errors.Is(err, invalidRule):
		fmt.Fprintln(out, "Extension Host 托管规则：异常")
	default:
		fmt.Fprintln(out, "Extension Host 托管规则：无法核实")
	}

	if geteuid() != 0 {
		checkJournalGroup(out)
	}
	return probe
}

// checkJournalGroup prints a non-blocking hint when the current account cannot
// read the system journal. Lookup failures are ignored on purpose.
func checkJournalGroup(out io.Writer) {
	account, err := lookupUserID(strconv.Itoa(os.Getuid()))
	if err != nil || account == nil {
		return
	}
	group, err := lookupGroup("systemd-journal")
	if err != nil || group == nil {
		return
	}
	member, known := groupMembership(account, group)
	if !known || member {
		return
	}
	fmt.Fprintln(out, "提示：当前账号不在 systemd-journal 组内，插件读取 journal 日志前需要将账号加入该组（例如 sudo usermod -aG systemd-journal <账号>）。")
}

// groupMembership reports whether account belongs to group. known is false when
// the supplementary group list cannot be read.
func groupMembership(account *user.User, group *user.Group) (member, known bool) {
	if account.Gid == group.Gid {
		return true, true
	}
	ids, err := account.GroupIds()
	if err != nil {
		return false, false
	}
	for _, id := range ids {
		if id == group.Gid {
			return true, true
		}
	}
	return false, true
}
