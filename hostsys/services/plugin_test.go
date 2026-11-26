package services

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestRestartCommand(t *testing.T) {
	const (
		systemctl = "/usr/bin/systemctl"
		unit      = "mihomo.service"
	)
	directArgs := []string{systemctl, "restart", "--no-ask-password", "--", unit}
	tests := []struct {
		name     string
		elevated bool
		root     bool
		wantPath string
		wantArgs []string
		wantSudo bool
	}{
		{
			name:     "direct",
			wantPath: systemctl,
			wantArgs: directArgs,
		},
		{
			name:     "elevated",
			elevated: true,
			wantPath: "/usr/bin/sudo",
			wantArgs: []string{"/usr/bin/sudo", "-n", "-k", "-u", "root", "--", systemctl, "restart", "--no-ask-password", "--", unit},
			wantSudo: true,
		},
		{
			name:     "root ignores elevation",
			elevated: true,
			root:     true,
			wantPath: systemctl,
			wantArgs: directArgs,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, usedSudo := restartCommand(context.Background(), systemctl, unit, tc.elevated, tc.root)
			if cmd.Path != tc.wantPath {
				t.Errorf("command path = %q, want %q", cmd.Path, tc.wantPath)
			}
			if !reflect.DeepEqual(cmd.Args, tc.wantArgs) {
				t.Errorf("command args = %#v, want %#v", cmd.Args, tc.wantArgs)
			}
			if usedSudo != tc.wantSudo {
				t.Errorf("usedSudo = %v, want %v", usedSudo, tc.wantSudo)
			}
		})
	}
}

func TestServiceCommand(t *testing.T) {
	const (
		systemctl = "/usr/bin/systemctl"
		unit      = "caddy.service"
		sudo      = "/usr/bin/sudo"
	)
	for _, action := range []Action{ActionStart, ActionStop, ActionRestart} {
		for _, tc := range []struct {
			name           string
			elevated, root bool
			wantSudo       bool
		}{
			{name: "host account"},
			{name: "elevated", elevated: true, wantSudo: true},
			{name: "root", root: true},
			{name: "elevated root", elevated: true, root: true},
		} {
			t.Run(string(action)+"/"+tc.name, func(t *testing.T) {
				wantArgs := []string{systemctl, string(action), "--no-ask-password", "--", unit}
				if tc.wantSudo {
					wantArgs = append([]string{sudo, "-n", "-k", "-u", "root", "--"}, wantArgs...)
				}
				cmd, usedSudo := serviceCommand(context.Background(), systemctl, action, unit, tc.elevated, tc.root)
				if cmd.Path != wantArgs[0] || !reflect.DeepEqual(cmd.Args, wantArgs) || usedSudo != tc.wantSudo {
					t.Fatalf("command = %q %q (sudo %v), want %q (sudo %v)", cmd.Path, cmd.Args, usedSudo, wantArgs, tc.wantSudo)
				}
			})
		}
	}
}

func TestPluginLogsUnavailable(t *testing.T) {
	backend := newPluginSystemd("/usr/bin/systemctl", "")
	_, err := backend.Logs(context.Background(), "caddy.service", 200)
	var serviceErr *Error
	if !errors.As(err, &serviceErr) || serviceErr.Code != "logs_unavailable" {
		t.Fatalf("Logs() error = %v, want logs_unavailable", err)
	}
}

func TestOpenPluginNoneBackend(t *testing.T) {
	p := probe{
		systemdDir: func() bool { return false },
		lookPath:   func(string) (string, error) { return "", exec.ErrNotFound },
	}
	backend, err := openPlugin("auto", p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.List(context.Background())
	var serviceErr *Error
	if !errors.As(err, &serviceErr) || serviceErr.Code != "operation_failed" || serviceErr.Message != "没有可用的服务后端" {
		t.Fatalf("List() error = %v, want operation_failed", err)
	}
	serviceErr = nil
	_, err = backend.Detail(context.Background(), "mihomo.service")
	if !errors.As(err, &serviceErr) || serviceErr.Code != "operation_failed" || serviceErr.Message != "没有可用的服务后端" {
		t.Fatalf("Detail() error = %v, want operation_failed", err)
	}
	serviceErr = nil
	err = backend.Restart(context.Background(), "mihomo.service", true)
	if !errors.As(err, &serviceErr) || serviceErr.Code != "operation_failed" || serviceErr.Message != "没有可用的服务后端" {
		t.Fatalf("Restart() error = %v, want operation_failed", err)
	}
}

func TestOpenPluginDetectsSystemd(t *testing.T) {
	p := probe{
		systemdDir: func() bool { return true },
		lookPath: func(name string) (string, error) {
			switch name {
			case "systemctl":
				return "/usr/bin/systemctl", nil
			case "journalctl":
				return "/usr/bin/journalctl", nil
			default:
				return "", exec.ErrNotFound
			}
		},
	}
	backend, err := openPlugin("auto", p)
	if err != nil {
		t.Fatal(err)
	}
	systemd, ok := backend.(*pluginSystemdBackend)
	if !ok {
		t.Fatalf("backend = %T, want *pluginSystemdBackend", backend)
	}
	if systemd.runtime.systemctl != "/usr/bin/systemctl" || systemd.runtime.journalctl != "/usr/bin/journalctl" {
		t.Errorf("unexpected runtime: %+v", systemd.runtime)
	}
}

func TestOpenPluginForcedSystemdMissing(t *testing.T) {
	p := probe{lookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	if _, err := openPlugin("systemd", p); err == nil {
		t.Fatal("expected an error")
	}
}
