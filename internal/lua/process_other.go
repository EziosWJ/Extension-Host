//go:build !linux

package lua

import "os/exec"

// CommandContext provides direct child cancellation on other platforms.
func configureExecCancellation(*exec.Cmd) {}
