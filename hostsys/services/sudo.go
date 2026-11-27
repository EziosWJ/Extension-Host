package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// sudoCommand builds a non-interactive root command.
func sudoCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "-k", "-u", "root", "--", path}, args...)...)
}

// sudoProbe reports whether the current account may use sudo
// non-interactively.
func sudoProbe(ctx context.Context) error {
	if os.Geteuid() == 0 {
		return nil
	}
	var stderr bytes.Buffer
	cmd := sudoCommand(ctx, "/usr/bin/true")
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

// Denied distinguishes sudo's own diagnostics from output of the program it ran.
func Denied(err error, stderr string) bool {
	var startError *os.PathError
	if errors.As(err, &startError) && startError.Path == "/usr/bin/sudo" {
		return true
	}
	for line := range strings.Lines(stderr) {
		if strings.HasPrefix(line, "sudo:") {
			return true
		}
	}
	return false
}
