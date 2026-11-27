package lua

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	glua "github.com/yuin/gopher-lua"
)

func newTestFSState(t *testing.T) *glua.LState {
	t.Helper()
	L := glua.NewState()
	t.Cleanup(L.Close)

	r := &Runtime{}
	host := L.NewTable()
	host.RawSetString("fs", r.fsModule(L))
	host.RawSetString("json", r.jsonModule(L))
	L.SetGlobal("host", host)
	return L
}

func TestFSStat(t *testing.T) {
	L := newTestFSState(t)
	dir := t.TempDir()

	filePath := filepath.Join(dir, "sample.txt")
	content := []byte("hello world")
	if err := os.WriteFile(filePath, content, 0o640); err != nil {
		t.Fatalf("write sample file: %v", err)
	}
	if err := os.Chmod(filePath, 0o640); err != nil {
		t.Fatalf("chmod sample file: %v", err)
	}

	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o750); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	linkPath := filepath.Join(dir, "sample.link")
	if err := os.Symlink(filePath, linkPath); err != nil {
		t.Fatalf("symlink file: %v", err)
	}

	brokenLink := filepath.Join(dir, "broken.link")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), brokenLink); err != nil {
		t.Fatalf("symlink broken: %v", err)
	}

	t.Run("regular file", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filePath))
		if err := L.DoString(`info, err = host.fs.stat(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("err") != glua.LNil {
			t.Fatalf("unexpected error: %v", L.GetGlobal("err"))
		}
		info, ok := L.GetGlobal("info").(*glua.LTable)
		if !ok {
			t.Fatalf("info is %T, want table", L.GetGlobal("info"))
		}
		if got := info.RawGetString("name").String(); got != "sample.txt" {
			t.Fatalf("name = %q, want %q", got, "sample.txt")
		}
		if got := info.RawGetString("size"); got != glua.LNumber(len(content)) {
			t.Fatalf("size = %v, want %d", got, len(content))
		}
		if got := info.RawGetString("mode").String(); got != "0640" {
			t.Fatalf("mode = %q, want %q", got, "0640")
		}
		modTimeStr := info.RawGetString("modTime").String()
		if _, err := time.Parse(time.RFC3339, modTimeStr); err != nil {
			t.Fatalf("modTime %q is not RFC3339: %v", modTimeStr, err)
		}
		if !strings.HasSuffix(modTimeStr, "Z") {
			t.Fatalf("modTime %q is not UTC", modTimeStr)
		}
		if info.RawGetString("isDir") != glua.LFalse {
			t.Fatalf("isDir = %v, want false", info.RawGetString("isDir"))
		}
		if info.RawGetString("isFile") != glua.LTrue {
			t.Fatalf("isFile = %v, want true", info.RawGetString("isFile"))
		}
		if info.RawGetString("isSymlink") != glua.LFalse {
			t.Fatalf("isSymlink = %v, want false", info.RawGetString("isSymlink"))
		}
	})

	t.Run("directory", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(subDir))
		if err := L.DoString(`info, err = host.fs.stat(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("err") != glua.LNil {
			t.Fatalf("unexpected error: %v", L.GetGlobal("err"))
		}
		info := L.GetGlobal("info").(*glua.LTable)
		if got := info.RawGetString("name").String(); got != "subdir" {
			t.Fatalf("name = %q, want %q", got, "subdir")
		}
		if info.RawGetString("isDir") != glua.LTrue {
			t.Fatalf("isDir = %v, want true", info.RawGetString("isDir"))
		}
		if info.RawGetString("isFile") != glua.LFalse {
			t.Fatalf("isFile = %v, want false", info.RawGetString("isFile"))
		}
		if info.RawGetString("isSymlink") != glua.LFalse {
			t.Fatalf("isSymlink = %v, want false", info.RawGetString("isSymlink"))
		}
	})

	t.Run("symlink to file", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(linkPath))
		if err := L.DoString(`info, err = host.fs.stat(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("err") != glua.LNil {
			t.Fatalf("unexpected error: %v", L.GetGlobal("err"))
		}
		info := L.GetGlobal("info").(*glua.LTable)
		if got := info.RawGetString("size"); got != glua.LNumber(len(content)) {
			t.Fatalf("size = %v, want %d", got, len(content))
		}
		if info.RawGetString("isFile") != glua.LTrue {
			t.Fatalf("isFile = %v, want true", info.RawGetString("isFile"))
		}
		if info.RawGetString("isDir") != glua.LFalse {
			t.Fatalf("isDir = %v, want false", info.RawGetString("isDir"))
		}
		if info.RawGetString("isSymlink") != glua.LTrue {
			t.Fatalf("isSymlink = %v, want true", info.RawGetString("isSymlink"))
		}
	})

	t.Run("broken symlink returns nil, err", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(brokenLink))
		if err := L.DoString(`info, err = host.fs.stat(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("info") != glua.LNil {
			t.Fatalf("info = %v, want nil", L.GetGlobal("info"))
		}
		if _, ok := L.GetGlobal("err").(glua.LString); !ok {
			t.Fatalf("err = %T, want string", L.GetGlobal("err"))
		}
	})

	t.Run("missing file returns nil, err", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "missing.txt")))
		if err := L.DoString(`info, err = host.fs.stat(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("info") != glua.LNil {
			t.Fatalf("info = %v, want nil", L.GetGlobal("info"))
		}
		if _, ok := L.GetGlobal("err").(glua.LString); !ok {
			t.Fatalf("err = %T, want string", L.GetGlobal("err"))
		}
	})

	t.Run("invalid path raises arg error", func(t *testing.T) {
		cases := []string{
			`host.fs.stat("")`,
			`host.fs.stat("bad\0path")`,
			`host.fs.stat(123)`,
			`host.fs.stat(nil)`,
		}
		for _, script := range cases {
			if err := L.DoString(script); err == nil {
				t.Fatalf("script %q succeeded, want arg error", script)
			}
		}
	})
}

func TestFSListDir(t *testing.T) {
	L := newTestFSState(t)
	dir := t.TempDir()

	t.Run("empty dir encodes as JSON array", func(t *testing.T) {
		emptyDir := filepath.Join(dir, "empty")
		if err := os.Mkdir(emptyDir, 0o755); err != nil {
			t.Fatalf("mkdir empty: %v", err)
		}
		L.SetGlobal("target", glua.LString(emptyDir))
		if err := L.DoString(`entries, err = host.fs.listDir(target); encoded = host.json.encode(entries)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("err") != glua.LNil {
			t.Fatalf("unexpected error: %v", L.GetGlobal("err"))
		}
		converted, err := toGo(L.GetGlobal("entries"))
		if err != nil {
			t.Fatalf("toGo: %v", err)
		}
		raw, err := json.Marshal(converted)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if string(raw) != "[]" {
			t.Fatalf("toGo JSON = %s, want []", raw)
		}
		if got := L.GetGlobal("encoded").String(); got != "[]" {
			t.Fatalf("host.json.encode = %s, want []", got)
		}
	})

	t.Run("populated dir sorted with metadata", func(t *testing.T) {
		populated := filepath.Join(dir, "populated")
		if err := os.Mkdir(populated, 0o755); err != nil {
			t.Fatalf("mkdir populated: %v", err)
		}
		if err := os.WriteFile(filepath.Join(populated, "b-file.txt"), []byte("12345"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		sub := filepath.Join(populated, "a-dir")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("mkdir sub: %v", err)
		}
		if err := os.Symlink(sub, filepath.Join(populated, "c-link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		L.SetGlobal("target", glua.LString(populated))
		if err := L.DoString(`entries, err = host.fs.listDir(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("err") != glua.LNil {
			t.Fatalf("unexpected error: %v", L.GetGlobal("err"))
		}
		entries, ok := L.GetGlobal("entries").(*glua.LTable)
		if !ok {
			t.Fatalf("entries is %T, want table", L.GetGlobal("entries"))
		}
		if entries.Len() != 3 {
			t.Fatalf("entries len = %d, want 3", entries.Len())
		}

		// Entry 1: a-dir
		e1 := entries.RawGetInt(1).(*glua.LTable)
		if e1.RawGetString("name").String() != "a-dir" ||
			e1.RawGetString("isDir") != glua.LTrue ||
			e1.RawGetString("isFile") != glua.LFalse ||
			e1.RawGetString("isSymlink") != glua.LFalse {
			t.Fatalf("unexpected dir entry: %v", e1)
		}

		// Entry 2: b-file.txt
		e2 := entries.RawGetInt(2).(*glua.LTable)
		if e2.RawGetString("name").String() != "b-file.txt" ||
			e2.RawGetString("isDir") != glua.LFalse ||
			e2.RawGetString("isFile") != glua.LTrue ||
			e2.RawGetString("isSymlink") != glua.LFalse ||
			e2.RawGetString("size") != glua.LNumber(5) {
			t.Fatalf("unexpected file entry: %v", e2)
		}
		if _, err := time.Parse(time.RFC3339, e2.RawGetString("modTime").String()); err != nil {
			t.Fatalf("invalid modTime: %v", err)
		}

		// Entry 3: c-link (symlink to dir -> DirEntry reports isSymlink=true, isDir=false, isFile=false)
		e3 := entries.RawGetInt(3).(*glua.LTable)
		if e3.RawGetString("name").String() != "c-link" ||
			e3.RawGetString("isDir") != glua.LFalse ||
			e3.RawGetString("isFile") != glua.LFalse ||
			e3.RawGetString("isSymlink") != glua.LTrue {
			t.Fatalf("unexpected symlink entry: %v", e3)
		}
	})

	t.Run("missing dir returns nil, err", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "missing")))
		if err := L.DoString(`entries, err = host.fs.listDir(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("entries") != glua.LNil {
			t.Fatalf("entries = %v, want nil", L.GetGlobal("entries"))
		}
		if _, ok := L.GetGlobal("err").(glua.LString); !ok {
			t.Fatalf("err = %T, want string", L.GetGlobal("err"))
		}
	})
}

func TestFSMkdirAll(t *testing.T) {
	L := newTestFSState(t)
	dir := t.TempDir()

	t.Run("nested dirs default mode", func(t *testing.T) {
		nested := filepath.Join(dir, "a", "b", "c")
		L.SetGlobal("target", glua.LString(nested))
		if err := L.DoString(`ok, err = host.fs.mkdirAll(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		info, err := os.Stat(nested)
		if err != nil || !info.IsDir() {
			t.Fatalf("stat nested dir: info=%v err=%v", info, err)
		}
	})

	t.Run("octal string mode", func(t *testing.T) {
		secretDir := filepath.Join(dir, "secret", "nested")
		L.SetGlobal("target", glua.LString(secretDir))
		if err := L.DoString(`ok, err = host.fs.mkdirAll(target, "0700")`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		info, err := os.Stat(secretDir)
		if err != nil {
			t.Fatalf("stat secretDir: %v", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("secretDir perm = %04o, expected group/other bits cleared", info.Mode().Perm())
		}
	})

	t.Run("integer mode", func(t *testing.T) {
		intDir := filepath.Join(dir, "intmode")
		L.SetGlobal("target", glua.LString(intDir))
		// 0700 in decimal is 448.
		if err := L.DoString(`ok, err = host.fs.mkdirAll(target, 448)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue {
			t.Fatalf("ok = %v, want true", L.GetGlobal("ok"))
		}
		info, err := os.Stat(intDir)
		if err != nil || !info.IsDir() {
			t.Fatalf("stat intDir: info=%v err=%v", info, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("intDir perm = %04o, expected group/other bits cleared", info.Mode().Perm())
		}
	})

	t.Run("invalid mode raises arg error", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "bad")))
		cases := []string{
			`host.fs.mkdirAll(target, -1)`,
			`host.fs.mkdirAll(target, 512)`,
			`host.fs.mkdirAll(target, 755.5)`,
			`host.fs.mkdirAll(target, "0888")`,
			`host.fs.mkdirAll(target, "1000")`,
			`host.fs.mkdirAll(target, "")`,
			`host.fs.mkdirAll(target, true)`,
			`host.fs.mkdirAll(target, {})`,
		}
		for _, script := range cases {
			if err := L.DoString(script); err == nil {
				t.Fatalf("script %q succeeded, want arg error", script)
			}
		}
	})
}

func TestFSWriteFile(t *testing.T) {
	L := newTestFSState(t)
	dir := t.TempDir()

	t.Run("atomic default with binary NUL bytes and exact 0600 mode", func(t *testing.T) {
		envPath := filepath.Join(dir, ".env")
		binaryData := "SECRET=1\x00BINARY\xff\n"
		L.SetGlobal("target", glua.LString(envPath))
		L.SetGlobal("payload", glua.LString(binaryData))
		if err := L.DoString(`ok, err = host.fs.writeFile(target, payload, { mode = "0600" })`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		gotBytes, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		if !bytes.Equal(gotBytes, []byte(binaryData)) {
			t.Fatalf("content = %x, want %x", gotBytes, []byte(binaryData))
		}
		info, err := os.Stat(envPath)
		if err != nil {
			t.Fatalf("stat .env: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("perm = %04o, want 0600", info.Mode().Perm())
		}

		// Overwrite existing file and verify no leftover temp files.
		if err := L.DoString(`ok, err = host.fs.writeFile(target, "updated", { mode = 420 })`); err != nil {
			t.Fatalf("DoString overwrite: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue {
			t.Fatalf("overwrite ok = %v, want true", L.GetGlobal("ok"))
		}
		gotBytes, err = os.ReadFile(envPath)
		if err != nil {
			t.Fatalf("read overwritten .env: %v", err)
		}
		if string(gotBytes) != "updated" {
			t.Fatalf("overwritten content = %q, want %q", gotBytes, "updated")
		}
		info, err = os.Stat(envPath)
		if err != nil {
			t.Fatalf("stat overwritten .env: %v", err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("overwritten perm = %04o, want 0644", info.Mode().Perm())
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".host-fs-") {
				t.Fatalf("leftover temp file found: %s", entry.Name())
			}
		}
	})

	t.Run("mkdir option creates parent directories", func(t *testing.T) {
		nestedFile := filepath.Join(dir, "deep", "nested", "config.yml")
		L.SetGlobal("target", glua.LString(nestedFile))
		if err := L.DoString(`ok, err = host.fs.writeFile(target, "k: v\n", { mkdir = true })`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		got, err := os.ReadFile(nestedFile)
		if err != nil {
			t.Fatalf("read nested file: %v", err)
		}
		if string(got) != "k: v\n" {
			t.Fatalf("content = %q, want %q", got, "k: v\n")
		}
	})

	t.Run("atomic false direct write", func(t *testing.T) {
		directFile := filepath.Join(dir, "direct.txt")
		L.SetGlobal("target", glua.LString(directFile))
		if err := L.DoString(`ok, err = host.fs.writeFile(target, "direct-content", { atomic = false, mode = "0640" })`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		got, err := os.ReadFile(directFile)
		if err != nil {
			t.Fatalf("read direct file: %v", err)
		}
		if string(got) != "direct-content" {
			t.Fatalf("content = %q, want %q", got, "direct-content")
		}
		info, err := os.Stat(directFile)
		if err != nil {
			t.Fatalf("stat direct file: %v", err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("perm = %04o, want 0640", info.Mode().Perm())
		}
	})

	t.Run("missing parent dir without mkdir returns nil, err", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "no-such-parent", "file.txt")))
		if err := L.DoString(`ok, err = host.fs.writeFile(target, "data")`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LNil {
			t.Fatalf("ok = %v, want nil", L.GetGlobal("ok"))
		}
		if _, ok := L.GetGlobal("err").(glua.LString); !ok {
			t.Fatalf("err = %T, want string", L.GetGlobal("err"))
		}
	})

	t.Run("atomic error cleans up temp file", func(t *testing.T) {
		existingDir := filepath.Join(dir, "is-a-dir")
		if err := os.Mkdir(existingDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		L.SetGlobal("target", glua.LString(existingDir))
		if err := L.DoString(`ok, err = host.fs.writeFile(target, "data")`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LNil {
			t.Fatalf("ok = %v, want nil", L.GetGlobal("ok"))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".host-fs-") {
				t.Fatalf("leftover temp file after failed rename: %s", entry.Name())
			}
		}
	})

	t.Run("invalid options raise arg error", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "invalid.txt")))
		cases := []string{
			`host.fs.writeFile(target, 123)`,
			`host.fs.writeFile(target, "ok", "not-a-table")`,
			`host.fs.writeFile(target, "ok", { mode = "0999" })`,
			`host.fs.writeFile(target, "ok", { mode = -1 })`,
			`host.fs.writeFile(target, "ok", { atomic = "yes" })`,
			`host.fs.writeFile(target, "ok", { mkdir = 1 })`,
		}
		for _, script := range cases {
			if err := L.DoString(script); err == nil {
				t.Fatalf("script %q succeeded, want arg error", script)
			}
		}
	})
}

func TestFSRemoveAll(t *testing.T) {
	L := newTestFSState(t)
	dir := t.TempDir()

	t.Run("recursive directory tree", func(t *testing.T) {
		tree := filepath.Join(dir, "tree")
		nested := filepath.Join(tree, "sub", "leaf")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir tree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(nested, "data.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write leaf file: %v", err)
		}

		L.SetGlobal("target", glua.LString(tree))
		if err := L.DoString(`ok, err = host.fs.removeAll(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
		if _, err := os.Stat(tree); !os.IsNotExist(err) {
			t.Fatalf("tree still exists: err=%v", err)
		}
	})

	t.Run("non-existent path succeeds", func(t *testing.T) {
		L.SetGlobal("target", glua.LString(filepath.Join(dir, "never-existed")))
		if err := L.DoString(`ok, err = host.fs.removeAll(target)`); err != nil {
			t.Fatalf("DoString: %v", err)
		}
		if L.GetGlobal("ok") != glua.LTrue || L.GetGlobal("err") != glua.LNil {
			t.Fatalf("got ok=%v err=%v, want true, nil", L.GetGlobal("ok"), L.GetGlobal("err"))
		}
	})

	t.Run("refuses root and relative dot paths", func(t *testing.T) {
		cases := []string{
			`host.fs.removeAll("/")`,
			`host.fs.removeAll("///")`,
			`host.fs.removeAll("/.")`,
			`host.fs.removeAll(".")`,
			`host.fs.removeAll("./")`,
			`host.fs.removeAll("..")`,
			`host.fs.removeAll("../")`,
			`host.fs.removeAll("a/..")`,
			`host.fs.removeAll("")`,
			`host.fs.removeAll("bad\0path")`,
		}
		for _, script := range cases {
			if err := L.DoString(script); err == nil {
				t.Fatalf("script %q succeeded, want arg error", script)
			}
		}
	})
}
