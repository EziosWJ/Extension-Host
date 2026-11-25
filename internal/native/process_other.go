//go:build !linux

package native

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func signalGroup(command *exec.Cmd, _ bool) error { return command.Process.Kill() }
