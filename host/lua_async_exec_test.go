package host_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
)

func TestLuaAsyncHTTPHandlers(t *testing.T) {
	ctx := testContext(t)
	extensionHost, root := newTestHost(t, ctx)
	probeStarted := make(chan struct{}, 1)
	probeCanceled := make(chan struct{}, 1)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			probeStarted <- struct{}{}
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		case "/cancel":
			probeStarted <- struct{}{}
			select {
			case <-r.Context().Done():
				probeCanceled <- struct{}{}
			case <-time.After(5 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, "probe")
	}))
	t.Cleanup(probe.Close)

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.async", "name": "Async handler test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local exec = host.process.exec
		local request = host.http.request
		local sleeper

		host.app.route("GET", "/fast", function()
			return {body = "fast"}
		end)

		host.app.route("GET", "/slow-exec", function()
			sleeper = coroutine.running()
			local marker = assert(io.open(host.dataDir .. "/exec-started", "w"))
			marker:close()
			assert(exec("sleep", {"0.4"}))
			return {body = "slept"}
		end)

		host.app.route("GET", "/resume-sleeper", function()
			local ok, err = coroutine.resume(sleeper)
			assert(not ok, "Lua resumed a host-owned coroutine")
			return {body = err}
		end)

		host.app.route("GET", "/probe", function(req)
			local res = assert(request({url = req.query.url}))
			return {body = res.body}
		end)

		host.app.route("GET", "/sequential", function(req)
			local function run(text)
				return exec("printf", {"%s", text})
			end
			local first = assert(run("one"))
			local second = assert(exec("printf", {"%s", "two"}))
			local res = assert(request({url = req.query.url}))
			return {body = first.stdout .. "|" .. second.stdout .. "|" .. res.body}
		end)

		host.app.route("GET", "/protected", function()
			local a, first = pcall(exec, "printf", {"%s", "pcall"})
			assert(a, first)
			local b, second = xpcall(function()
				return exec("printf", {"%s", "xpcall"})
			end, tostring)
			assert(b, second)
			return {body = first.stdout .. "|" .. second.stdout}
		end)

		host.app.route("GET", "/metamethod", function(req)
			local obj = setmetatable({}, {__index = function()
				return request({url = req.query.url})
			end})
			return {body = obj.response.body}
		end)

		host.app.route("GET", "/cancel-exec", function()
			exec("sh", {"-c", 'printf "%s\n" "$$" > "$1"; exec sleep 10', "sh", host.dataDir .. "/cancel.pid"})
			return {body = "finished"}
		end)

		host.app.route("GET", "/yield", function()
			coroutine.yield("unexpected")
			return {body = "finished"}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	dataDir := filepath.Join(root, "plugins", plugin.ID, "data")

	call := func(callCtx context.Context, path string) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(callCtx, http.MethodGet, plugin.APIPath+path, nil))
		return res
	}
	callAsync := func(callCtx context.Context, path string) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- call(callCtx, path) }()
		return done
	}
	awaitResponse := func(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case res := <-done:
			return res
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for handler to finish")
			return nil
		}
	}
	checkBody := func(t *testing.T, res *httptest.ResponseRecorder, want string) {
		t.Helper()
		if res.Code != http.StatusOK || res.Body.String() != want {
			t.Fatalf("response = %d %q, want 200 %q", res.Code, res.Body.String(), want)
		}
	}
	checkFast := func(t *testing.T) {
		t.Helper()
		fastCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		res := call(fastCtx, "fast")
		if elapsed := time.Since(start); elapsed >= 150*time.Millisecond {
			t.Fatalf("fast request took %s, want less than 150ms", elapsed)
		}
		checkBody(t, res, "fast")
	}
	awaitSignal := func(t *testing.T, signal <-chan struct{}, event string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", event)
		}
	}

	t.Run("exec does not block other handlers", func(t *testing.T) {
		done := callAsync(ctx, "slow-exec")
		marker := filepath.Join(dataDir, "exec-started")
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			} else if !os.IsNotExist(err) || time.Now().After(deadline) {
				t.Fatalf("waiting for slow exec handler: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		checkFast(t)
		checkBody(t, call(ctx, "resume-sleeper"), "cannot resume an HTTP handler coroutine")
		checkBody(t, awaitResponse(t, done), "slept")
	})
	t.Run("handler resumes between consecutive calls", func(t *testing.T) {
		checkBody(t, call(ctx, "sequential?url="+probe.URL), "one|two|probe")
	})
	t.Run("protected calls still work", func(t *testing.T) {
		checkBody(t, call(ctx, "protected"), "pcall|xpcall")
		checkBody(t, call(ctx, "metamethod?url="+probe.URL), "probe")
	})
	t.Run("HTTP probe does not block other handlers", func(t *testing.T) {
		done := callAsync(ctx, "probe?url="+probe.URL+"/slow")
		awaitSignal(t, probeStarted, "slow probe to start")
		checkFast(t)
		checkBody(t, awaitResponse(t, done), "probe")
	})
	t.Run("client cancellation kills exec", func(t *testing.T) {
		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := callAsync(reqCtx, "cancel-exec")
		pid := waitForPIDFile(t, filepath.Join(dataDir, "cancel.pid"))
		cancel()
		awaitResponse(t, done)
		waitForProcessGone(t, pid)
		checkFast(t)
	})
	t.Run("client cancellation closes HTTP request", func(t *testing.T) {
		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := callAsync(reqCtx, "probe?url="+probe.URL+"/cancel")
		awaitSignal(t, probeStarted, "cancelable probe to start")
		cancel()
		awaitResponse(t, done)
		awaitSignal(t, probeCanceled, "upstream cancellation")
		checkFast(t)
	})
	t.Run("direct yield fails the request", func(t *testing.T) {
		res := call(ctx, "yield")
		if res.Code != http.StatusInternalServerError {
			t.Fatalf("unexpected yield response = %d %q", res.Code, res.Body.String())
		}
		checkFast(t)
	})
}
