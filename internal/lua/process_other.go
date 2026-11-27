//go:build !linux

package lua

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

// CommandContext provides direct child cancellation on other platforms.
func configureExecCancellation(*exec.Cmd) {}

func signalProcessGroup(pid int, force bool) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if force {
		err = proc.Kill()
	} else {
		err = proc.Signal(os.Interrupt)
	}
	if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return err
}
