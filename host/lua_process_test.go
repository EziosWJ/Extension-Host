package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
)

func TestLuaProcessLibrariesAndExit(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.process", "name": "Process test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.on("stop", function() print("stop hook completed") end)
		host.app.route("GET", "/check", function()
			assert(type(os.setenv) == "function")
			assert(os.execute("exit 0") == 0)
			local command = assert(io.popen("printf plugin-output"))
			local output = command:read("*a")
			assert(command:close() == 0)
			return { body = output }
		end)
		host.app.route("POST", "/exit", function()
			pcall(os.exit, 7)
			print("execution continued after exit")
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(method, path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, plugin.APIPath+path, nil))
		return response
	}
	if response := call(http.MethodGet, "check"); response.Code != http.StatusOK || response.Body.String() != "plugin-output" {
		t.Fatalf("check response = %d, %q", response.Code, response.Body)
	}
	if response := call(http.MethodPost, "exit"); response.Code != http.StatusConflict {
		t.Fatalf("exit response = %d, %q", response.Code, response.Body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for plugin = getPlugin(t, extensionHost, plugin.ID); plugin.Status != host.StatusStopped && time.Now().Before(deadline); plugin = getPlugin(t, extensionHost, plugin.ID) {
		time.Sleep(time.Millisecond)
	}
	if !plugin.Enabled || plugin.Status != host.StatusStopped {
		t.Fatalf("after exit: enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
	log, err := os.ReadFile(plugin.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "stop hook completed") || !strings.Contains(string(log), "code=7") ||
		strings.Contains(string(log), "execution continued after exit") {
		t.Fatalf("exit log = %s", log)
	}
	if err := extensionHost.Start(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodGet, "check"); response.Code != http.StatusOK {
		t.Fatalf("check after start = %d, %q", response.Code, response.Body)
	}
}

func TestLuaExitDuringStartupKeepsPluginEnabled(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.startup", "name": "Startup test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.on("stop", function() print("startup stop hook completed") end)
		os.exit()
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	plugin = getPlugin(t, extensionHost, plugin.ID)
	if !plugin.Enabled || plugin.Status != host.StatusStopped || plugin.Error != "" {
		t.Fatalf("after exit: enabled %v, status %s, error %q", plugin.Enabled, plugin.Status, plugin.Error)
	}
	log, err := os.ReadFile(plugin.LogPath)
	if err != nil || !strings.Contains(string(log), "startup stop hook completed") {
		t.Fatalf("startup exit log = %q, %v", log, err)
	}
}

func TestLuaAsyncProcessReturnsBeforeExitAndStopsWithPlugin(t *testing.T) {
	t.Setenv("GODESK_PROCESS_TEST_OLD", "inherited")
	t.Setenv("GODESK_PROCESS_TEST_REMOVE", "inherited")
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.async", "name": "Async test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local gate = host.dataDir .. "/exit"
		local function start(program, args, env)
			return assert(host.process.start(program, args, function(result)
				host.storage.set("exit", host.json.encode(result))
			end, env))
		end
		host.app.route("POST", "/start", function()
			local pid = start("sh", {"-c", 'while [ ! -f "$1" ]; do sleep 0.01; done; printf "%s" "$2:$GODESK_PROCESS_TEST_OLD:${GODESK_PROCESS_TEST_REMOVE-unset}" >&2; exit 17',
				"sh", gate, "stderr with spaces ;"}, {
					GODESK_PROCESS_TEST_OLD = "overridden",
					GODESK_PROCESS_TEST_REMOVE = false,
				})
			return {body = tostring(pid)}
		end)
		host.app.route("POST", "/release", function()
			assert(io.open(gate, "w")):close()
			return {}
		end)
		host.app.route("POST", "/sleep", function()
			return {body = tostring(start("sleep", {"30"}))}
		end)
		host.app.route("GET", "/status", function()
			return {body = host.storage.get("exit") or ""}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(method, path string) string {
		t.Helper()
		requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(requestCtx, method, plugin.APIPath+path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s: %d, %q", method, path, res.Code, res.Body)
		}
		return res.Body.String()
	}
	pid, err := strconv.Atoi(call(http.MethodPost, "start"))
	if err != nil || pid <= 0 {
		t.Fatalf("launch pid = %d, %v", pid, err)
	}
	if got := call(http.MethodGet, "status"); got != "" {
		t.Fatalf("unexpected early completion %q", got)
	}
	call(http.MethodPost, "release")
	var result struct {
		PID      int    `json:"pid"`
		ExitCode int    `json:"exitCode"`
		Stderr   string `json:"stderr"`
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		body := call(http.MethodGet, "status")
		if body != "" {
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process completion was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	if result.PID != pid || result.ExitCode != 17 || result.Stderr != "stderr with spaces ;:overridden:unset" {
		t.Fatalf("process result = %+v", result)
	}
	if os.Getenv("GODESK_PROCESS_TEST_OLD") != "inherited" || os.Getenv("GODESK_PROCESS_TEST_REMOVE") != "inherited" {
		t.Fatal("child environment changed the host environment")
	}
	pid, err = strconv.Atoi(call(http.MethodPost, "sleep"))
	if err != nil || pid <= 0 {
		t.Fatalf("sleep pid = %d, %v", pid, err)
	}
	if err := extensionHost.Stop(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process still present after plugin stop: %v", err)
	}
}

func TestLuaSyncProcessExec(t *testing.T) {
	ctx := testContext(t)
	extensionHost, root := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.exec", "name": "Exec test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local exec = host.process.exec
		host.app.route("GET", "/result", function()
			local result = assert(exec("sh", {"-c", 'cat; printf "%s" "$1" >&2; exit 17', "sh", "with spaces ;"}, {
				stdin = "input\000bytes\n",
			}))
			assert(result.exitCode == 17 and result.stdout == "input\000bytes\n" and result.stderr == "with spaces ;")
			assert(result.stdoutTruncated == false and result.stderrTruncated == false and result.timedOut == false)
			assert(result.error == nil)
			result = assert(exec("cat", {}))
			assert(result.exitCode == 0 and result.stdout == "" and result.stderr == "")
			return {body = "ok"}
		end)
		host.app.route("GET", "/limits", function()
			local result = assert(exec("sh", {"-c", "head -c 65536 /dev/zero; head -c 65537 /dev/zero >&2"}))
			assert(result.exitCode == 0, result.stderr)
			assert(result.stdout == string.rep("\000", 65536) and result.stdoutTruncated == false)
			assert(result.stderr == result.stdout and result.stderrTruncated == true)
			result = assert(exec("head", {"-c", "65537", "/dev/zero"}))
			assert(#result.stdout == 65536 and result.stdoutTruncated == true and result.stderrTruncated == false)
			return {body = "ok"}
		end)
		host.app.route("GET", "/timeout", function()
			local result = assert(exec("sh", {"-c", 'printf started; sleep 30 & wait'}, {timeoutMs = 500}))
			assert(result.timedOut and result.exitCode == -1 and result.stdout == "started")
			return {body = "ok"}
		end)
		host.app.route("GET", "/invalid", function()
			local result, err = exec("/nonexistent/extension-host/command", {})
			assert(result == nil and type(err) == "string")
			assert(not pcall(exec, 123, {}))
			assert(not pcall(exec, "cat", {"one", [3] = "three"}))
			assert(not pcall(exec, "cat", {"one", extra = "two"}))
			assert(not pcall(exec, "cat", {123}))
			assert(not pcall(exec, "cat", {}, {stdin = false}))
			assert(not pcall(exec, "cat", {}, {timeoutMs = 0}))
			assert(not pcall(exec, "cat", {}, {timeoutMs = 1.5}))
			assert(not pcall(exec, "cat", {}, {timeoutMs = "1000"}))
			assert(not pcall(exec, "cat", {}, {timeoutMs = math.huge}))
			return {body = "ok"}
		end)
		host.app.route("GET", "/wait", function()
			exec("sh", {"-c", 'printf "%s\n" "$$" > "$1"; exec sleep 30', "sh", host.dataDir .. "/exec.pid"})
			return {}
		end)
		host.app.on("stop", function()
			assert(io.open(host.dataDir .. "/stop-hook", "w")):close()
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, plugin.APIPath+path, nil))
		return response
	}
	for _, path := range []string{"result", "limits", "timeout", "invalid"} {
		t.Run(path, func(t *testing.T) {
			if response := call(path); response.Code != http.StatusOK || response.Body.String() != "ok" {
				t.Fatalf("response = %d, %q", response.Code, response.Body)
			}
		})
	}

	dataDir := filepath.Join(root, "plugins", plugin.ID, "data")
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- call("wait") }()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for pid == 0 {
		data, _ := os.ReadFile(filepath.Join(dataDir, "exec.pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			t.Fatalf("exec did not write a PID to %s", dataDir)
		}
		time.Sleep(time.Millisecond)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := extensionHost.Stop(stopCtx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process still present after plugin stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "stop-hook")); err != nil {
		t.Fatalf("stop hook did not run: %v", err)
	}
	select {
	case <-response:
	case <-ctx.Done():
		t.Fatal("request did not return after stopping the plugin")
	}
}
