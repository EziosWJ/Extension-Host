package services

import (
	"errors"
	"os"
	"testing"
)

func TestDenied(t *testing.T) {
	if !Denied(errors.New("exit status 1"), "sudo: a password is required") {
		t.Error("sudo diagnostic was not recognized")
	}
	if !Denied(&os.PathError{Op: "fork/exec", Path: "/usr/bin/sudo", Err: os.ErrPermission}, "") {
		t.Error("missing sudo binary was not recognized")
	}
	if Denied(errors.New("exit status 1"), "Failed to restart mihomo.service: Access denied") {
		t.Error("systemctl diagnostics must not be treated as sudo diagnostics")
	}
}
