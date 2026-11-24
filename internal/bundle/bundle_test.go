package bundle

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifest = `{
	"schemaVersion": 1,
	"id": "com.godesk.test",
	"name": "Test",
	"version": "1.0.0",
	"hostApi": {"min": 1, "max": 1},
	"entry": {"web": "web/index.html", "lua": "lua/init.lua"}
}`

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestLoadZipAndRoundTrip(t *testing.T) {
	archive := makeZip(t, map[string]string{
		"manifest.json":  validManifest,
		"web/index.html": "<html></html>",
		"lua/init.lua":   "print('hi')",
	})
	pkg, err := LoadZip(archive)
	if err != nil {
		t.Fatalf("LoadZip: %v", err)
	}
	again, err := pkg.Zip()
	if err != nil {
		t.Fatalf("Zip: %v", err)
	}
	repacked, err := LoadZip(again)
	if err != nil {
		t.Fatalf("reload repacked ZIP: %v", err)
	}
	if len(repacked.Files) != 3 {
		t.Fatalf("files = %d", len(repacked.Files))
	}
	second, err := pkg.Zip()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, second) {
		t.Fatal("Zip output is not deterministic")
	}
}

func TestLoadZipRejectsInvalidPackages(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			"missing manifest",
			map[string]string{"web/index.html": "x"},
			"manifest.json is missing",
		},
		{
			"missing entry",
			map[string]string{"manifest.json": validManifest, "lua/init.lua": "x"},
			"entry.web",
		},
		{
			"traversal",
			map[string]string{"manifest.json": validManifest, "../evil.lua": "x", "web/index.html": "x", "lua/init.lua": "x"},
			"invalid path",
		},
		{
			"absolute",
			map[string]string{"manifest.json": validManifest, "/etc/passwd": "x", "web/index.html": "x", "lua/init.lua": "x"},
			"invalid path",
		},
		{
			"backslash",
			map[string]string{"manifest.json": validManifest, `web\index.html`: "x", "lua/init.lua": "x"},
			"invalid path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadZip(makeZip(t, test.files))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadDirAndExclude(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", validManifest)
	write("web/index.html", "<html></html>")
	write("lua/init.lua", "print('hi')")
	output := filepath.Join(dir, "dist", "package.zip")
	write("dist/package.zip", "stale output")

	pkg, err := LoadDir(dir, output)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if _, ok := pkg.Files["dist/package.zip"]; ok {
		t.Fatal("excluded output was included")
	}

	if err := os.Symlink(filepath.Join(dir, "web/index.html"), filepath.Join(dir, "link.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir, output); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "link.html")); err != nil {
		t.Fatal(err)
	}

	extracted := t.TempDir()
	if err := pkg.Extract(extracted); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, "lua", "init.lua"))
	if err != nil || string(data) != "print('hi')" {
		t.Fatalf("extracted content = %q, %v", data, err)
	}
}

func TestLoadZipRejectsDuplicatePaths(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for range 2 {
		entry, err := writer.Create("manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(validManifest))
	}
	entry, err := writer.Create("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("x"))
	entry, err = writer.Create("lua/init.lua")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("x"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadZip(buffer.Bytes()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
}
