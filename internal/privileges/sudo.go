// Package privileges provides the single implementation of privileged
// command execution and the managed sudoers rule used by Extension Host.
package privileges

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var (
	sudo    = "/usr/bin/sudo"
	trueBin = "/usr/bin/true"
	geteuid = os.Geteuid
)

// Command builds a command that runs with root privileges. When the host
// already runs as root the command is executed directly; otherwise it is
// wrapped in a non-interactive sudo invocation.
func Command(ctx context.Context, path string, args ...string) *exec.Cmd {
	if geteuid() == 0 {
		return exec.CommandContext(ctx, path, args...)
	}
	return exec.CommandContext(ctx, sudo, append([]string{"-n", "-k", "-u", "root", "--", path}, args...)...)
}

// Probe reports whether the current account may run root commands without a
// password prompt.
func Probe(ctx context.Context) error {
	if geteuid() == 0 {
		return nil
	}
	var stderr bytes.Buffer
	cmd := Command(ctx, trueBin)
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

// Denied distinguishes sudo's own diagnostics from output produced by the
// program it ran.
func Denied(err error, stderr string) bool {
	var pathError *os.PathError
	if errors.As(err, &pathError) && pathError.Path == sudo {
		return true
	}
	for line := range strings.Lines(stderr) {
		if strings.HasPrefix(line, "sudo:") {
			return true
		}
	}
	return false
}
