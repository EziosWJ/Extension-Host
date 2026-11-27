package services

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type systemdRuntime struct {
	systemctl  string
	journalctl string
}

type systemdBackend struct {
	runtime   systemdRuntime
	sudoProbe func(context.Context) error
}

func newSystemd(systemctl, journalctl string) Backend {
	return &systemdBackend{
		runtime:   systemdRuntime{systemctl: systemctl, journalctl: journalctl},
		sudoProbe: sudoProbe,
	}
}

func (b *systemdBackend) Name() string { return "systemd" }

func (b *systemdBackend) Capabilities() Capabilities {
	capabilities := b.runtime.capabilities()
	capabilities.Reload = true
	capabilities.DaemonReload = true
	return capabilities
}

func (r *systemdRuntime) capabilities() Capabilities {
	return Capabilities{
		Start:         true,
		Stop:          true,
		Restart:       true,
		Enable:        true,
		Disable:       true,
		Logs:          r.journalctl != "",
		StateReliable: true,
	}
}

func (b *systemdBackend) List(ctx context.Context) ([]Service, error) {
	return b.runtime.list(ctx, b.Capabilities())
}

func (b *systemdBackend) Start(ctx context.Context, unit string) error {
	return b.action(ctx, ActionStart, unit)
}

func (b *systemdBackend) Stop(ctx context.Context, unit string) error {
	return b.action(ctx, ActionStop, unit)
}

func (b *systemdBackend) Restart(ctx context.Context, unit string) error {
	return b.action(ctx, ActionRestart, unit)
}

func (b *systemdBackend) Reload(ctx context.Context, unit string) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	return reloadError(b.action(ctx, ActionReload, unit))
}

func (b *systemdBackend) DaemonReload(ctx context.Context) error {
	return reloadError(b.runAction(ctx, "daemon-reload", "--no-ask-password"))
}

func (b *systemdBackend) Enable(ctx context.Context, unit string) error {
	return b.action(ctx, ActionEnable, unit)
}

func (b *systemdBackend) Disable(ctx context.Context, unit string) error {
	return b.action(ctx, ActionDisable, unit)
}

func (b *systemdBackend) action(ctx context.Context, action Action, unit string) error {
	return b.runAction(ctx, string(action), "--no-ask-password", "--", unit)
}

func (b *systemdBackend) runAction(ctx context.Context, args ...string) error {
	cmd, usedSudo := nativeSystemctlCommand(ctx, b.runtime.systemctl, os.Geteuid() == 0, b.sudoProbe, args...)
	_, err := runCommand(ctx, cmd, usedSudo)
	return err
}

func nativeSystemctlCommand(ctx context.Context, path string, root bool, probe func(context.Context) error, args ...string) (*exec.Cmd, bool) {
	if !root && probe(ctx) == nil {
		return sudoCommand(ctx, path, args...), true
	}
	return exec.CommandContext(ctx, path, args...), false
}

func reloadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "timeout", Message: err.Error()}
	}
	return err
}

func (b *systemdBackend) Detail(ctx context.Context, unit string) (ServiceDetail, error) {
	return b.runtime.detail(ctx, unit)
}

func (b *systemdBackend) Logs(ctx context.Context, unit string, lines int) ([]string, error) {
	return b.runtime.logs(ctx, unit, lines)
}

func (r *systemdRuntime) list(ctx context.Context, capabilities Capabilities) ([]Service, error) {
	files, err := r.run(ctx, r.systemctl, "list-unit-files", "--type=service", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		return nil, err
	}
	units, err := r.run(ctx, r.systemctl, "list-units", "--type=service", "--all", "--full", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		return nil, err
	}
	list := buildServices(files, units, capabilities)
	if !capabilities.Reload {
		return list, nil
	}
	args := []string{"show", "--no-pager", "-p", "Id,Names,CanReload", "--"}
	firstUnit := len(args)
	for _, service := range list {
		if service.State == StateRunning && ValidUnit(service.Name) {
			args = append(args, service.Name)
		}
	}
	if len(args) == firstUnit {
		return list, nil
	}
	output, err := r.run(ctx, r.systemctl, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// Preserve the listing if the optional action metadata is unavailable.
		return list, nil
	}
	reloadable := parseReloadableUnits(output)
	for i := range list {
		service := &list[i]
		if service.State == StateRunning && reloadable[service.Name] {
			service.Actions = actionsFor(service.State, service.BootState, capabilities)
		}
	}
	return list, nil
}

func (r *systemdRuntime) detail(ctx context.Context, unit string) (ServiceDetail, error) {
	output, err := r.run(ctx, r.systemctl, "show", "--no-pager",
		"-p", "LoadState,ActiveState,SubState,UnitFileState,MainPID,MemoryCurrent,CPUUsageNSec,ActiveEnterTimestamp,FragmentPath",
		"--", unit)
	if err != nil {
		return ServiceDetail{}, err
	}
	detail, ok := buildDetail(unit, parseShow(output))
	if !ok {
		return ServiceDetail{}, &Error{Code: "not_found", Message: "服务不存在"}
	}
	return detail, nil
}

func (r *systemdRuntime) logs(ctx context.Context, unit string, lines int) ([]string, error) {
	if r.journalctl == "" {
		return nil, logsUnavailable("journalctl 不可用")
	}
	cmd := exec.CommandContext(ctx, r.journalctl,
		"-u", unit, "-n", strconv.Itoa(lines), "--no-pager", "--quiet", "-o", "short-iso")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if strings.Contains(strings.ToLower(message), "no journal files were found") {
			return []string{}, nil
		}
		return nil, commandError(err, message)
	}
	return splitLines(stdout.String()), nil
}

func (r *systemdRuntime) run(ctx context.Context, path string, args ...string) (string, error) {
	return runCommand(ctx, exec.CommandContext(ctx, path, args...), false)
}

func runCommand(ctx context.Context, cmd *exec.Cmd, elevated bool) (string, error) {
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if elevated && Denied(err, message) {
			if message == "" {
				message = err.Error()
			}
			return "", &Error{Code: "permission", Message: message}
		}
		return "", commandError(err, message)
	}
	return stdout.String(), nil
}

func commandError(err error, message string) *Error {
	if message == "" {
		message = err.Error()
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "access denied"),
		strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "interactive authentication required"),
		strings.Contains(lower, "authentication is required"),
		strings.Contains(lower, "polkit"):
		return &Error{Code: "permission", Message: message}
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "not loaded"),
		strings.Contains(lower, "does not exist"):
		return &Error{Code: "not_found", Message: message}
	default:
		return &Error{Code: "operation_failed", Message: message}
	}
}

func splitLines(output string) []string {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return []string{}
	}
	return strings.Split(output, "\n")
}
