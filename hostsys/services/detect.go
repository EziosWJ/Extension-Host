package services

import (
	"fmt"
	"os"
	"os/exec"
)

type probe struct {
	systemdDir func() bool
	lookPath   func(string) (string, error)
}

func systemdProbe() probe {
	return probe{
		systemdDir: func() bool {
			info, err := os.Stat("/run/systemd/system")
			return err == nil && info.IsDir()
		},
		lookPath: exec.LookPath,
	}
}

func open(choice string, p probe) (Backend, error) {
	switch choice {
	case "", "auto":
		if p.systemdDir() {
			if systemctl, err := p.lookPath("systemctl"); err == nil {
				journalctl, _ := p.lookPath("journalctl")
				return newSystemd(systemctl, journalctl), nil
			}
		}
		return None(), nil
	case "systemd":
		systemctl, err := p.lookPath("systemctl")
		if err != nil {
			return nil, fmt.Errorf("service backend systemd: %w", err)
		}
		journalctl, _ := p.lookPath("journalctl")
		return newSystemd(systemctl, journalctl), nil
	case "none":
		return None(), nil
	default:
		return nil, fmt.Errorf("invalid -service-backend %q (want auto, systemd, or none)", choice)
	}
}

// Open selects a service backend for GoDesk's native service management. It is
// called once at startup.
func Open(choice string) (Backend, error) {
	return open(choice, systemdProbe())
}

func openPlugin(choice string, p probe) (PluginBackend, error) {
	switch choice {
	case "", "auto":
		if p.systemdDir() {
			if systemctl, err := p.lookPath("systemctl"); err == nil {
				journalctl, _ := p.lookPath("journalctl")
				return newPluginSystemd(systemctl, journalctl), nil
			}
		}
		return nonePlugin(), nil
	case "systemd":
		systemctl, err := p.lookPath("systemctl")
		if err != nil {
			return nil, fmt.Errorf("service backend systemd: %w", err)
		}
		journalctl, _ := p.lookPath("journalctl")
		return newPluginSystemd(systemctl, journalctl), nil
	case "none":
		return nonePlugin(), nil
	default:
		return nil, fmt.Errorf("invalid -service-backend %q (want auto, systemd, or none)", choice)
	}
}

// OpenPlugin selects a service backend for plugins. It shares detection with
// Open, but its actions run as the host account unless the caller asks for
// elevation.
func OpenPlugin(choice string) (PluginBackend, error) {
	return openPlugin(choice, systemdProbe())
}
