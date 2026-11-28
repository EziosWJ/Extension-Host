package privileges

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// saveGlobals snapshots the package-level seams and restores them when the
// test finishes.
func saveGlobals(t *testing.T) {
	t.Helper()
	savedSudo, savedTrue, savedEUID := sudo, trueBin, geteuid
	savedRuleDir, savedRulePath := ruleDir, rulePath
	savedSudoers, savedVisudo := sudoersPath, visudo
	savedGetenv, savedChown := getenv, chown
	savedLookupUser, savedLookupUserID, savedLookupGroup := lookupUser, lookupUserID, lookupGroup
	savedUID, savedGID := expectedOwnerUID, expectedOwnerGID
	t.Cleanup(func() {
		sudo, trueBin, geteuid = savedSudo, savedTrue, savedEUID
		ruleDir, rulePath = savedRuleDir, savedRulePath
		sudoersPath, visudo = savedSudoers, savedVisudo
		getenv, chown = savedGetenv, savedChown
		lookupUser, lookupUserID, lookupGroup = savedLookupUser, savedLookupUserID, savedLookupGroup
		expectedOwnerUID, expectedOwnerGID = savedUID, savedGID
	})
}

type testEnv struct {
	dir         string
	ruleDir     string
	rulePath    string
	sudoersPath string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	saveGlobals(t)

	dir := t.TempDir()
	e := &testEnv{
		dir:         dir,
		ruleDir:     filepath.Join(dir, "sudoers.d"),
		rulePath:    filepath.Join(dir, "sudoers.d", "extension-host"),
		sudoersPath: filepath.Join(dir, "sudoers"),
	}
	if err := os.MkdirAll(e.ruleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.sudoersPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ruleDir, rulePath, sudoersPath = e.ruleDir, e.rulePath, e.sudoersPath
	getenv = func(string) string { return "" }
	chown = func(string, int, int) error { return nil }
	lookupUser, lookupUserID, lookupGroup = user.Lookup, user.LookupId, user.LookupGroup
	geteuid = os.Geteuid

	// Derive ownership expectations from a file created in the target
	// directory so that ownership checks pass even on unusual mounts.
	sample := filepath.Join(e.ruleDir, ".ownership-sample")
	if err := os.WriteFile(sample, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(sample, &stat); err != nil {
		t.Fatal(err)
	}
	expectedOwnerUID, expectedOwnerGID = stat.Uid, stat.Gid
	if err := os.Remove(sample); err != nil {
		t.Fatal(err)
	}

	e.useVisudoOK(t)
	return e
}

func (e *testEnv) script(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// useVisudoOK installs a fake visudo that accepts -c -f and reports the managed
// file as loaded for a plain -c.
func (e *testEnv) useVisudoOK(t *testing.T) {
	t.Helper()
	visudo = e.script(t, "visudo-ok", fmt.Sprintf(`
has_file=0
for arg in "$@"; do
  if [ "$arg" = "-f" ]; then
    has_file=1
  fi
done
if [ "$has_file" = "1" ]; then
  exit 0
fi
printf '%%s: parsed OK\n' %q
exit 0
`, e.rulePath))
}

func (e *testEnv) useVisudoFail(t *testing.T) {
	t.Helper()
	visudo = e.script(t, "visudo-fail", "printf 'syntax error\\n' >&2\nexit 1\n")
}

// useVisudoRustOnlyMain mimics visudo-rs: only the main sudoers file is
// reported, never the drop-in file.
func (e *testEnv) useVisudoRustOnlyMain(t *testing.T) {
	t.Helper()
	visudo = e.script(t, "visudo-rust", fmt.Sprintf("printf '%%s: parsed OK\\n' %q\nexit 0\n", e.sudoersPath))
}

// useVisudoRollback accepts the temporary file but never reports the managed
// file as loaded.
func (e *testEnv) useVisudoRollback(t *testing.T) {
	t.Helper()
	visudo = e.script(t, "visudo-rollback", fmt.Sprintf(`
for arg in "$@"; do
  if [ "$arg" = "-f" ]; then
    exit 0
  fi
done
printf '%%s: parsed OK\n' %q
exit 0
`, e.sudoersPath))
}

func (e *testEnv) writeRule(t *testing.T, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(e.rulePath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.rulePath, mode); err != nil {
		t.Fatal(err)
	}
}

func setRoot(t *testing.T) {
	t.Helper()
	geteuid = func() int { return 0 }
}

func setNonRoot(t *testing.T) {
	t.Helper()
	geteuid = func() int { return 1 }
}

func setSudoCaller(t *testing.T, name, uid string) {
	t.Helper()
	getenv = func(key string) string {
		switch key {
		case "SUDO_USER":
			return name
		case "SUDO_UID":
			return uid
		default:
			return ""
		}
	}
	lookupUser = func(username string) (*user.User, error) {
		if username != name {
			return nil, user.UnknownUserError(username)
		}
		return &user.User{Username: name, Uid: uid}, nil
	}
}

func wantContains(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("output missing %q:\n%s", s, got)
		}
	}
}

func TestCommand(t *testing.T) {
	saveGlobals(t)

	geteuid = func() int { return 0 }
	cmd := Command(context.Background(), "/bin/echo", "a", "b")
	if got, want := cmd.Args, []string{"/bin/echo", "a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("root command args = %q, want %q", got, want)
	}

	geteuid = func() int { return 1 }
	cmd = Command(context.Background(), "/usr/bin/systemctl", "status", "demo.service")
	want := []string{"/usr/bin/sudo", "-n", "-k", "-u", "root", "--", "/usr/bin/systemctl", "status", "demo.service"}
	if got := cmd.Args; !slices.Equal(got, want) {
		t.Fatalf("sudo command args = %q, want %q", got, want)
	}
}

func TestProbe(t *testing.T) {
	saveGlobals(t)
	ctx := context.Background()

	geteuid = func() int { return 0 }
	sudo = "/definitely/missing/sudo"
	if err := Probe(ctx); err != nil {
		t.Fatalf("root probe = %v, want nil", err)
	}

	e := newTestEnv(t)
	geteuid = func() int { return 1 }

	sudo = e.script(t, "sudo-ok", "exit 0\n")
	if err := Probe(ctx); err != nil {
		t.Fatalf("successful probe = %v, want nil", err)
	}

	sudo = e.script(t, "sudo-denied", "printf 'sudo: a password is required\\n' >&2\nexit 1\n")
	err := Probe(ctx)
	if err == nil {
		t.Fatal("probe should fail when sudo requires a password")
	}
	if !strings.Contains(err.Error(), "sudo: a password is required") {
		t.Errorf("probe error = %v, want sudo diagnostic", err)
	}

	sudo = e.script(t, "sudo-silent", "exit 1\n")
	if err := Probe(ctx); err == nil || strings.Contains(err.Error(), "password") {
		t.Errorf("silent failure = %v, want bare exit error", err)
	}
}

func TestDenied(t *testing.T) {
	saveGlobals(t)
	sudo = "/usr/bin/sudo"

	if !Denied(&os.PathError{Op: "fork/exec", Path: sudo, Err: fs.ErrNotExist}, "") {
		t.Error("PathError for sudo should be denied")
	}
	if !Denied(nil, "sudo: a password is required\n") {
		t.Error("sudo diagnostic should be denied")
	}
	if !Denied(nil, "some output\nsudo: no tty present\n") {
		t.Error("a later sudo diagnostic should be denied")
	}

	// A systemctl diagnostic must not be mistaken for sudo's own error.
	if Denied(&os.PathError{Op: "fork/exec", Path: "/usr/bin/systemctl", Err: fs.ErrNotExist}, "") {
		t.Error("systemctl PathError must not be denied")
	}
	cmd := exec.CommandContext(context.Background(), "/definitely/missing/systemctl", "status")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("missing systemctl unexpectedly succeeded")
	}
	if Denied(err, string(out)) {
		t.Errorf("systemctl failure %v misreported as sudo denial", err)
	}
	if Denied(nil, "systemctl: command not found\n") {
		t.Error("systemctl diagnostic must not be denied")
	}
}

