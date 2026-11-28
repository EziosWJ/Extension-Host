package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func (b *systemdBackend) StreamLogs(ctx context.Context, unit string, initialLines int, emit func(line string) error) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	if initialLines < 0 || initialLines > MaxLogLines {
		return &Error{Code: "invalid_lines", Message: fmt.Sprintf("initialLines 必须是 0 到 %d 之间的整数", MaxLogLines)}
	}
	if emit == nil {
		return &Error{Code: "invalid_emit", Message: "日志回调不能为空"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.runtime.journalctl == "" {
		return logsUnavailable("journalctl 不可用")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(streamCtx, b.runtime.journalctl,
		"-u", unit, "-f", "-n", strconv.Itoa(initialLines), "--no-pager", "--output=short-iso")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second // Bound the wait for inherited stderr to close.
	// Own stdout so Wait can reap the process without closing the reader.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return commandError(err, "")
	}
	cmd.Stdout = stdoutWriter
	err = cmd.Start()
	_ = stdoutWriter.Close()
	if err != nil {
		_ = stdout.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return journalStreamError(err, "")
	}

	// Cancellation must close stdout even if Wait has already reaped the
	// process and a descendant still holds the write end.
	stdoutClosed := make(chan struct{})
	stopClosingStdout := context.AfterFunc(ctx, func() {
		_ = stdout.Close()
		close(stdoutClosed)
	})

	reaped := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(reaped)
	}()
	defer func() {
		cancel()
		if stopClosingStdout() {
			_ = stdout.Close()
		} else {
			<-stdoutClosed
		}
		<-reaped
	}()

	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if err := ctx.Err(); err != nil {
			return err
		}
		if line != "" {
			if err := emit(strings.TrimSuffix(line, "\n")); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return commandError(readErr, "")
		}
	}

	<-reaped
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if waitErr != nil {
		return journalStreamError(waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func journalStreamError(err error, message string) *Error {
	lower := strings.ToLower(message)
	switch {
	case errors.Is(err, os.ErrPermission), strings.Contains(lower, "insufficient permissions"):
		if message == "" {
			message = err.Error()
		}
		return &Error{Code: "permission", Message: message}
	case errors.Is(err, os.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		return logsUnavailable(err.Error())
	case strings.Contains(lower, "no journal files were found"):
		return logsUnavailable(message)
	default:
		return commandError(err, message)
	}
}

func logsUnavailable(message string) *Error {
	return &Error{Code: "logs_unavailable", Message: message}
}
