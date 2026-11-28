package services

import (
	"context"
	"os/exec"

	"github.com/EziosWJ/Extension-Host/internal/privileges"
)

// sudoCommand builds a non-interactive root command.
func sudoCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return privileges.Command(ctx, path, args...)
}

// sudoProbe reports whether the current account may use sudo
// non-interactively.
func sudoProbe(ctx context.Context) error {
	return privileges.Probe(ctx)
}

// Denied distinguishes sudo's own diagnostics from output of the program it ran.
func Denied(err error, stderr string) bool {
	return privileges.Denied(err, stderr)
}