func TestRuleFor(t *testing.T) {
	want := "# Managed by Extension Host\n#1000 ALL=(root) NOPASSWD: ALL\n"
	if got := ruleFor("1000"); got != want {
		t.Fatalf("ruleFor = %q, want %q", got, want)
	}
}

func TestOrdinaryUID(t *testing.T) {
	valid := []string{"1", "1000", "65534", "4294967294"}
	for _, uid := range valid {
		if !ordinaryUID(uid) {
			t.Errorf("ordinaryUID(%q) = false", uid)
		}
	}
	invalid := []string{"", "0", "00", "01", "0001000", "-1", "1.0", " 1", "1 ", "abc", "0x10", "4294967295", "4294967296"}
	for _, uid := range invalid {
		if ordinaryUID(uid) {
			t.Errorf("ordinaryUID(%q) = true", uid)
		}
	}
}

func TestParseIncludesRule(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"at includedir", "@includedir " + e.ruleDir + "\n", true},
		{"at includedir trailing slash", "@includedir " + e.ruleDir + "/\n", true},
		{"at includedir comment", "@includedir " + e.ruleDir + " # managed\n", true},
		{"hash includedir", "#includedir " + e.ruleDir + "\n", true},
		{"hash includedir comment", "#includedir " + e.ruleDir + " # managed\n", true},
		{"at include", "@include " + e.rulePath + "\n", true},
		{"hash include", "#include " + e.rulePath + "\n", true},
		{"plain comment", "# this mentions " + e.ruleDir + "\n", false},
		{"other directory", "@includedir /etc/other.d\n", false},
		{"other file", "@include /etc/other\n", false},
		{"empty", "\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseIncludesRule(c.content); got != c.want {
				t.Errorf("parseIncludesRule(%q) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

func TestSudoCaller(t *testing.T) {
	newTestEnv(t)

	setSudoCaller(t, "alice", "1000")
	uid, err := sudoCaller()
	if err != nil || uid != "1000" {
		t.Fatalf("sudoCaller = %q, %v", uid, err)
	}

	cases := []struct {
		name   string
		env    map[string]string
		lookup func(string) (*user.User, error)
	}{
		{"missing user", map[string]string{"SUDO_UID": "1000"}, nil},
		{"missing uid", map[string]string{"SUDO_USER": "alice"}, nil},
		{"root user", map[string]string{"SUDO_USER": "root", "SUDO_UID": "0"}, nil},
		{"leading zero", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "0100"}, nil},
		{"reserved uid", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "4294967295"}, nil},
		{"lookup failure", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "1000"}, func(string) (*user.User, error) {
			return nil, user.UnknownUserError("alice")
		}},
		{"uid mismatch", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "1000"}, func(string) (*user.User, error) {
			return &user.User{Username: "alice", Uid: "1001"}, nil
		}},
		{"name mismatch", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "1000"}, func(string) (*user.User, error) {
			return &user.User{Username: "bob", Uid: "1000"}, nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := c.env
			getenv = func(k string) string { return env[k] }
			if c.lookup != nil {
				lookupUser = c.lookup
			} else {
				lookupUser = user.Lookup
			}
			if _, err := sudoCaller(); err == nil {
				t.Errorf("sudoCaller accepted %v", c.env)
			}
		})
	}
}

