package services

import (
	"context"
	"os"
	"os/exec"
)

// PluginBackend is the service surface exposed to plugins. Restart runs as the
// host account unless elevated is true and the host is not already root.
type PluginBackend interface {
	List(ctx context.Context) ([]Service, error)
	Detail(ctx context.Context, unit string) (ServiceDetail, error)
	Restart(ctx context.Context, unit string, elevated bool) error
}

type pluginSystemdBackend struct {
	runtime systemdRuntime
}

func newPluginSystemd(systemctl, journalctl string) PluginBackend {
	return &pluginSystemdBackend{runtime: systemdRuntime{systemctl: systemctl, journalctl: journalctl}}
}

func (b *pluginSystemdBackend) List(ctx context.Context) ([]Service, error) {
	return b.runtime.list(ctx)
}

func (b *pluginSystemdBackend) Detail(ctx context.Context, unit string) (ServiceDetail, error) {
	return b.runtime.detail(ctx, unit)
}

func (b *pluginSystemdBackend) Restart(ctx context.Context, unit string, elevated bool) error {
	cmd, usedSudo := restartCommand(ctx, b.runtime.systemctl, unit, elevated, os.Geteuid() == 0)
	_, err := runCommand(ctx, cmd, usedSudo)
	return err
}

// restartCommand builds the plugin restart command without executing it. When
// elevation is requested and the process is not root, the same systemctl
// invocation runs through non-interactive sudo. No sudo probe runs first, so a
// restricted sudoers rule that allows only this unit restart still works.
func restartCommand(ctx context.Context, systemctl, unit string, elevated, root bool) (*exec.Cmd, bool) {
	args := []string{"restart", "--no-ask-password", "--", unit}
	if elevated && !root {
		sudoArgs := append([]string{"-n", "-k", "-u", "root", "--", systemctl}, args...)
		return exec.CommandContext(ctx, "/usr/bin/sudo", sudoArgs...), true
	}
	return exec.CommandContext(ctx, systemctl, args...), false
}

type nonePluginBackend struct{}

func nonePlugin() PluginBackend { return nonePluginBackend{} }

func (nonePluginBackend) List(context.Context) ([]Service, error) { return nil, unavailable() }

func (nonePluginBackend) Detail(context.Context, string) (ServiceDetail, error) {
	return ServiceDetail{}, unavailable()
}

func (nonePluginBackend) Restart(context.Context, string, bool) error { return unavailable() }
