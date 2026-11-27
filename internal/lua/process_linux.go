package lua

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func configureExecCancellation(cmd *exec.Cmd) {
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalProcessGroup(cmd.Process.Pid, true)
	}
}

func signalProcessGroup(pid int, force bool) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
