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

func TestLuaSyncProcessExecOptions(t *testing.T) {
	t.Setenv("EXTHOST_EXEC_KEEP", "kept")
	t.Setenv("EXTHOST_EXEC_OVERRIDE", "host-value")
	t.Setenv("EXTHOST_EXEC_REMOVE", "host-remove")

	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	workDir := t.TempDir()
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		resolvedWorkDir = workDir
	}

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.exec.options", "name": "Exec options test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local exec = host.process.exec
		host.app.route("GET", "/cwd-env", function(req)
			local dir = req.query.dir
			local result = assert(exec("sh", {"-c", 'printf "%s|%s|%s|%s|%s" "$(pwd -P)" "$EXTHOST_EXEC_KEEP" "$EXTHOST_EXEC_OVERRIDE" "${EXTHOST_EXEC_REMOVE-unset}" "$EXTHOST_EXEC_ADDED"'}, {
				cwd = dir,
				env = {
					EXTHOST_EXEC_OVERRIDE = "child-value",
					EXTHOST_EXEC_REMOVE = false,
					EXTHOST_EXEC_ADDED = "added-value",
				},
			}))
			assert(result.exitCode == 0, result.stderr)
			return {body = result.stdout}
		end)
		host.app.route("GET", "/max-output", function()
			local large = assert(exec("head", {"-c", "100000", "/dev/zero"}, {maxOutputBytes = 131072}))
			assert(large.exitCode == 0 and #large.stdout == 100000 and large.stdoutTruncated == false)
			assert(large.stderr == "" and large.stderrTruncated == false)

			local small = assert(exec("sh", {"-c", "head -c 32 /dev/zero; head -c 16 /dev/zero >&2"}, {maxOutputBytes = 16}))
			assert(small.exitCode == 0)
			assert(#small.stdout == 16 and small.stdoutTruncated == true)
			assert(#small.stderr == 16 and small.stderrTruncated == false)
			return {body = "ok"}
		end)
		host.app.route("GET", "/invalid", function()
			local res, err = exec("pwd", {}, {cwd = "/nonexistent/extension-host/cwd"})
			assert(res == nil and type(err) == "string")

			local badCalls = {
				function() exec("pwd", {}, {cwd = ""}) end,
				function() exec("pwd", {}, {cwd = "/tmp\000bad"}) end,
				function() exec("pwd", {}, {cwd = 42}) end,
				function() exec("pwd", {}, {env = "not-a-table"}) end,
				function() exec("pwd", {}, {env = {[""] = "x"}}) end,
				function() exec("pwd", {}, {env = {["A=B"] = "x"}}) end,
				function() exec("pwd", {}, {env = {["A\000B"] = "x"}}) end,
				function() exec("pwd", {}, {env = {A = 123}}) end,
				function() exec("pwd", {}, {env = {A = "x\000y"}}) end,
				function() exec("pwd", {}, {maxOutputBytes = 0}) end,
				function() exec("pwd", {}, {maxOutputBytes = -5}) end,
				function() exec("pwd", {}, {maxOutputBytes = 4194305}) end,
				function() exec("pwd", {}, {maxOutputBytes = 12.5}) end,
				function() exec("pwd", {}, {maxOutputBytes = "64"}) end,
				function() exec("pwd", {}, {maxOutputBytes = math.huge}) end,
			}
			for _, fn in ipairs(badCalls) do
				local ok, callErr = pcall(fn)
				assert(not ok, "expected argument error")
				assert(tostring(callErr):find("#3", 1, true), "expected arg #3 error, got: " .. tostring(callErr))
			end
			return {body = "ok"}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(path string) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, http.MethodGet, plugin.APIPath+path, nil))
		return res
	}

	res := call("cwd-env?dir=" + workDir)
	want := resolvedWorkDir + "|kept|child-value|unset|added-value"
	if res.Code != http.StatusOK || res.Body.String() != want {
		t.Fatalf("cwd-env = %d, %q; want %q", res.Code, res.Body.String(), want)
	}
	if os.Getenv("EXTHOST_EXEC_OVERRIDE") != "host-value" || os.Getenv("EXTHOST_EXEC_REMOVE") != "host-remove" || os.Getenv("EXTHOST_EXEC_ADDED") != "" {
		t.Fatal("exec mutated host environment")
	}
	for _, path := range []string{"max-output", "invalid"} {
		t.Run(path, func(t *testing.T) {
			if r := call(path); r.Code != http.StatusOK || r.Body.String() != "ok" {
				t.Fatalf("%s = %d, %q", path, r.Code, r.Body.String())
			}
		})
	}
}

