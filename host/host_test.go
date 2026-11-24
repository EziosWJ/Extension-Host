package host_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
)

const (
	helloID   = "com.godesk.hello"
	helloPath = "/extensions/com.godesk.hello"
)

func TestOpenCreatesDataDirectory(t *testing.T) {
	ctx := testContext(t)
	dataDir := filepath.Join(t.TempDir(), "nested", "host-data")
	extensionHost, err := host.Open(ctx, host.Options{
		DataDir: dataDir,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = extensionHost.Close(closeCtx)
	})
	if _, err := os.Stat(filepath.Join(dataDir, "host.db")); err != nil {
		t.Fatalf("host.db: %v", err)
	}
}

func TestHelloPluginLifecycle(t *testing.T) {
	ctx := testContext(t)
	extensionHost, dataDir := newTestHost(t, ctx)
	server := startServer(t, extensionHost)

	plugin := installExample(t, ctx, extensionHost, "hello")
	if plugin.Enabled || plugin.Status != host.StatusStopped {
		t.Fatalf("new installation = enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
	if !plugin.HasWeb || !plugin.HasLua {
		t.Fatalf("entries = web %v, lua %v", plugin.HasWeb, plugin.HasLua)
	}
	if list := listPlugins(t, extensionHost); len(list) != 1 {
		t.Fatalf("plugins = %d", len(list))
	}

	if status, _ := httpGet(t, server.URL+helloPath+"/web/"); status != http.StatusConflict {
		t.Fatalf("web entry before enable = %d", status)
	}
	if status, _ := httpGet(t, server.URL+helloPath+"/api/hello"); status != http.StatusConflict {
		t.Fatalf("API before enable = %d", status)
	}

	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	plugin = getPlugin(t, extensionHost, helloID)
	if !plugin.Enabled || plugin.Status != host.StatusRunning {
		t.Fatalf("enabled plugin = %v, %s", plugin.Enabled, plugin.Status)
	}
	if status, body := httpGet(t, server.URL+helloPath+"/web/"); status != 200 || !strings.Contains(body, "Hello Extension") {
		t.Fatalf("web entry = %d, %q", status, body)
	}

	var response struct {
		Message string `json:"message"`
		Visitor string `json:"visitor"`
		Visits  int    `json:"visits"`
		Secret  bool   `json:"secretConfigured"`
	}
	status, body := httpGet(t, server.URL+helloPath+"/api/hello?name=Ada")
	if status != 200 || json.Unmarshal([]byte(body), &response) != nil {
		t.Fatalf("hello = %d, %q", status, body)
	}
	if response.Visits != 1 || response.Visitor != "Ada" || !strings.Contains(response.Message, "你好") {
		t.Fatalf("response = %#v", response)
	}

	if status, body := httpGet(t, server.URL+helloPath+"/api/stats"); status != 200 || !strings.Contains(body, `"visits"`) {
		t.Fatalf("stats = %d, %q", status, body)
	}

	status = httpJSON(t, http.MethodPut, server.URL+"/api/v1/extensions/"+helloID+"/config",
		map[string]any{"greeting": "Bonjour", "token": "s3cret"}, &plugin)
	if status != 200 {
		t.Fatalf("config update = %d", status)
	}
	if _, leaked := plugin.Config["token"]; leaked {
		t.Fatal("secret value appears in the management response")
	}
	if plugin.Config["greeting"] != "Bonjour" {
		t.Fatalf("config = %#v", plugin.Config)
	}

	response = struct {
		Message string `json:"message"`
		Visitor string `json:"visitor"`
		Visits  int    `json:"visits"`
		Secret  bool   `json:"secretConfigured"`
	}{}
	status, body = httpGet(t, server.URL+helloPath+"/api/hello")
	if status != 200 || json.Unmarshal([]byte(body), &response) != nil {
		t.Fatalf("hello after config = %d, %q", status, body)
	}
	if response.Visits != 2 || !response.Secret || !strings.Contains(response.Message, "Bonjour") {
		t.Fatalf("response after config = %#v", response)
	}

	logData, err := os.ReadFile(plugin.LogPath)
	if err != nil {
		t.Fatalf("read plugin log: %v", err)
	}
	if strings.Contains(string(logData), "s3cret") {
		t.Fatal("secret value appears in the plugin log")
	}

	if err := extensionHost.Stop(ctx, helloID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	plugin = getPlugin(t, extensionHost, helloID)
	if !plugin.Enabled || plugin.Status != host.StatusStopped {
		t.Fatalf("stopped plugin = enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
	if status, _ := httpGet(t, server.URL+helloPath+"/api/hello"); status != http.StatusConflict {
		t.Fatalf("API while stopped = %d", status)
	}
	if err := extensionHost.Start(ctx, helloID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, body = httpGet(t, server.URL+helloPath+"/api/hello")
	if status != 200 || !strings.Contains(body, `"visits":3`) {
		t.Fatalf("hello after restart = %d, %q", status, body)
	}

	if err := extensionHost.Disable(ctx, helloID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	plugin = getPlugin(t, extensionHost, helloID)
	if plugin.Enabled || plugin.Status != host.StatusStopped {
		t.Fatalf("disabled plugin = enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
	if err := extensionHost.Start(ctx, helloID); err == nil {
		t.Fatal("Start on a disabled plugin succeeded")
	}

	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	status, body = httpGet(t, server.URL+helloPath+"/api/hello")
	if status != 200 || !strings.Contains(body, `"visits":4`) {
		t.Fatalf("hello after re-enable = %d, %q", status, body)
	}

	_ = dataDir
}

func TestRestartRestoresEnabledPluginsAndData(t *testing.T) {
	ctx := testContext(t)
	dataDir := t.TempDir()
	extensionHost := openHost(t, ctx, dataDir)
	installExample(t, ctx, extensionHost, "hello")
	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	server := startServer(t, extensionHost)
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 || !strings.Contains(body, `"visits":1`) {
		t.Fatalf("first run = %d, %q", status, body)
	}
	if status := httpJSON(t, http.MethodPut, server.URL+"/api/v1/extensions/"+helloID+"/config",
		map[string]any{"greeting": "Bonjour"}, nil); status != 200 {
		t.Fatalf("config update = %d", status)
	}
	if err := extensionHost.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted := openHost(t, ctx, dataDir)
	if err := restarted.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restartedServer := startServer(t, restarted)
	plugin := getPlugin(t, restarted, helloID)
	if !plugin.Enabled || plugin.Status != host.StatusRunning {
		t.Fatalf("restored plugin = %v, %s", plugin.Enabled, plugin.Status)
	}
	status, body := httpGet(t, restartedServer.URL+helloPath+"/api/hello")
	if status != 200 || !strings.Contains(body, `"visits":2`) || !strings.Contains(body, "Bonjour") {
		t.Fatalf("restored run = %d, %q", status, body)
	}

	if err := restarted.Disable(ctx, helloID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := restarted.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	third := openHost(t, ctx, dataDir)
	if err := third.Restore(ctx); err != nil {
		t.Fatalf("Restore disabled plugin: %v", err)
	}
	plugin = getPlugin(t, third, helloID)
	if plugin.Enabled || plugin.Status != host.StatusStopped {
		t.Fatalf("disabled plugin after restart = %v, %s", plugin.Enabled, plugin.Status)
	}
}

func TestUninstallDataSemantics(t *testing.T) {
	ctx := testContext(t)
	extensionHost, dataDir := newTestHost(t, ctx)
	server := startServer(t, extensionHost)
	installExample(t, ctx, extensionHost, "hello")
	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 || !strings.Contains(body, `"visits":1`) {
		t.Fatalf("first call = %d, %q", status, body)
	}

	if err := extensionHost.Uninstall(ctx, helloID, host.UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	pluginDir := filepath.Join(dataDir, "plugins", helloID)
	if _, err := os.Stat(filepath.Join(pluginDir, "business.db")); err != nil {
		t.Fatalf("business data was not kept: %v", err)
	}
	installExample(t, ctx, extensionHost, "hello")
	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable after keep-data uninstall: %v", err)
	}
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 || !strings.Contains(body, `"visits":2`) {
		t.Fatalf("kept data call = %d, %q", status, body)
	}

	if err := extensionHost.Uninstall(ctx, helloID, host.UninstallOptions{PurgeData: true}); err != nil {
		t.Fatalf("purge uninstall: %v", err)
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Fatalf("plugin data still exists after purge: %v", err)
	}
	installExample(t, ctx, extensionHost, "hello")
	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable after purge: %v", err)
	}
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 || !strings.Contains(body, `"visits":1`) {
		t.Fatalf("purged data call = %d, %q", status, body)
	}
}

func TestWebSocketEventsAndRelease(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServer(t, extensionHost)
	installExample(t, ctx, extensionHost, "hello")
	if err := extensionHost.Enable(ctx, helloID); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	connection := dialEvents(t, ctx, server.URL, helloID)
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 {
		t.Fatalf("trigger = %d, %q", status, body)
	}
	event := readEvent(t, ctx, connection)
	if event.Event != "visit" {
		t.Fatalf("event = %#v", event)
	}
	if count, _ := event.Data["count"].(float64); int(count) != 1 {
		t.Fatalf("event count = %#v", event.Data)
	}

	// Closing the page only releases that subscription; the plugin keeps running.
	if err := connection.Close(websocket.StatusNormalClosure, "page closed"); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	if status, body := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 || !strings.Contains(body, `"visits":2`) {
		t.Fatalf("plugin after page close = %d, %q", status, body)
	}

	connection = dialEvents(t, ctx, server.URL, helloID)
	if err := extensionHost.Stop(ctx, helloID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, _, err := connection.Read(readCtx)
	cancel()
	if err == nil {
		t.Fatal("websocket stayed open after the plugin stopped")
	}
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Logf("close status = %v (acceptable as long as the connection ended)", err)
	}

	if err := extensionHost.Start(ctx, helloID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	connection = dialEvents(t, ctx, server.URL, helloID)
	if status, _ := httpGet(t, server.URL+helloPath+"/api/hello"); status != 200 {
		t.Fatalf("trigger after start = %d", status)
	}
	if event := readEvent(t, ctx, connection); event.Event != "visit" {
		t.Fatalf("event after restart = %#v", event)
	}

	if err := extensionHost.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	readCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = connection.Read(readCtx)
	cancel()
	if err == nil {
		t.Fatal("websocket stayed open after the host closed")
	}
}

func TestStartFailureCanBeRetried(t *testing.T) {
	ctx := testContext(t)
	extensionHost, dataDir := newTestHost(t, ctx)
	server := startServer(t, extensionHost)
	manifest := `{
		"schemaVersion": 1,
		"id": "com.godesk.flaky",
		"name": "Flaky",
		"version": "1.0.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`
	lua := `
local marker = host.dataDir .. "/ready"
local file = io.open(marker, "r")
if not file then
  error("missing ready file: " .. marker)
end
file:close()
host.app.route("GET", "/ok", function()
  return { status = 200, body = "ok" }
end)
`
	installPackage(t, ctx, extensionHost, manifest, map[string]string{"init.lua": lua})
	if err := extensionHost.Enable(ctx, "com.godesk.flaky"); err == nil {
		t.Fatal("Enable succeeded despite a failing start hook")
	}
	plugin := getPlugin(t, extensionHost, "com.godesk.flaky")
	if !plugin.Enabled || plugin.Status != host.StatusFailed {
		t.Fatalf("failed plugin = enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
	if !strings.Contains(plugin.Error, "missing ready file") {
		t.Fatalf("error = %q", plugin.Error)
	}

	marker := filepath.Join(dataDir, "plugins", "com.godesk.flaky", "data", "ready")
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := extensionHost.Start(ctx, "com.godesk.flaky"); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	plugin = getPlugin(t, extensionHost, "com.godesk.flaky")
	if plugin.Status != host.StatusRunning {
		t.Fatalf("status after retry = %s", plugin.Status)
	}
	if status, body := httpGet(t, server.URL+"/extensions/com.godesk.flaky/api/ok"); status != 200 || body != "ok" {
		t.Fatalf("retried plugin = %d, %q", status, body)
	}
}

func TestUnauthorizedHostAPIIsExplicit(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServer(t, extensionHost)
	manifest := `{
		"schemaVersion": 1,
		"id": "com.godesk.denied",
		"name": "Denied",
		"version": "1.0.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`
	lua := `
host.app.route("GET", "/call", function()
  host.http.request("GET", "http://127.0.0.1/")
  return { status = 200, body = "unreachable" }
end)
`
	installPackage(t, ctx, extensionHost, manifest, map[string]string{"init.lua": lua})
	if err := extensionHost.Enable(ctx, "com.godesk.denied"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	status, body := httpGet(t, server.URL+"/extensions/com.godesk.denied/api/call")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, %q", status, body)
	}
	if !strings.Contains(body, "host.http.request is not authorized") {
		t.Fatalf("error = %q", body)
	}
}

func TestWebOnlyAndLuaOnlyPlugins(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServer(t, extensionHost)

	webOnly := installExample(t, ctx, extensionHost, "web-only")
	if err := extensionHost.Enable(ctx, webOnly.ID); err != nil {
		t.Fatalf("Enable web-only: %v", err)
	}
	if status, body := httpGet(t, server.URL+"/extensions/com.godesk.webonly/web/"); status != 200 || !strings.Contains(body, "Web-only") {
		t.Fatalf("web-only page = %d, %q", status, body)
	}
	status, body := httpGet(t, server.URL+"/extensions/com.godesk.webonly/api/ping")
	if status != http.StatusNotFound || !strings.Contains(body, "no Lua entry") {
		t.Fatalf("web-only API = %d, %q", status, body)
	}

	luaOnly := installExample(t, ctx, extensionHost, "lua-only")
	if err := extensionHost.Enable(ctx, luaOnly.ID); err != nil {
		t.Fatalf("Enable lua-only: %v", err)
	}
	status, body = httpGet(t, server.URL+"/extensions/com.godesk.luaonly/api/ping")
	if status != 200 || !strings.Contains(body, `"message":"pong"`) {
		t.Fatalf("lua-only API = %d, %q", status, body)
	}
	status, body = httpGet(t, server.URL+"/extensions/com.godesk.luaonly/web/")
	if status != http.StatusNotFound || !strings.Contains(body, "no web entry") {
		t.Fatalf("lua-only web = %d, %q", status, body)
	}

	connection := dialEvents(t, ctx, server.URL, luaOnly.ID)
	status = httpJSON(t, http.MethodPost, server.URL+"/extensions/com.godesk.luaonly/api/emit",
		map[string]any{"hello": "world"}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("emit = %d", status)
	}
	event := readEvent(t, ctx, connection)
	if event.Event != "message" || event.Data["hello"] != "world" {
		t.Fatalf("event = %#v", event)
	}
}

func TestInvalidPackagesAndConflicts(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)

	if _, err := extensionHost.Install(ctx, bytes.NewReader([]byte("not a zip"))); err == nil {
		t.Fatal("invalid ZIP accepted")
	}
	if _, err := extensionHost.Install(ctx, bytes.NewReader(zipFiles(t, map[string]string{
		"web/index.html": "x",
	}))); err == nil || !strings.Contains(err.Error(), "manifest.json is missing") {
		t.Fatalf("missing manifest error = %v", err)
	}
	if _, err := extensionHost.Install(ctx, bytes.NewReader(zipFiles(t, map[string]string{
		"manifest.json": `{
			"schemaVersion": 1,
			"id": "com.godesk.missing",
			"name": "Missing",
			"version": "1.0.0",
			"hostApi": {"min": 1, "max": 1},
			"entry": {"lua": "init.lua"}
		}`,
	}))); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing entry error = %v", err)
	}
	if _, err := extensionHost.Install(ctx, bytes.NewReader(zipFiles(t, map[string]string{
		"manifest.json": `{
			"schemaVersion": 1,
			"id": "com.godesk.future",
			"name": "Future",
			"version": "1.0.0",
			"hostApi": {"min": 2, "max": 2},
			"entry": {"lua": "init.lua"}
		}`,
		"init.lua": "return true",
	}))); err == nil || !strings.Contains(err.Error(), "host provides 1") {
		t.Fatalf("incompatible API error = %v", err)
	}
	if _, err := extensionHost.Install(ctx, bytes.NewReader(zipFiles(t, map[string]string{
		"manifest.json": `{
			"schemaVersion": 1,
			"id": "com.godesk.traversal",
			"name": "Traversal",
			"version": "1.0.0",
			"hostApi": {"min": 1, "max": 1},
			"entry": {"lua": "../evil.lua"}
		}`,
		"../evil.lua": "return true",
	}))); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("traversal error = %v", err)
	}

	installExample(t, ctx, extensionHost, "hello")
	if _, err := extensionHost.Install(ctx, bytes.NewReader(exampleZip(t, "hello"))); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestConfigValidationAndSessionAdapter(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServerWithOptions(t, extensionHost, host.HTTPOptions{
		RequireSession: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Test-Session") != "ok" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	installExample(t, ctx, extensionHost, "hello")

	status, _ := httpGet(t, server.URL+"/api/v1/extensions")
	if status != http.StatusUnauthorized {
		t.Fatalf("session adapter status = %d", status)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/extensions", nil)
	request.Header.Set("X-Test-Session", "ok")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("session request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("authorized status = %d", response.StatusCode)
	}

	openServer := startServer(t, extensionHost)
	status = httpJSON(t, http.MethodPut, openServer.URL+"/api/v1/extensions/"+helloID+"/config",
		map[string]any{"greeting": 3}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("wrong type status = %d", status)
	}
	status = httpJSON(t, http.MethodPut, openServer.URL+"/api/v1/extensions/"+helloID+"/config",
		map[string]any{"unknown": "x"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", status)
	}

	manifest := `{
		"schemaVersion": 1,
		"id": "com.godesk.required",
		"name": "Required",
		"version": "1.0.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"web": "index.html"},
		"config": {"apiKey": {"type": "string", "required": true, "secret": true}}
	}`
	installPackage(t, ctx, extensionHost, manifest, map[string]string{"index.html": "<html></html>"})
	status = httpJSON(t, http.MethodPut, openServer.URL+"/api/v1/extensions/com.godesk.required/config",
		map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("missing required field status = %d", status)
	}
	var plugin host.Plugin
	status = httpJSON(t, http.MethodPut, openServer.URL+"/api/v1/extensions/com.godesk.required/config",
		map[string]any{"apiKey": "value"}, &plugin)
	if status != 200 {
		t.Fatalf("required field status = %d", status)
	}
	if _, leaked := plugin.Config["apiKey"]; leaked {
		t.Fatal("required secret leaked")
	}
}

func newTestHost(t *testing.T, ctx context.Context) (*host.Host, string) {
	t.Helper()
	dataDir := t.TempDir()
	return openHost(t, ctx, dataDir), dataDir
}

func openHost(t *testing.T, ctx context.Context, dataDir string) *host.Host {
	t.Helper()
	extensionHost, err := host.Open(ctx, host.Options{
		DataDir: dataDir,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = extensionHost.Close(closeCtx)
	})
	return extensionHost
}

func startServer(t *testing.T, extensionHost *host.Host) *httptest.Server {
	t.Helper()
	return startServerWithOptions(t, extensionHost, host.HTTPOptions{})
}

func startServerWithOptions(t *testing.T, extensionHost *host.Host, opts host.HTTPOptions) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(extensionHost.Handler(opts))
	t.Cleanup(server.Close)
	return server
}

func installExample(t *testing.T, ctx context.Context, extensionHost *host.Host, name string) host.Plugin {
	t.Helper()
	plugin, err := extensionHost.Install(ctx, bytes.NewReader(exampleZip(t, name)))
	if err != nil {
		t.Fatalf("Install %s: %v", name, err)
	}
	return plugin
}

func exampleZip(t *testing.T, name string) []byte {
	t.Helper()
	pkg, err := bundle.LoadDir(filepath.Join("..", "examples", name))
	if err != nil {
		t.Fatalf("load example %s: %v", name, err)
	}
	data, err := pkg.Zip()
	if err != nil {
		t.Fatalf("pack example %s: %v", name, err)
	}
	return data
}

func installPackage(t *testing.T, ctx context.Context, extensionHost *host.Host, manifestJSON string, files map[string]string) host.Plugin {
	t.Helper()
	all := make(map[string]string, len(files)+1)
	for name, content := range files {
		all[name] = content
	}
	all["manifest.json"] = manifestJSON
	plugin, err := extensionHost.Install(ctx, bytes.NewReader(zipFiles(t, all)))
	if err != nil {
		t.Fatalf("Install package: %v", err)
	}
	return plugin
}

func zipFiles(t *testing.T, files map[string]string) []byte {
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

func listPlugins(t *testing.T, extensionHost *host.Host) []host.Plugin {
	t.Helper()
	plugins, err := extensionHost.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return plugins
}

func getPlugin(t *testing.T, extensionHost *host.Host, id string) host.Plugin {
	t.Helper()
	for _, plugin := range listPlugins(t, extensionHost) {
		if plugin.ID == id {
			return plugin
		}
	}
	t.Fatalf("plugin %s is not installed", id)
	return host.Plugin{}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response.StatusCode, string(body)
}

func httpJSON(t *testing.T, method, url string, value any, out any) int {
	t.Helper()
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
	}
	return response.StatusCode
}

type eventPayload struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

func dialEvents(t *testing.T, ctx context.Context, serverURL, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/extensions/" + id + "/events"
	connection, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	return connection
}

func readEvent(t *testing.T, ctx context.Context, connection *websocket.Conn) eventPayload {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, data, err := connection.Read(readCtx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var event eventPayload
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode event %q: %v", data, err)
	}
	return event
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