func TestSetupRequiresRoot(t *testing.T) {
	e := newTestEnv(t)
	setNonRoot(t)
	setSudoCaller(t, "alice", "1000")

	if err := Setup(context.Background()); err == nil {
		t.Fatal("Setup should fail without root")
	}
	if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rule should not exist, stat error = %v", err)
	}
}

func TestSetupRejectsInvalidCaller(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		lookup func(string) (*user.User, error)
	}{
		{"missing user", map[string]string{"SUDO_UID": "1000"}, nil},
		{"missing uid", map[string]string{"SUDO_USER": "alice"}, nil},
		{"root caller", map[string]string{"SUDO_USER": "root", "SUDO_UID": "0"}, nil},
		{"malformed uid", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "01"}, nil},
		{"lookup error", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "1000"}, func(string) (*user.User, error) {
			return nil, user.UnknownUserError("alice")
		}},
		{"uid mismatch", map[string]string{"SUDO_USER": "alice", "SUDO_UID": "1000"}, func(string) (*user.User, error) {
			return &user.User{Username: "alice", Uid: "1001"}, nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			setRoot(t)
			env := c.env
			getenv = func(k string) string { return env[k] }
			lookupUser = c.lookup
			if lookupUser == nil {
				lookupUser = user.Lookup
			}

			if err := Setup(context.Background()); err == nil {
				t.Fatal("Setup should reject the caller")
			}
			if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("rule should not exist, stat error = %v", err)
			}
		})
	}
}