func TestLuaAsyncProcessStartKillsProcessGroup(t *testing.T) {
	ctx := testContext(t)
	extensionHost, root := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.start.pgid", "name": "Start pgid test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.route("POST", "/launch", function()
			local pid = assert(host.process.start("sh", {
				"-c",
				'sleep 30 & printf "%s\n" "$!" > "$1"; wait',
				"sh",
				host.dataDir .. "/child.pid",
			}, function() end))
			return {body = tostring(pid)}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, http.MethodPost, plugin.APIPath+"launch", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("launch = %d, %q", res.Code, res.Body)
	}
	parentPID, err := strconv.Atoi(strings.TrimSpace(res.Body.String()))
	if err != nil || parentPID <= 0 {
		t.Fatalf("parent pid = %q, %v", res.Body.String(), err)
	}

	childPIDFile := filepath.Join(root, "plugins", plugin.ID, "data", "child.pid")
	childPID := waitForPIDFile(t, childPIDFile)
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("grandchild %d not alive before stop: %v", childPID, err)
	}

	if err := extensionHost.Stop(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, parentPID)
	waitForProcessGone(t, childPID)
}

func TestLuaSpawnStreamingAndOrdering(t *testing.T) {
	t.Setenv("EXTHOST_SPAWN_OVERRIDE", "host-val")
	t.Setenv("EXTHOST_SPAWN_REMOVE", "host-rem")

	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	workDir := t.TempDir()
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		resolvedWorkDir = workDir
	}

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.spawn.stream", "name": "Spawn stream test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local spawn = host.process.spawn
		local gate = host.dataDir .. "/release"
		local handle = nil
		local state = {
			stdout = "",
			stderr = "",
			stdoutBeforeExit = "",
			stderrBeforeExit = "",
			runningInExit = nil,
			stopInExit = nil,
			exit = nil,
		}

		host.app.route("POST", "/start", function(req)
			local dir = req.query.dir
			handle = assert(spawn("sh", {
				"-c",
				'in=$(cat); printf "first:%s:%s:%s:%s|" "$in" "$(pwd -P)" "$EXTHOST_SPAWN_OVERRIDE" "${EXTHOST_SPAWN_REMOVE-unset}"; printf "err1|" >&2; while [ ! -f "$1" ]; do sleep 0.01; done; printf "final-out"; printf "err2" >&2; exit 23',
				"sh",
				gate,
			}, {
				cwd = dir,
				env = {
					EXTHOST_SPAWN_OVERRIDE = "spawn-val",
					EXTHOST_SPAWN_REMOVE = false,
				},
				stdin = "hello-stdin",
				onStdout = function(chunk)
					state.stdout = state.stdout .. chunk
				end,
				onStderr = function(chunk)
					state.stderr = state.stderr .. chunk
				end,
				onExit = function(result)
					state.stdoutBeforeExit = state.stdout
					state.stderrBeforeExit = state.stderr
					state.runningInExit = handle:running()
					state.stopInExit = handle:stop()
					state.exit = result
				end,
			}))
			assert(handle:running() == true)
			return {body = tostring(handle:pid())}
		end)

		host.app.route("POST", "/release", function()
			assert(io.open(gate, "w")):close()
			return {body = "ok"}
		end)

		host.app.route("GET", "/state", function()
			return {
				headers = {["content-type"] = "application/json"},
				body = host.json.encode({
					running = handle and handle:running() or false,
					stdout = state.stdout,
					stderr = state.stderr,
					stdoutBeforeExit = state.stdoutBeforeExit,
					stderrBeforeExit = state.stderrBeforeExit,
					runningInExit = state.runningInExit,
					stopInExit = state.stopInExit,
					exit = state.exit,
				}),
			}
		end)

		host.app.route("GET", "/no-streams", function()
			local done = nil
			local h = assert(spawn("sh", {"-c", "printf ignored-out; printf ignored-err >&2; exit 0"}, {
				onExit = function(res) done = res end,
			}))
			assert(h:pid() > 0)
			return {body = "ok"}
		end)

		host.app.route("GET", "/invalid", function()
			local h, err = spawn("/nonexistent/extension-host/command", {})
			assert(h == nil and type(err) == "string")

			local bad = {
				function() spawn(123, {}) end,
				function() spawn("true", {"ok", extra = "no"}) end,
				function() spawn("true", {}, {cwd = ""}) end,
				function() spawn("true", {}, {cwd = "/tmp\000x"}) end,
				function() spawn("true", {}, {env = "bad"}) end,
				function() spawn("true", {}, {env = {["A=B"] = "1"}}) end,
				function() spawn("true", {}, {stdin = 123}) end,
				function() spawn("true", {}, {timeoutMs = 0}) end,
				function() spawn("true", {}, {timeoutMs = 1.5}) end,
				function() spawn("true", {}, {onStdout = "not-fn"}) end,
				function() spawn("true", {}, {onStderr = 123}) end,
				function() spawn("true", {}, {onExit = {}}) end,
			}
			for _, fn in ipairs(bad) do
				assert(not pcall(fn), "expected argument error")
			end
			if handle then
				assert(not pcall(function() handle:stop(-1) end))
				assert(not pcall(function() handle:stop(30001) end))
				assert(not pcall(function() handle:stop(1.5) end))
				assert(not pcall(function() handle:stop("100") end))
			end
			return {body = "ok"}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(method, path string) string {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, method, plugin.APIPath+path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, %q", method, path, res.Code, res.Body)
		}
		return res.Body.String()
	}

	pid, err := strconv.Atoi(call(http.MethodPost, "start?dir="+workDir))
	if err != nil || pid <= 0 {
		t.Fatalf("spawn pid = %d, %v", pid, err)
	}

	type spawnExit struct {
		PID                int    `json:"pid"`
		ExitCode           int    `json:"exitCode"`
		TimedOut           bool   `json:"timedOut"`
		Stopped            bool   `json:"stopped"`
		StdoutDroppedBytes int64  `json:"stdoutDroppedBytes"`
		StderrDroppedBytes int64  `json:"stderrDroppedBytes"`
		Error              string `json:"error"`
	}
	type spawnState struct {
		Running          bool       `json:"running"`
		Stdout           string     `json:"stdout"`
		Stderr           string     `json:"stderr"`
		StdoutBeforeExit string     `json:"stdoutBeforeExit"`
		StderrBeforeExit string     `json:"stderrBeforeExit"`
		RunningInExit    *bool      `json:"runningInExit"`
		StopInExit       *bool      `json:"stopInExit"`
		Exit             *spawnExit `json:"exit"`
	}
	getState := func() spawnState {
		var s spawnState
		if err := json.Unmarshal([]byte(call(http.MethodGet, "state")), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	wantFirstOut := "first:hello-stdin:" + resolvedWorkDir + ":spawn-val:unset|"
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := getState()
		if s.Stdout == wantFirstOut && s.Stderr == "err1|" {
			if !s.Running || s.Exit != nil {
				t.Fatalf("process should still be running while streaming: %+v", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for live chunks; got %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}

	call(http.MethodPost, "release")

	var final spawnState
	deadline = time.Now().Add(3 * time.Second)
	for {
		final = getState()
		if final.Exit != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for exit; got %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
	}

	wantFullOut := wantFirstOut + "final-out"
	if final.Running {
		t.Fatal("handle:running() should be false after exit")
	}
	if final.RunningInExit == nil || *final.RunningInExit {
		t.Fatalf("runningInExit = %v, want false", final.RunningInExit)
	}
	if final.StopInExit == nil || *final.StopInExit {
		t.Fatalf("stopInExit = %v, want false", final.StopInExit)
	}
	if final.StdoutBeforeExit != wantFullOut || final.StderrBeforeExit != "err1|err2" {
		t.Fatalf("chunks were not completely drained before onExit: stdout=%q stderr=%q", final.StdoutBeforeExit, final.StderrBeforeExit)
	}
	if final.Exit.PID != pid || final.Exit.ExitCode != 23 || final.Exit.TimedOut || final.Exit.Stopped ||
		final.Exit.StdoutDroppedBytes != 0 || final.Exit.StderrDroppedBytes != 0 || final.Exit.Error != "" {
		t.Fatalf("unexpected exit result: %+v", final.Exit)
	}
	if os.Getenv("EXTHOST_SPAWN_OVERRIDE") != "host-val" || os.Getenv("EXTHOST_SPAWN_REMOVE") != "host-rem" {
		t.Fatal("spawn mutated host environment")
	}

	if got := call(http.MethodGet, "no-streams"); got != "ok" {
		t.Fatalf("no-streams = %q", got)
	}
	if got := call(http.MethodGet, "invalid"); got != "ok" {
		t.Fatalf("invalid = %q", got)
	}
}

func TestLuaSpawnStopTimeoutAndCleanup(t *testing.T) {
	ctx := testContext(t)
	extensionHost, root := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.spawn.control", "name": "Spawn control test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local spawn = host.process.spawn
		local handles = {}
		local results = {}
		local outputs = {}

		host.app.route("POST", "/graceful", function()
			outputs.graceful = ""
			results.graceful = nil
			handles.graceful = assert(spawn("sh", {
				"-c",
				'trap "printf caught-term; exit 0" TERM; printf ready; while true; do sleep 0.05; done',
			}, {
				onStdout = function(chunk) outputs.graceful = outputs.graceful .. chunk end,
				onExit = function(res) results.graceful = res end,
			}))
			return {body = tostring(handles.graceful:pid())}
		end)

		host.app.route("POST", "/escalate", function()
			results.escalate = nil
			handles.escalate = assert(spawn("sh", {
				"-c",
				'trap "" TERM; sleep 30 & printf "%s\n" "$!" > "$1"; wait',
				"sh",
				host.dataDir .. "/escalate.pid",
			}, {
				onExit = function(res) results.escalate = res end,
			}))
			return {body = tostring(handles.escalate:pid())}
		end)

		host.app.route("POST", "/kill-now", function()
			results.killnow = nil
			handles.killnow = assert(spawn("sh", {
				"-c",
				'trap "" TERM; sleep 30 & printf "%s\n" "$!" > "$1"; wait',
				"sh",
				host.dataDir .. "/killnow.pid",
			}, {
				onExit = function(res) results.killnow = res end,
			}))
			return {body = tostring(handles.killnow:pid())}
		end)

		host.app.route("POST", "/timeout", function()
			results.timeout = nil
			handles.timeout = assert(spawn("sh", {
				"-c",
				'sleep 30 & printf "%s\n" "$!" > "$1"; wait',
				"sh",
				host.dataDir .. "/timeout.pid",
			}, {
				timeoutMs = 250,
				onExit = function(res) results.timeout = res end,
			}))
			return {body = tostring(handles.timeout:pid())}
		end)

		host.app.route("POST", "/orphan", function()
			handles.orphan = assert(spawn("sh", {
				"-c",
				'sleep 30 & printf "%s\n" "$!" > "$1"; wait',
				"sh",
				host.dataDir .. "/orphan.pid",
			}))
			return {body = tostring(handles.orphan:pid())}
		end)

		host.app.route("POST", "/stop-handle", function(req)
			local name = req.query.name
			local h = assert(handles[name])
			local first, second
			if req.query.grace then
				local g = tonumber(req.query.grace)
				first = h:stop(g)
				second = h:stop(g)
			else
				first = h:stop()
				second = h:stop()
			end
			return {
				headers = {["content-type"] = "application/json"},
				body = host.json.encode({first = first, second = second}),
			}
		end)

		host.app.route("GET", "/status", function(req)
			local name = req.query.name
			return {
				headers = {["content-type"] = "application/json"},
				body = host.json.encode({
					running = handles[name] and handles[name]:running() or false,
					output = outputs[name] or "",
					exit = results[name],
				}),
			}
		end)

		host.app.on("stop", function()
			local h, err = spawn("true", {})
			local f = assert(io.open(host.dataDir .. "/stop-spawn-check", "w"))
			f:write(tostring(h == nil) .. ":" .. tostring(err))
			f:close()
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	dataDir := filepath.Join(root, "plugins", plugin.ID, "data")

	call := func(method, path string) string {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, method, plugin.APIPath+path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, %q", method, path, res.Code, res.Body)
		}
		return res.Body.String()
	}

	type exitInfo struct {
		PID      int  `json:"pid"`
		ExitCode int  `json:"exitCode"`
		TimedOut bool `json:"timedOut"`
		Stopped  bool `json:"stopped"`
	}
	type statusInfo struct {
		Running bool      `json:"running"`
		Output  string    `json:"output"`
		Exit    *exitInfo `json:"exit"`
	}
	waitExit := func(name string) statusInfo {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var s statusInfo
			if err := json.Unmarshal([]byte(call(http.MethodGet, "status?name="+name)), &s); err != nil {
				t.Fatal(err)
			}
			if s.Exit != nil {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s exit; status=%+v", name, s)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stopHandle := func(name, grace string) {
		t.Helper()
		q := "stop-handle?name=" + name
		if grace != "" {
			q += "&grace=" + grace
		}
		var resp struct {
			First  bool `json:"first"`
			Second bool `json:"second"`
		}
		if err := json.Unmarshal([]byte(call(http.MethodPost, q)), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.First || resp.Second {
			t.Fatalf("stop(%s) idempotency = %+v, want first=true second=false", name, resp)
		}
	}

	// 1. Graceful SIGTERM stop.
	call(http.MethodPost, "graceful")
	deadline := time.Now().Add(2 * time.Second)
	for {
		var s statusInfo
		_ = json.Unmarshal([]byte(call(http.MethodGet, "status?name=graceful")), &s)
		if strings.Contains(s.Output, "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("graceful child did not become ready: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopHandle("graceful", "")
	graceful := waitExit("graceful")
	if graceful.Running || !graceful.Exit.Stopped || graceful.Exit.TimedOut || graceful.Exit.ExitCode != 0 ||
		!strings.Contains(graceful.Output, "caught-term") {
		t.Fatalf("graceful stop result = %+v, exit = %+v", graceful, graceful.Exit)
	}

	// 2. SIGTERM ignored -> escalated to SIGKILL after graceMs, killing grandchild too.
	call(http.MethodPost, "escalate")
	escalateChild := waitForPIDFile(t, filepath.Join(dataDir, "escalate.pid"))
	stopHandle("escalate", "150")
	escalate := waitExit("escalate")
	if escalate.Running || !escalate.Exit.Stopped || escalate.Exit.TimedOut || escalate.Exit.ExitCode != -1 {
		t.Fatalf("escalate stop result = %+v, exit = %+v", escalate, escalate.Exit)
	}
	waitForProcessGone(t, escalateChild)

	// 3. Immediate SIGKILL (graceMs = 0), killing grandchild too.
	call(http.MethodPost, "kill-now")
	killNowChild := waitForPIDFile(t, filepath.Join(dataDir, "killnow.pid"))
	stopHandle("killnow", "0")
	killNow := waitExit("killnow")
	if killNow.Running || !killNow.Exit.Stopped || killNow.Exit.TimedOut || killNow.Exit.ExitCode != -1 {
		t.Fatalf("kill-now result = %+v, exit = %+v", killNow, killNow.Exit)
	}
	waitForProcessGone(t, killNowChild)

	// 4. Timeout killing process group.
	call(http.MethodPost, "timeout")
	timeoutChild := waitForPIDFile(t, filepath.Join(dataDir, "timeout.pid"))
	timeoutRes := waitExit("timeout")
	if timeoutRes.Running || !timeoutRes.Exit.TimedOut || timeoutRes.Exit.Stopped || timeoutRes.Exit.ExitCode != -1 {
		t.Fatalf("timeout result = %+v, exit = %+v", timeoutRes, timeoutRes.Exit)
	}
	waitForProcessGone(t, timeoutChild)

	// 5. Plugin stop kills active spawned process group and rejects spawn during stop hook.
	orphanPID, err := strconv.Atoi(call(http.MethodPost, "orphan"))
	if err != nil || orphanPID <= 0 {
		t.Fatalf("orphan pid = %d, %v", orphanPID, err)
	}
	orphanChild := waitForPIDFile(t, filepath.Join(dataDir, "orphan.pid"))

	if err := extensionHost.Stop(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, orphanPID)
	waitForProcessGone(t, orphanChild)

	stopCheck, err := os.ReadFile(filepath.Join(dataDir, "stop-spawn-check"))
	if err != nil || !strings.HasPrefix(string(stopCheck), "true:") {
		t.Fatalf("stop hook spawn check = %q, %v", stopCheck, err)
	}
}

func TestLuaSpawnBackpressureAndResponsiveness(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.spawn.pressure", "name": "Spawn pressure test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local spawn = host.process.spawn
		local received = 0
		local exitResult = nil
		local chunkErrors = 0
		local errDemoReceived = 0
		local errDemoExit = nil

		host.app.route("POST", "/flood", function()
			received = 0
			exitResult = nil
			assert(spawn("head", {"-c", "1048576", "/dev/zero"}, {
				onStdout = function(chunk)
					received = received + #chunk
				end,
				onExit = function(res)
					exitResult = res
				end,
			}))
			-- Keep Lua busy briefly while the child writes 1 MiB so the 256 KiB buffer overflows.
			host.process.exec("sleep", {"0.3"})
			return {body = "ok"}
		end)

		host.app.route("GET", "/flood-status", function()
			return {
				headers = {["content-type"] = "application/json"},
				body = host.json.encode({
					received = received,
					exit = exitResult,
				}),
			}
		end)

		host.app.route("POST", "/callback-error", function()
			chunkErrors = 0
			errDemoReceived = 0
			errDemoExit = nil
			assert(spawn("sh", {"-c", "head -c 20000 /dev/zero; sleep 0.08; head -c 100 /dev/zero"}, {
				onStdout = function(chunk)
					errDemoReceived = errDemoReceived + #chunk
					if chunkErrors == 0 then
						chunkErrors = 1
						error("intentional callback failure")
					end
				end,
				onExit = function(res)
					errDemoExit = res
				end,
			}))
			return {body = "ok"}
		end)

		host.app.route("GET", "/callback-error-status", function()
			return {
				headers = {["content-type"] = "application/json"},
				body = host.json.encode({
					received = errDemoReceived,
					exit = errDemoExit,
				}),
			}
		end)

		host.app.route("POST", "/background-stream", function()
			assert(spawn("sh", {"-c", "i=0; while [ $i -lt 20 ]; do printf 'tick\n'; sleep 0.02; i=$((i+1)); done"}, {
				onStdout = function() end,
			}))
			return {body = "ok"}
		end)

		host.app.route("GET", "/ping", function()
			return {body = "pong"}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	call := func(method, path string) string {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, method, plugin.APIPath+path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, %q", method, path, res.Code, res.Body)
		}
		return res.Body.String()
	}

	// 1. Flood >256 KiB while Lua is busy -> child exits cleanly without deadlock, exact byte accounting.
	call(http.MethodPost, "flood")
	type floodStatus struct {
		Received int64 `json:"received"`
		Exit     *struct {
			ExitCode           int   `json:"exitCode"`
			StdoutDroppedBytes int64 `json:"stdoutDroppedBytes"`
			StderrDroppedBytes int64 `json:"stderrDroppedBytes"`
		} `json:"exit"`
	}
	deadline := time.Now().Add(3 * time.Second)
	var status floodStatus
	for {
		if err := json.Unmarshal([]byte(call(http.MethodGet, "flood-status")), &status); err != nil {
			t.Fatal(err)
		}
		if status.Exit != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flood process did not finish; status=%+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Exit.ExitCode != 0 {
		t.Fatalf("flood exitCode = %d", status.Exit.ExitCode)
	}
	if status.Exit.StdoutDroppedBytes <= 0 {
		t.Fatalf("expected stdoutDroppedBytes > 0 under backpressure, got %+v", status)
	}
	if status.Received+status.Exit.StdoutDroppedBytes != 1048576 {
		t.Fatalf("byte conservation failed: received=%d dropped=%d sum=%d",
			status.Received, status.Exit.StdoutDroppedBytes, status.Received+status.Exit.StdoutDroppedBytes)
	}

	// 2. Callback error is logged and does not crash runtime or block subsequent chunks/onExit.
	call(http.MethodPost, "callback-error")
	deadline = time.Now().Add(3 * time.Second)
	for {
		var errStatus floodStatus
		if err := json.Unmarshal([]byte(call(http.MethodGet, "callback-error-status")), &errStatus); err != nil {
			t.Fatal(err)
		}
		if errStatus.Exit != nil {
			if errStatus.Received != 20100 {
				t.Fatalf("received after callback error = %d, want 20100", errStatus.Received)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback-error process did not exit; status=%+v", errStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 3. HTTP requests remain responsive while spawned process streams in background.
	call(http.MethodPost, "background-stream")
	for i := range 10 {
		if got := call(http.MethodGet, "ping"); got != "pong" {
			t.Fatalf("ping #%d = %q", i, got)
		}
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for PID file %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if status, readErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status"); readErr == nil {
			text := string(status)
			if strings.Contains(text, "\nState:\tZ") || strings.Contains(text, "\nState:\tX") {
				return
			}
		} else if os.IsNotExist(readErr) {
			return
		}
		if time.Now().After(deadline) {
			status, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
			t.Fatalf("process %d still alive: kill=%v status=%s", pid, err, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
