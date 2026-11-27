package lua

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runHostHelperScript(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	entry := filepath.Join(dir, "main.lua")
	if err := os.WriteFile(entry, []byte(script), 0o600); err != nil {
		t.Fatalf("write entry script: %v", err)
	}
	r := New(Options{EntryPath: entry, PackageRoot: dir, DataDir: dir})
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := r.Stop(t.Context()); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
}

func TestHostJSONArray(t *testing.T) {
	runHostHelperScript(t, `
		local json = host.json
		assert(json.encode(json.array()) == "[]")
		assert(json.encode(json.array(nil)) == "[]")
		assert(json.encode(json.array({})) == "[]")
		assert(json.encode(json.array({"a", "b"})) == '["a","b"]')

		local mt = {__index = {[2] = "inherited"}}
		local source = setmetatable({"a"}, mt)
		local copy = json.array(source)
		assert(copy ~= source and getmetatable(source) == mt)
		assert(getmetatable(copy).__host_array == true)
		assert(json.encode(copy) == '["a"]')
		copy[1] = nil
		assert(source[1] == "a" and json.encode(copy) == "[]")

		local function assertBadArray(value)
			local ok, err = pcall(json.array, value)
			assert(not ok and string.find(err, "bad argument #1", 1, true), err)
		end
		assertBadArray("a")
		assertBadArray({key = "a"})
		assertBadArray({"a", key = "b"})
		assertBadArray({[0] = "a"})
		assertBadArray({[-1] = "a"})
		assertBadArray({[1.5] = "a"})
		assertBadArray({[true] = "a"})
		assertBadArray(setmetatable({[1] = "a", [3] = "b"}, mt))
	`)
}

func TestHostProcessLookPath(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("locate sh: %v", err)
	}
	sh, err = filepath.Abs(sh)
	if err != nil {
		t.Fatalf("resolve sh path: %v", err)
	}
	runHostHelperScript(t, fmt.Sprintf(`
		local lookPath = host.process.lookPath
		local path, err = lookPath("sh")
		assert(type(path) == "string" and path ~= "" and path == %q, err)
		assert(err == nil)

		path, err = lookPath("extension-host-h3-nonexistent-command")
		assert(path == nil and type(err) == "string" and err ~= "")

		local function assertBadName(...)
			local ok, err = pcall(lookPath, ...)
			assert(not ok and string.find(err, "bad argument #1", 1, true), err)
		end
		assertBadName()
		assertBadName("")
		assertBadName("sh\0")
		assertBadName(123)
	`, sh))
}

func TestHostUser(t *testing.T) {
	runHostHelperScript(t, `
		local user = host.user
		assert(type(user) == "table")
		assert(type(user.homeDir) == "string")
		assert(type(user.username) == "string")
		assert(type(user.uid) == "number" and user.uid % 1 == 0)
		assert(type(user.gid) == "number" and user.gid % 1 == 0)
		assert(type(user.os) == "string" and user.os ~= "")
		assert(type(user.arch) == "string" and user.arch ~= "")
		assert(not pcall(function() user.os = "changed" end))
		assert(not pcall(function() user.extra = true end))
		assert(not pcall(setmetatable, user, {}))
		assert(not pcall(rawset, user, "os", "changed"))
		assert(not pcall(table.insert, user, "extra"))
		assert(not pcall(function() module("host.user.extra") end))

		local ordinary = {}
		rawset(ordinary, "field", true)
		table.insert(ordinary, "value")
		assert(ordinary.field and ordinary[1] == "value")
		local function declareModule()
			module("h3.ordinary")
		end
		declareModule()
		assert(type(h3.ordinary) == "table")
	`)
}

func TestRuntimeMountsHostHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "host HTTP")
	}))
	defer server.Close()

	runHostHelperScript(t, fmt.Sprintf(`
		local response, err = host.http.request({url = %q})
		assert(response, err)
		assert(response.status == 200 and response.body == "host HTTP")
	`, server.URL))
}
