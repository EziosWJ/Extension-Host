package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStreamLogsLines(t *testing.T) {
	journal := serviceTestScript(t, `
if [ "$*" != "-u demo.service -f -n 2 --no-pager --output=short-iso" ]; then
	printf 'Unexpected arguments: %s\n' "$*" >&2
	exit 1
fi
printf 'initial entry\n%70000s\n' x
sleep 0.05
printf 'live entry\nlast line without newline'
`)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var lines []string
	err := newSystemd("", journal).StreamLogs(ctx, "demo.service", 2, func(line string) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"initial entry", strings.Repeat(" ", 69999) + "x", "live entry", "last line without newline"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("expected four intact lines including the 70000-byte line, got %d lines", len(lines))
	}
}

func TestStreamLogsStopsAndReaps(t *testing.T) {
	journal := serviceTestScript(t, "printf '%s\\n' \"$$\"\nexec sleep 30\n")
	backend := newSystemd("", journal)
	emitError := errors.New("writer stopped")
	for _, tc := range []struct {
		name string
		want error
	}{
		{"context canceled", context.Canceled},
		{"emit error", emitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			pid := make(chan int, 1)
			done := make(chan error, 1)
			go func() {
				done <- backend.StreamLogs(ctx, "demo.service", 0, func(line string) error {
					child, err := strconv.Atoi(line)
					if err != nil {
						return err
					}
					pid <- child
					if tc.want == emitError {
						return emitError
					}
					return nil
				})
			}()
			var child int
			select {
			case child = <-pid:
			case <-ctx.Done():
				t.Fatal("journalctl never reported its PID")
			}
			if tc.want == context.Canceled {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Errorf("StreamLogs() = %v, want %v", err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("StreamLogs did not finish")
			}
			process, err := os.FindProcess(child)
			if err == nil {
				defer process.Release()
				err = process.Signal(syscall.Signal(0))
			}
			if !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("journalctl PID %d still present after StreamLogs returned: %v", child, err)
			}
		})
	}
}

func TestStreamLogsReapsWhileEmitIsBlocked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux procfs")
	}
	journal := serviceTestScript(t, "printf '%s\\n' \"$$\"\nexec sleep 30\n")
	ctx, cancel := context.WithCancel(t.Context())
	pid := make(chan int, 1)
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	done := make(chan struct{})
	var streamErr error
	go func() {
		defer close(done)
		streamErr = newSystemd("", journal).StreamLogs(ctx, "demo.service", 0, func(line string) error {
			child, err := strconv.Atoi(line)
			if err != nil {
				return err
			}
			pid <- child
			<-blocked
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("StreamLogs did not finish after releasing emit")
		}
	})

	var child int
	select {
	case child = <-pid:
	case <-done:
		t.Fatalf("StreamLogs returned before emit blocked: %v", streamErr)
	case <-time.After(5 * time.Second):
		t.Fatal("journalctl never reported its PID")
	}
	procStat := filepath.Join("/proc", strconv.Itoa(child), "stat")
	if _, err := os.Stat(procStat); err != nil {
		t.Fatalf("journalctl PID %d is missing before cancellation: %v", child, err)
	}
	cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		_, err := os.Stat(procStat)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatalf("checking journalctl PID %d: %v", child, err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("journalctl PID %d is still present while emit is blocked", child)
		}
	}
	select {
	case <-done:
		t.Fatal("StreamLogs returned while emit was still blocked")
	default:
	}
	release()
	select {
	case <-done:
		if !errors.Is(streamErr, context.Canceled) {
			t.Errorf("StreamLogs() = %v, want context.Canceled", streamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamLogs did not finish after releasing emit")
	}
}

func TestStreamLogsErrorCodes(t *testing.T) {
	emit := func(string) error { return nil }
	ctx := t.Context()
	backend := newSystemd("", "")
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "*.service", 0, emit), "invalid_unit")
	for _, n := range []int{-1, MaxLogLines + 1} {
		assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", n, emit), "invalid_lines")
	}
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", 0, nil), "invalid_emit")
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", MaxLogLines, emit), "logs_unavailable")
	backend = newSystemd("", filepath.Join(t.TempDir(), "missing-journalctl"))
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", 0, emit), "logs_unavailable")
	backend = newSystemd("", serviceTestScript(t, "printf 'No journal files were opened due to insufficient permissions.\\n' >&2\nexit 1\n"))
	assertServiceErrorCode(t, backend.StreamLogs(ctx, "demo.service", 0, emit), "permission")
}
