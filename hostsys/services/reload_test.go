package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func serviceTestScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertServiceErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var serviceErr *Error
	if !errors.As(err, &serviceErr) || serviceErr.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

func TestSystemdListReloadActions(t *testing.T) {
	path := serviceTestScript(t, `
case "$1" in
list-unit-files)
	printf '%s\n' 'yes.service enabled' 'no.service enabled' 'stopped.service disabled' 'oneshot.service static'
	;;
list-units)
	printf '%s\n' 'yes.service loaded active running Yes' 'no.service loaded active running No' \
		'stopped.service loaded inactive dead Stopped' 'oneshot.service loaded active exited Oneshot' \
		'alias.service loaded active running Alias'
	;;
show)
	printf '%s\n' 'Id=canonical.service' 'Names=canonical.service alias.service' 'CanReload=yes' '' \
		'Id=no.service' 'CanReload=no' '' 'Id=yes.service' 'CanReload=yes' '' \
		'Id=stopped.service' 'CanReload=yes' '' 'Id=oneshot.service' 'CanReload=yes'
	;;
*) exit 1 ;;
esac
`)
	for _, tc := range []struct {
		name string
		list func(context.Context) ([]Service, error)
		want []string
	}{
		{"native", newSystemd(path, "").List, []string{"alias.service", "yes.service"}},
		{"plugin", newPluginSystemd(path, "").List, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := tc.list(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 5 {
				t.Fatalf("listed %d services, want 5", len(list))
			}
			var got []string
			for _, service := range list {
				if ContainsAction(service.Actions, ActionReload) {
					got = append(got, service.Name)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("reload actions for %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNativeSystemctlReloadPrivilege(t *testing.T) {
	const path = "/usr/bin/systemctl"
	for _, args := range [][]string{
		{"reload", "--no-ask-password", "--", "demo.service"},
		{"daemon-reload", "--no-ask-password"},
	} {
		for _, tc := range []struct {
			name       string
			root       bool
			probeError error
			wantSudo   bool
		}{
			{name: "passwordless sudo", wantSudo: true},
			{name: "polkit fallback", probeError: errors.New("password required")},
			{name: "root", root: true},
		} {
			t.Run(args[0]+"/"+tc.name, func(t *testing.T) {
				cmd, usedSudo := nativeSystemctlCommand(t.Context(), path, tc.root, func(context.Context) error {
					if tc.root {
						t.Fatal("sudo probe invoked as root")
					}
					return tc.probeError
				}, args...)
				want := append([]string{path}, args...)
				if tc.wantSudo {
					want = append([]string{"/usr/bin/sudo", "-n", "-k", "-u", "root", "--"}, want...)
				}
				if cmd.Path != want[0] || usedSudo != tc.wantSudo || !reflect.DeepEqual(cmd.Args, want) {
					t.Fatalf("command = %v (sudo %v), want %v (sudo %v)", cmd.Args, usedSudo, want, tc.wantSudo)
				}
			})
		}
	}
}

func TestSystemdReloadOperations(t *testing.T) {
	path := serviceTestScript(t, `
case "$*" in
'reload --no-ask-password -- demo.service'|'daemon-reload --no-ask-password') ;;
*) printf 'Unexpected arguments: %s\n' "$*" >&2; exit 1 ;;
esac
case "$RELOAD_TEST_RESULT" in
permission) printf 'Interactive authentication required.\n' >&2; exit 1 ;;
failed) printf 'Job failed.\n' >&2; exit 1 ;;
missing) printf 'Unit demo.service not found.\n' >&2; exit 1 ;;
esac
`)
	backend := newSystemd(path, "").(*systemdBackend)
	backend.sudoProbe = func(context.Context) error { return errors.New("password required") }
	if caps := backend.Capabilities(); !caps.Reload || !caps.DaemonReload {
		t.Fatalf("systemd reload capabilities disabled: %+v", caps)
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"reload", func(ctx context.Context) error { return backend.Reload(ctx, "demo.service") }},
		{"daemon-reload", backend.DaemonReload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELOAD_TEST_RESULT", "")
			if err := tc.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			for result, code := range map[string]string{"permission": "permission", "failed": "operation_failed", "missing": "not_found"} {
				t.Setenv("RELOAD_TEST_RESULT", result)
				assertServiceErrorCode(t, tc.run(t.Context()), code)
			}
			expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			assertServiceErrorCode(t, tc.run(expired), "timeout")
		})
	}
	assertServiceErrorCode(t, backend.Reload(t.Context(), "*.service"), "invalid_unit")
}

func TestNoneReloadAndStreamLogs(t *testing.T) {
	backend := None()
	if caps := backend.Capabilities(); caps.Reload || caps.DaemonReload || caps.Logs {
		t.Fatalf("unexpected none capabilities: %+v", caps)
	}
	ctx := t.Context()
	assertServiceErrorCode(t, backend.Reload(ctx, "demo.service"), "unsupported")
	assertServiceErrorCode(t, backend.DaemonReload(ctx), "unsupported")
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", 0, func(string) error { return nil }), "logs_unavailable")
}

func TestSystemdReloadDeadlineWithInheritedOutput(t *testing.T) {
	path := serviceTestScript(t, `
sleep 30 &
printf '%s\n' "$!" > "$RELOAD_TEST_CHILD"
wait
`)
	backend := newSystemd(path, "").(*systemdBackend)
	backend.sudoProbe = func(context.Context) error { return errors.New("password required") }
	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"reload", func(ctx context.Context) error { return backend.Reload(ctx, "demo.service") }},
		{"daemon-reload", backend.DaemonReload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv("RELOAD_TEST_CHILD", pidFile)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			done := make(chan struct{})
			var runErr error
			go func() {
				defer close(done)
				runErr = tc.run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				data, _ := os.ReadFile(pidFile)
				if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); pid > 0 {
					child, err := os.FindProcess(pid)
					if err == nil {
						err = child.Kill()
						_ = child.Release()
					}
					if err != nil && !errors.Is(err, os.ErrProcessDone) {
						t.Errorf("cleaning up background PID %d: %v", pid, err)
					}
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("reload did not finish after cleaning up the background child")
				}
			})

			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				data, err := os.ReadFile(pidFile)
				if err == nil && strings.HasSuffix(string(data), "\n") {
					break
				}
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-done:
					t.Fatalf("reload returned before starting its background child: %v", runErr)
				case <-ctx.Done():
					t.Fatal("background child did not start before the deadline")
				case <-ticker.C:
				}
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("context expired before the background child was confirmed running: %v", err)
			}
			deadline, _ := ctx.Deadline()
			select {
			case <-done:
				assertServiceErrorCode(t, runErr, "timeout")
			case <-time.After(time.Until(deadline.Add(3 * time.Second))):
				t.Fatal("reload still blocked on inherited output 3s after its deadline")
			}
		})
	}
}
