package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
)

// sudoStubs records privileged calls so tests never touch the real system.
type sudoStubs struct {
	setupCalls, checkCalls, removeCalls int
	setupErr, checkErr, removeErr       error
	checkOutput                         string
}

// stubSudoCommands replaces the privileged operations and restores them when
// the test finishes.
func stubSudoCommands(t *testing.T, stubs *sudoStubs) {
	t.Helper()
	originalSetup, originalCheck, originalRemove := runSetupSudo, runCheckSudo, runRemoveSudo
	t.Cleanup(func() {
		runSetupSudo, runCheckSudo, runRemoveSudo = originalSetup, originalCheck, originalRemove
	})
	runSetupSudo = func(context.Context) error {
		stubs.setupCalls++
		return stubs.setupErr
	}
	runCheckSudo = func(_ context.Context, out io.Writer) error {
		stubs.checkCalls++
		if stubs.checkOutput != "" {
			if _, err := fmt.Fprintln(out, stubs.checkOutput); err != nil {
				return err
			}
		}
		return stubs.checkErr
	}
	runRemoveSudo = func() error {
		stubs.removeCalls++
		return stubs.removeErr
	}
}

// sudoCommand couples a subcommand with the stub counter it must trigger.
type sudoCommand struct {
	name     string
	run      func(context.Context, []string, io.Writer) error
	executed func(*sudoStubs) int
}

func sudoCommands() []sudoCommand {
	return []sudoCommand{
		{
			name:     "setup-sudo",
			run:      setupSudo,
			executed: func(stubs *sudoStubs) int { return stubs.setupCalls },
		},
		{
			name:     "check-sudo",
			run:      checkSudo,
			executed: func(stubs *sudoStubs) int { return stubs.checkCalls },
		},
		{
			name:     "remove-sudo",
			run:      removeSudo,
			executed: func(stubs *sudoStubs) int { return stubs.removeCalls },
		},
	}
}

func TestSudoCommandDispatch(t *testing.T) {
	stubs := &sudoStubs{}
	stubSudoCommands(t, stubs)
	for _, command := range sudoCommands() {
		if err := run(t.Context(), []string{command.name}); err != nil {
			t.Fatalf("ext %s: %v", command.name, err)
		}
	}
	for _, command := range sudoCommands() {
		if calls := command.executed(stubs); calls != 1 {
			t.Fatalf("%s ran %d times, want 1", command.name, calls)
		}
	}
}

func TestSudoCommandRejectsArguments(t *testing.T) {
	for _, command := range sudoCommands() {
		t.Run(command.name, func(t *testing.T) {
			stubs := &sudoStubs{}
			stubSudoCommands(t, stubs)
			var out strings.Builder

			if err := command.run(t.Context(), []string{"extra"}, &out); err == nil ||
				!strings.Contains(err.Error(), "accepts no positional arguments") {
				t.Fatalf("positional argument error = %v", err)
			}
			if err := command.run(t.Context(), []string{"--unknown"}, &out); err == nil ||
				!strings.Contains(err.Error(), "unknown flag") {
				t.Fatalf("unknown flag error = %v", err)
			}
			if err := run(t.Context(), []string{command.name, "extra"}); err == nil {
				t.Fatalf("ext %s accepted a positional argument", command.name)
			}
			if calls := command.executed(stubs); calls != 0 {
				t.Fatalf("%s ran %d times, want 0", command.name, calls)
			}
			if got := out.String(); got != "" {
				t.Fatalf("%s output = %q, want nothing", command.name, got)
			}
		})
	}
}

func TestSudoCommandSuccess(t *testing.T) {
	stubs := &sudoStubs{checkOutput: "probe ok"}
	stubSudoCommands(t, stubs)

	setupOutput := new(strings.Builder)
	if err := setupSudo(t.Context(), nil, setupOutput); err != nil {
		t.Fatalf("setupSudo: %v", err)
	}
	if got := setupOutput.String(); !strings.Contains(got, "已安装") || !strings.Contains(got, managedSudoersPath) {
		t.Fatalf("setupSudo output = %q", got)
	}

	checkOutput := new(strings.Builder)
	if err := checkSudo(t.Context(), nil, checkOutput); err != nil {
		t.Fatalf("checkSudo: %v", err)
	}
	if got := checkOutput.String(); got != "probe ok\n" {
		t.Fatalf("checkSudo output = %q, want it to reach out", got)
	}

	removeOutput := new(strings.Builder)
	if err := removeSudo(t.Context(), nil, removeOutput); err != nil {
		t.Fatalf("removeSudo: %v", err)
	}
	if got := removeOutput.String(); !strings.Contains(got, "已移除") || !strings.Contains(got, managedSudoersPath) {
		t.Fatalf("removeSudo output = %q", got)
	}

	if stubs.setupCalls != 1 || stubs.checkCalls != 1 || stubs.removeCalls != 1 {
		t.Fatalf("calls = setup %d, check %d, remove %d", stubs.setupCalls, stubs.checkCalls, stubs.removeCalls)
	}
}

func TestSudoCommandErrors(t *testing.T) {
	sentinel := errors.New("privileged operation failed")
	for _, command := range sudoCommands() {
		t.Run(command.name, func(t *testing.T) {
			stubs := &sudoStubs{setupErr: sentinel, checkErr: sentinel, removeErr: sentinel}
			stubSudoCommands(t, stubs)
			var out strings.Builder
			if err := command.run(t.Context(), nil, &out); !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want the stubbed failure", err)
			}
			if calls := command.executed(stubs); calls != 1 {
				t.Fatalf("%s ran %d times, want 1", command.name, calls)
			}
			if got := out.String(); got != "" {
				t.Fatalf("%s printed a success hint after failure: %q", command.name, got)
			}
		})
	}
}

func TestSudoCommandHelp(t *testing.T) {
	for _, command := range sudoCommands() {
		for _, help := range []string{"-h", "--help"} {
			t.Run(command.name+"/"+help, func(t *testing.T) {
				stubs := &sudoStubs{}
				stubSudoCommands(t, stubs)
				if err := command.run(t.Context(), []string{help}, io.Discard); !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("error = %v, want flag.ErrHelp", err)
				}
				if calls := command.executed(stubs); calls != 0 {
					t.Fatalf("%s ran %d times, want 0", command.name, calls)
				}
			})
		}
	}
}
