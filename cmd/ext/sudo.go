package main

import (
	"context"
	"fmt"
	"io"

	"github.com/EziosWJ/Extension-Host/internal/privileges"
)

// managedSudoersPath is the drop-in rule installed by setup-sudo.
const managedSudoersPath = "/etc/sudoers.d/extension-host"

// Package-level indirections let tests replace privileged operations.
var (
	runSetupSudo  = privileges.Setup
	runCheckSudo  = privileges.Check
	runRemoveSudo = privileges.Remove
)

func setupSudo(ctx context.Context, args []string, out io.Writer) error {
	positional, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("setup-sudo accepts no positional arguments")
	}
	if err := runSetupSudo(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "已安装托管 sudoers 规则：%s\n", managedSudoersPath)
	return nil
}

func checkSudo(ctx context.Context, args []string, out io.Writer) error {
	positional, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("check-sudo accepts no positional arguments")
	}
	return runCheckSudo(ctx, out)
}

func removeSudo(ctx context.Context, args []string, out io.Writer) error {
	positional, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("remove-sudo accepts no positional arguments")
	}
	if err := runRemoveSudo(); err != nil {
		return err
	}
	fmt.Fprintf(out, "已移除托管 sudoers 规则：%s\n", managedSudoersPath)
	return nil
}