func TestSetupCreatesRule(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")

	if err := Setup(context.Background()); err != nil {
		t.Fatalf("Setup = %v", err)
	}
	content, err := os.ReadFile(e.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), ruleFor("1000"); got != want {
		t.Errorf("rule content = %q, want %q", got, want)
	}
	info, err := os.Stat(e.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o440); got != want {
		t.Errorf("rule mode = %04o, want %04o", got, want)
	}
	var stat unix.Stat_t
	if err := unix.Stat(e.rulePath, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Uid != expectedOwnerUID || stat.Gid != expectedOwnerGID {
		t.Errorf("rule owner = %d:%d, want %d:%d", stat.Uid, stat.Gid, expectedOwnerUID, expectedOwnerGID)
	}

	leftovers, err := filepath.Glob(filepath.Join(e.ruleDir, ".extension-host-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestSetupIdempotent(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")

	if err := Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(e.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background()); err != nil {
		t.Fatalf("second Setup = %v", err)
	}
	second, err := os.ReadFile(e.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("rule changed across idempotent Setup: %q -> %q", first, second)
	}
}

func TestSetupRejectsOtherAccount(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	if err := Setup(context.Background()); err != nil {
		t.Fatal(err)
	}

	setSudoCaller(t, "bob", "2000")
	err := Setup(context.Background())
	if err == nil {
		t.Fatal("Setup should reject a different account")
	}
	if !strings.Contains(err.Error(), "已授权给 UID 1000") {
		t.Errorf("conflict error = %v", err)
	}
	content, err := os.ReadFile(e.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), ruleFor("1000"); got != want {
		t.Errorf("rule was overwritten: %q", got)
	}
}

func TestSetupRejectsUnmanagedFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("manual rule", func(t *testing.T) {
		e := newTestEnv(t)
		setRoot(t)
		setSudoCaller(t, "alice", "1000")
		e.writeRule(t, "root ALL=(ALL:ALL) ALL\n", 0o440)

		err := Setup(ctx)
		if !errors.Is(err, invalidRule) {
			t.Fatalf("Setup error = %v, want invalidRule", err)
		}
		content, err := os.ReadFile(e.rulePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "root ALL=(ALL:ALL) ALL\n" {
			t.Errorf("manual rule was modified: %q", content)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		e := newTestEnv(t)
		setRoot(t)
		setSudoCaller(t, "alice", "1000")
		target := filepath.Join(e.dir, "target")
		if err := os.WriteFile(target, []byte(ruleFor("1000")), 0o440); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, e.rulePath); err != nil {
			t.Fatal(err)
		}

		if err := Setup(ctx); !errors.Is(err, invalidRule) {
			t.Fatalf("Setup error = %v, want invalidRule", err)
		}
		info, err := os.Lstat(e.rulePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Error("symlink was replaced")
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != ruleFor("1000") {
			t.Errorf("symlink target was modified: %q", content)
		}
	})

	t.Run("wrong mode", func(t *testing.T) {
		e := newTestEnv(t)
		setRoot(t)
		setSudoCaller(t, "alice", "1000")
		e.writeRule(t, ruleFor("1000"), 0o644)

		if err := Setup(ctx); !errors.Is(err, invalidRule) {
			t.Fatalf("Setup error = %v, want invalidRule", err)
		}
		info, err := os.Stat(e.rulePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode changed to %04o", info.Mode().Perm())
		}
	})

	t.Run("wrong owner expectation", func(t *testing.T) {
		e := newTestEnv(t)
		setRoot(t)
		setSudoCaller(t, "alice", "1000")
		e.writeRule(t, ruleFor("1000"), 0o440)
		expectedOwnerUID++

		if err := Setup(ctx); !errors.Is(err, invalidRule) {
			t.Fatalf("Setup error = %v, want invalidRule", err)
		}
	})
}

func TestSetupVisudoFailureDoesNotPublish(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.useVisudoFail(t)

	if err := Setup(context.Background()); err == nil {
		t.Fatal("Setup should fail when visudo rejects the temporary file")
	}
	if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rule should not be published, stat error = %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(e.ruleDir, ".extension-host-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestSetupRollsBackWhenNotLoaded(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.useVisudoRollback(t)

	if err := Setup(context.Background()); err == nil {
		t.Fatal("Setup should fail when the rule is not loaded")
	}
	if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rule should be rolled back, stat error = %v", err)
	}
}

func TestSetupAcceptsVisudoRustWithIncludedir(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.useVisudoRustOnlyMain(t)
	if err := os.WriteFile(e.sudoersPath, []byte("@includedir "+e.ruleDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Setup(context.Background()); err != nil {
		t.Fatalf("Setup = %v", err)
	}
	if _, err := os.Stat(e.rulePath); err != nil {
		t.Fatalf("rule should exist: %v", err)
	}
}

func TestRemoveRequiresRoot(t *testing.T) {
	e := newTestEnv(t)
	setNonRoot(t)
	e.writeRule(t, ruleFor("1000"), 0o440)

	if err := Remove(); err == nil {
		t.Fatal("Remove should fail without root")
	}
	if _, err := os.Stat(e.rulePath); err != nil {
		t.Errorf("rule should remain: %v", err)
	}
}

func TestRemoveMissingRuleIsIdempotent(t *testing.T) {
	newTestEnv(t)
	setRoot(t)
	if err := Remove(); err != nil {
		t.Fatalf("Remove missing rule = %v", err)
	}
}

func TestRemoveOwnAccount(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.writeRule(t, ruleFor("1000"), 0o440)

	if err := Remove(); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rule should be removed, stat error = %v", err)
	}
}

func TestRemoveOtherAccountIsRejected(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.writeRule(t, ruleFor("2000"), 0o440)

	if err := Remove(); err == nil {
		t.Fatal("Remove should reject a rule owned by another account")
	}
	if _, err := os.Stat(e.rulePath); err != nil {
		t.Errorf("rule should remain: %v", err)
	}
}

func TestRemoveUnmanagedRuleIsRejected(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	e.writeRule(t, "root ALL=(ALL:ALL) ALL\n", 0o440)

	err := Remove()
	if !errors.Is(err, invalidRule) {
		t.Fatalf("Remove error = %v, want invalidRule", err)
	}
	if _, err := os.Stat(e.rulePath); err != nil {
		t.Errorf("unmanaged rule should remain: %v", err)
	}
}

func TestRemoveFromRootShell(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	e.writeRule(t, ruleFor("1000"), 0o440)

	if err := Remove(); err != nil {
		t.Fatalf("Remove from root shell = %v", err)
	}
	if _, err := os.Stat(e.rulePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rule should be removed, stat error = %v", err)
	}
}

func TestRemoveRejectsInvalidSudoIdentity(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	getenv = func(key string) string {
		if key == "SUDO_USER" {
			return "alice"
		}
		return ""
	}
	e.writeRule(t, ruleFor("1000"), 0o440)

	if err := Remove(); err == nil {
		t.Fatal("Remove should reject an incomplete sudo identity")
	}
	if _, err := os.Stat(e.rulePath); err != nil {
		t.Errorf("rule should remain: %v", err)
	}
}

func TestCheckProbeAndMissingRule(t *testing.T) {
	e := newTestEnv(t)
	geteuid = os.Geteuid
	if geteuid() == 0 {
		t.Skip("non-root scenario")
	}
	sudo = e.script(t, "sudo-ok", "exit 0\n")

	var out bytes.Buffer
	if err := Check(context.Background(), &out); err != nil {
		t.Fatalf("Check = %v", err)
	}
	wantContains(t, out.String(), "免密 root 命令探测成功", "Extension Host 托管规则：不存在")
}

func TestCheckProbeFailureReturnsError(t *testing.T) {
	e := newTestEnv(t)
	geteuid = os.Geteuid
	if geteuid() == 0 {
		t.Skip("non-root scenario")
	}
	sudo = e.script(t, "sudo-denied", "printf 'sudo: a password is required\\n' >&2\nexit 1\n")

	var out bytes.Buffer
	err := Check(context.Background(), &out)
	if err == nil {
		t.Fatal("Check should return the probe error")
	}
	wantContains(t, out.String(), "免密 root 命令探测失败", "Extension Host 托管规则：不存在")
}

func TestCheckRuleStates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root scenario")
	}

	t.Run("own account", func(t *testing.T) {
		e := newTestEnv(t)
		sudo = e.script(t, "sudo-ok", "exit 0\n")
		e.writeRule(t, ruleFor(strconv.Itoa(os.Getuid())), 0o440)

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		wantContains(t, out.String(), "Extension Host 托管规则：本账号已授权")
	})

	t.Run("other account", func(t *testing.T) {
		e := newTestEnv(t)
		sudo = e.script(t, "sudo-ok", "exit 0\n")
		other := "1"
		if os.Getuid() == 1 {
			other = "2"
		}
		e.writeRule(t, ruleFor(other), 0o440)

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		wantContains(t, out.String(), "Extension Host 托管规则：其他账号已授权")
	})

	t.Run("invalid rule", func(t *testing.T) {
		e := newTestEnv(t)
		sudo = e.script(t, "sudo-ok", "exit 0\n")
		e.writeRule(t, ruleFor("1000"), 0o644)

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		wantContains(t, out.String(), "Extension Host 托管规则：异常")
	})

	t.Run("unreadable rule", func(t *testing.T) {
		e := newTestEnv(t)
		sudo = e.script(t, "sudo-ok", "exit 0\n")
		if err := os.Mkdir(e.rulePath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(e.rulePath, 0o755) })

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		wantContains(t, out.String(), "Extension Host 托管规则：无法核实")
	})
}

func TestCheckAsRoot(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	e.writeRule(t, ruleFor("1000"), 0o440)

	var out bytes.Buffer
	if err := Check(context.Background(), &out); err != nil {
		t.Fatalf("Check = %v", err)
	}
	wantContains(t, out.String(), "提权探测：当前已是 root", "Extension Host 托管规则：授权给 UID 1000")
}

func TestCheckAsRootUnloaded(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	e.writeRule(t, ruleFor("1000"), 0o440)
	e.useVisudoFail(t)

	var out bytes.Buffer
	if err := Check(context.Background(), &out); err != nil {
		t.Fatalf("Check = %v", err)
	}
	wantContains(t, out.String(), "Extension Host 托管规则：无法核实")
}

func TestCheckJournalHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root scenario")
	}

	prepare := func(t *testing.T) (*testEnv, *user.User) {
		e := newTestEnv(t)
		sudo = e.script(t, "sudo-ok", "exit 0\n")
		account, err := user.Current()
		if err != nil {
			t.Skipf("无法读取当前账号: %v", err)
		}
		return e, account
	}

	t.Run("not a member", func(t *testing.T) {
		_, account := prepare(t)
		ids, err := account.GroupIds()
		if err != nil {
			t.Skipf("无法枚举用户组: %v", err)
		}
		gid := "999999999"
		for _, id := range ids {
			if id == gid {
				t.Skip("unexpected group collision")
			}
		}
		lookupUserID = func(string) (*user.User, error) { return account, nil }
		lookupGroup = func(name string) (*user.Group, error) {
			return &user.Group{Name: name, Gid: gid}, nil
		}

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		wantContains(t, out.String(), "systemd-journal")
	})

	t.Run("member via primary group", func(t *testing.T) {
		_, account := prepare(t)
		lookupUserID = func(string) (*user.User, error) { return account, nil }
		lookupGroup = func(name string) (*user.Group, error) {
			return &user.Group{Name: name, Gid: account.Gid}, nil
		}

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		if strings.Contains(out.String(), "systemd-journal") {
			t.Errorf("unexpected hint for a member:\n%s", out.String())
		}
	})

	t.Run("lookup group fails", func(t *testing.T) {
		_, account := prepare(t)
		lookupUserID = func(string) (*user.User, error) { return account, nil }
		lookupGroup = func(string) (*user.Group, error) {
			return nil, user.UnknownGroupError("systemd-journal")
		}

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		if strings.Contains(out.String(), "systemd-journal") {
			t.Errorf("lookup failure should stay silent:\n%s", out.String())
		}
	})

	t.Run("lookup user fails", func(t *testing.T) {
		e, _ := prepare(t)
		lookupUserID = func(string) (*user.User, error) {
			return nil, user.UnknownUserError("nobody")
		}
		lookupGroup = func(name string) (*user.Group, error) {
			return &user.Group{Name: name, Gid: "999999999"}, nil
		}

		var out bytes.Buffer
		if err := Check(context.Background(), &out); err != nil {
			t.Fatalf("Check = %v", err)
		}
		if strings.Contains(out.String(), "systemd-journal") {
			t.Errorf("lookup failure should stay silent:\n%s", out.String())
		}
		_ = e
	})
}

func TestConcurrentSetupAndRemove(t *testing.T) {
	e := newTestEnv(t)
	setRoot(t)
	setSudoCaller(t, "alice", "1000")
	ctx := context.Background()

	const n = 10
	errs := make(chan error, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- Setup(ctx)
		}()
		go func() {
			defer wg.Done()
			errs <- Remove()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent operation: %v", err)
		}
	}

	leftovers, globErr := filepath.Glob(filepath.Join(e.ruleDir, ".extension-host-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestSudoersIntegration exercises the real system only when explicitly
// requested on a root shell.
func TestSudoersIntegration(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要以 root 运行")
	}
	if os.Getenv("EXT_TEST_SUDO_INTEGRATION") != "1" {
		t.Skip("未设置 EXT_TEST_SUDO_INTEGRATION=1")
	}
	ctx := context.Background()
	if err := Probe(ctx); err != nil {
		t.Fatalf("Probe = %v", err)
	}
	out, err := validate(ctx, "-c")
	if err != nil {
		t.Fatalf("visudo -c = %v: %s", err, out)
	}
	if !strings.Contains(out, "parsed OK") {
		t.Errorf("visudo -c output = %q", out)
	}
}
