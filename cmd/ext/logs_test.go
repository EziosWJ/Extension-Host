package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShowLogsProject(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, ".ext", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDevTestFile(t, filepath.Join(dir, "manifest.json"), `{"id":"com.example.project"}`)
	writeDevTestFile(t, filepath.Join(logDir, "com.example.other-1.0.0.log"), "wrong plugin\n")
	writeDevTestFile(t, filepath.Join(logDir, "com.example.project-1.0.0.log"), "first\nsecond\nthird\n")

	var out strings.Builder
	if err := showLogs(t.Context(), []string{"-n", "2"}, dir, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "second\nthird\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestShowLogsSelectsLatest(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "custom")
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name    string
		content string
	}{
		{"com.example.plugin-2.0.0.log", "older log\n"},
		{"com.example.plugin-1.0.0.log", "first\nsecond"},
		{"com.example.other-1.0.0.log", "wrong plugin\n"},
	}
	for i, file := range files {
		path := filepath.Join(logDir, file.name)
		writeDevTestFile(t, path, file.content)
		modified := time.Unix(int64(i)+1, 0)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	args := []string{"--data-dir", "custom", "com.example.plugin", "--lines=0"}
	if err := showLogs(t.Context(), args, dir, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "first\nsecond" {
		t.Fatalf("output = %q", got)
	}
}

func TestShowLogsDefaultTail(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "ext-data", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The last line is unterminated and crosses a reverse-read block.
	want := strings.Repeat("line\n", 99) + strings.Repeat("x", 64*1024)
	writeDevTestFile(t, filepath.Join(logDir, "project-1.0.0.log"), "excluded\n"+want)
	var out strings.Builder
	if err := showLogs(t.Context(), nil, dir, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != want {
		t.Fatalf("default tail read %d bytes, want %d", len(got), len(want))
	}
}

func TestShowLogsFollow(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "ext-data", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "project-1.0.0.log")
	writeDevTestFile(t, path, "skipped\ninitial\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out := make(logTestOutput, 16)
	done := make(chan error, 1)
	go func() {
		done <- showLogs(ctx, []string{"-f", "project", "-n=1"}, dir, out)
	}()
	expectOutput := func(want string) {
		t.Helper()
		var got strings.Builder
		for got.Len() < len(want) {
			select {
			case chunk := <-out:
				got.WriteString(chunk)
			case err := <-done:
				t.Fatalf("follow exited while waiting for %q: %v", want, err)
			case <-ctx.Done():
				t.Fatalf("follow produced %q, want %q", got.String(), want)
			}
		}
		if got.String() != want {
			t.Fatalf("follow produced %q, want %q", got.String(), want)
		}
	}
	expectOutput("initial\n")

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("appended\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append log: write %v, close %v", writeErr, closeErr)
	}
	expectOutput("appended\n")

	// Truncate and rewrite past the last byte already read.
	const rewritten = "rewritten after truncation and regrowth\n"
	writeDevTestFile(t, path, rewritten)
	expectOutput(rewritten)

	replacement := filepath.Join(logDir, "replacement")
	const rotated = "first after rotation\nsecond line longer than the old log\n"
	writeDevTestFile(t, replacement, rotated)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	expectOutput(rotated)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("follow did not exit after cancellation")
	}
}

type logTestOutput chan string

func (out logTestOutput) Write(data []byte) (int, error) {
	out <- string(data)
	return len(data), nil
}

func TestShowLogsErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing logs", nil, "no log files"},
		{"missing plugin logs", []string{"missing"}, `no log files found for plugin "missing"`},
		{"negative lines", []string{"--lines", "-1"}, "invalid --lines"},
		{"non-integer lines", []string{"--lines=text"}, "invalid --lines"},
		{"missing lines value", []string{"--lines"}, "requires a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := showLogs(t.Context(), tc.args, dir, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	logDir := filepath.Join(dir, "ext-data", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDevTestFile(t, filepath.Join(logDir, "first-1.0.0.log"), "")
	writeDevTestFile(t, filepath.Join(logDir, "second-1.0.0.log"), "")
	err := showLogs(t.Context(), nil, dir, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "specify plugin-id") {
		t.Fatalf("error = %v, want a prompt to specify plugin-id", err)
	}
}
