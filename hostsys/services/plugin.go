package services

import (
	"context"
	"os"
	"os/exec"
)

// PluginBackend is the service surface exposed to plugins. Start, Stop and
// Restart run as the host account unless elevated is true and the host is not root.
type PluginBackend interface {
	List(ctx context.Context) ([]Service, error)
	Detail(ctx context.Context, unit string) (ServiceDetail, error)
	Start(ctx context.Context, unit string, elevated bool) error
	Stop(ctx context.Context, unit string, elevated bool) error
	Restart(ctx context.Context, unit string, elevated bool) error
	Logs(ctx context.Context, unit string, lines int) ([]string, error)
}

type pluginSystemdBackend struct {
	runtime systemdRuntime
}

func newPluginSystemd(systemctl, journalctl string) PluginBackend {
	return &pluginSystemdBackend{runtime: systemdRuntime{systemctl: systemctl, journalctl: journalctl}}
}

func (b *pluginSystemdBackend) List(ctx context.Context) ([]Service, error) {
	return b.runtime.list(ctx, b.runtime.capabilities())
}

func (b *pluginSystemdBackend) Detail(ctx context.Context, unit string) (ServiceDetail, error) {
	return b.runtime.detail(ctx, unit)
}

func (b *pluginSystemdBackend) Start(ctx context.Context, unit string, elevated bool) error {
	cmd, usedSudo := serviceCommand(ctx, b.runtime.systemctl, ActionStart, unit, elevated, os.Geteuid() == 0)
	_, err := runCommand(ctx, cmd, usedSudo)
	return err
}

func (b *pluginSystemdBackend) Stop(ctx context.Context, unit string, elevated bool) error {
	cmd, usedSudo := serviceCommand(ctx, b.runtime.systemctl, ActionStop, unit, elevated, os.Geteuid() == 0)
	_, err := runCommand(ctx, cmd, usedSudo)
	return err
}

func (b *pluginSystemdBackend) Restart(ctx context.Context, unit string, elevated bool) error {
	cmd, usedSudo := restartCommand(ctx, b.runtime.systemctl, unit, elevated, os.Geteuid() == 0)
	_, err := runCommand(ctx, cmd, usedSudo)
	return err
}

func (b *pluginSystemdBackend) Logs(ctx context.Context, unit string, lines int) ([]string, error) {
	return b.runtime.logs(ctx, unit, lines)
}

func restartCommand(ctx context.Context, systemctl, unit string, elevated, root bool) (*exec.Cmd, bool) {
	return serviceCommand(ctx, systemctl, ActionRestart, unit, elevated, root)
}

// serviceCommand builds the plugin service command without executing it. When
// elevation is requested and the process is not root, the same systemctl
// invocation runs through non-interactive sudo. No sudo probe runs first, so a
// restricted sudoers rule can allow individual actions and units.
func serviceCommand(ctx context.Context, systemctl string, action Action, unit string, elevated, root bool) (*exec.Cmd, bool) {
	args := []string{string(action), "--no-ask-password", "--", unit}
	if elevated && !root {
		return sudoCommand(ctx, systemctl, args...), true
	}
	return exec.CommandContext(ctx, systemctl, args...), false
}

type nonePluginBackend struct{}

func nonePlugin() PluginBackend { return nonePluginBackend{} }

func (nonePluginBackend) List(context.Context) ([]Service, error) { return nil, unavailable() }

func (nonePluginBackend) Detail(context.Context, string) (ServiceDetail, error) {
	return ServiceDetail{}, unavailable()
}

func (nonePluginBackend) Start(context.Context, string, bool) error   { return unavailable() }
func (nonePluginBackend) Stop(context.Context, string, bool) error    { return unavailable() }
func (nonePluginBackend) Restart(context.Context, string, bool) error { return unavailable() }
func (nonePluginBackend) Logs(context.Context, string, int) ([]string, error) {
	return nil, unavailable()
}
