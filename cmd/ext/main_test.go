package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-version"}} {
		if err := run(t.Context(), args); err != nil {
			t.Fatalf("ext %v: %v", args, err)
		}
	}
	output := versionOutput()
	if !strings.HasPrefix(output, "ext v0.1.0") {
		t.Fatalf("version = %q, want an ext v0.1.0 prefix", output)
	}
}

func TestParseFlagsAfterPositional(t *testing.T) {
	listen := ""
	dataDir := ""
	positional, err := parseFlags([]string{
		"./plugin",
		"--listen", "127.0.0.1:7000",
		"--data-dir=./data",
	}, []stringFlag{
		{names: []string{"listen"}, value: &listen},
		{names: []string{"data-dir"}, value: &dataDir},
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if len(positional) != 1 || positional[0] != "./plugin" {
		t.Fatalf("positional = %#v", positional)
	}
	if listen != "127.0.0.1:7000" || dataDir != "./data" {
		t.Fatalf("flags = %q, %q", listen, dataDir)
	}

	if _, err := parseFlags([]string{"--unknown"}, nil); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := parseFlags([]string{"--listen"}, []stringFlag{{names: []string{"listen"}, value: &listen}}); err == nil {
		t.Fatal("missing flag value accepted")
	}
}

func TestPackAndValidate(t *testing.T) {
	source := filepath.Join("..", "..", "examples", "hello")
	output := filepath.Join(t.TempDir(), "hello.zip")
	if err := pack([]string{source, "-o", output}); err != nil {
		t.Fatalf("pack: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("packed file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("packed file is empty")
	}
	pkg, err := loadBundle(output)
	if err != nil {
		t.Fatalf("load packed ZIP: %v", err)
	}
	if pkg.Manifest.ID != "com.example.hello" {
		t.Fatalf("id = %q", pkg.Manifest.ID)
	}
	if err := validate([]string{output}); err != nil {
		t.Fatalf("validate ZIP: %v", err)
	}
	if err := validate([]string{source}); err != nil {
		t.Fatalf("validate directory: %v", err)
	}

	if err := validate([]string{t.TempDir()}); err == nil {
		t.Fatal("validate accepted a directory without a manifest")
	}
	if err := pack([]string{output}); err == nil {
		t.Fatal("pack accepted a ZIP as its source")
	}
}

func TestRunCommandFromDirectoryAndZip(t *testing.T) {
	source := filepath.Join("..", "..", "examples", "hello")
	archive := filepath.Join(t.TempDir(), "hello.zip")
	if err := pack([]string{source, "-o", archive}); err != nil {
		t.Fatalf("pack: %v", err)
	}

	for _, input := range []string{source, archive} {
		t.Run(filepath.Base(input), func(t *testing.T) {
			dataDir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				result <- runPlugin(ctx, []string{input, "--data-dir", dataDir, "--listen", "127.0.0.1:0"})
			}()

			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dataDir, "plugins", "com.example.hello", "business.db")); err == nil {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("runPlugin exited early: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				if time.Now().After(deadline) {
					cancel()
					t.Fatal("plugin did not start in time")
				}
			}
			cancel()
			select {
			case err := <-result:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("runPlugin: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("runPlugin did not stop")
			}
		})
	}
}

func TestValidateStrictCommand(t *testing.T) {
	dir := t.TempDir()
	const manifestTemplate = `{
	  "schemaVersion":1, "id":"PLUGIN_ID", "name":"Test", "version":"0.1.0",
	  "hostApi":{"min":1,"max":1}, "entry":{"web":"web/index.html"},
	  "description":"An example for testing strict validation.", "author":{"name":"Test Author"}, "license":"MIT",
	  "icon":"assets/icon.svg"
	}`
	for _, name := range []string{"web", "assets"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeDevTestFile(t, filepath.Join(dir, "web/index.html"), "<h1>Test</h1>")
	writeDevTestFile(t, filepath.Join(dir, "assets/icon.svg"), "<svg/>")
	writeManifest := func(id string) {
		t.Helper()
		writeDevTestFile(t, filepath.Join(dir, "manifest.json"), strings.Replace(manifestTemplate, "PLUGIN_ID", id, 1))
	}
	writeManifest("de.jianwang.test")
	archive := filepath.Join(t.TempDir(), "plugin.zip")
	if err := run(t.Context(), []string{"pack", dir, "-o", archive}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, archive} {
		for _, args := range [][]string{{"validate", "--strict", path}, {"validate", path, "--strict"}} {
			if err := run(t.Context(), args); err != nil {
				t.Fatalf("ext %q: %v", args, err)
			}
		}
	}

	writeManifest("com.example.test")
	if err := run(t.Context(), []string{"validate", dir}); err != nil {
		t.Fatalf("ordinary validation of example: %v", err)
	}
	for _, args := range [][]string{{"validate", "--strict", dir}, {"validate", dir, "--strict"}} {
		if err := run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "id") {
			t.Fatalf("ext %q: got %v, want strict ID error", args, err)
		}
	}
}
