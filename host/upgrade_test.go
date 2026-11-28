package host_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
)

const (
	upgradeID   = "com.godesk.upgrade"
	upgradePath = "/api/v1/extensions/" + upgradeID
)

func TestUpgradeRunningPlugin(t *testing.T) {
	ctx := testContext(t)
	h, dataDir := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	server := startServer(t, h)
	if status := httpJSON(t, http.MethodPut, server.URL+upgradePath+"/config",
		map[string]any{"greeting": "saved", "token": "retained-value"}, nil); status != http.StatusOK {
		t.Fatalf("configure = %d", status)
	}
	if err := h.Enable(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if status, body := httpGet(t, server.URL+old.APIPath+"seed"); status != http.StatusOK || body != "seeded" {
		t.Fatalf("seed = %d %q", status, body)
	}
	connection := dialEvents(t, ctx, server.URL, old.ID)

	handler := h.Handler(host.HTTPOptions{})
	newPackage := upgradeArchive(t, old.ID, "3.0.0", "New", upgradeScript("new"), nil)
	conflict := uploadPlugin(handler, "/api/v1/extensions", newPackage)
	checkExtensionError(t, conflict, http.StatusConflict, "already_installed")
	var conflictBody struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &conflictBody); err != nil || conflictBody.PluginID != old.ID {
		t.Fatalf("conflict = %s", conflict.Body)
	}

	response := uploadPlugin(handler, upgradePath+"/upgrade", newPackage)
	plugin := decodeUpgrade(t, response)
	if plugin.Version != "3.0.0" || plugin.Name != "New" || !plugin.Enabled || plugin.Status != host.StatusRunning ||
		!plugin.InstalledAt.Equal(old.InstalledAt) || plugin.Config["greeting"] != "saved" || plugin.LogPath == old.LogPath {
		t.Fatalf("upgraded plugin = %+v", plugin)
	}
	if _, exists := plugin.Config["token"]; exists {
		t.Fatal("secret appeared in upgrade response")
	}
	if _, _, err := connection.Read(ctx); err == nil {
		t.Fatal("old event subscription stayed open")
	}
	if status, body := httpGet(t, server.URL+plugin.APIPath+"state"); status != http.StatusOK || body != "new:saved:secret-ok:business-data:old:old" {
		t.Fatalf("new instance = %d %q", status, body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "packages", old.ID)); !os.IsNotExist(err) {
		t.Fatalf("previous package still exists: %v", err)
	}

	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened := openHost(t, ctx, dataDir)
	if err := reopened.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	restored := getPlugin(t, reopened, old.ID)
	if restored.Version != plugin.Version || restored.Status != host.StatusRunning || restored.Config["greeting"] != "saved" {
		t.Fatalf("restored plugin = %+v", restored)
	}
	reopenedServer := startServer(t, reopened)
	if status, body := httpGet(t, reopenedServer.URL+restored.APIPath+"state"); status != http.StatusOK || body != "new:saved:secret-ok:business-data:new:new" {
		t.Fatalf("restored instance = %d %q", status, body)
	}
}

func TestUpgradeDisabledPluginAllowsSameVersionAndDowngrade(t *testing.T) {
	ctx := testContext(t)
	h, _ := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	server := startServer(t, h)

	for _, version := range []string{old.Version, "1.0.0"} {
		archive := upgradeArchive(t, old.ID, version, "Replacement "+version, upgradeScript(version), nil)
		plugin, err := h.Upgrade(ctx, old.ID, bytes.NewReader(archive))
		if err != nil {
			t.Fatalf("Upgrade: %v", err)
		}
		if plugin.Enabled || plugin.Status != host.StatusStopped || plugin.StartedAt != nil || plugin.Version != version ||
			plugin.Name != "Replacement "+version {
			t.Fatalf("upgraded disabled plugin = %+v", plugin)
		}
		if _, err := os.Stat(plugin.LogPath); !os.IsNotExist(err) {
			t.Fatalf("disabled plugin log exists: %v", err)
		}
		if err := h.Enable(ctx, old.ID); err != nil {
			t.Fatal(err)
		}
		if status, body := httpGet(t, server.URL+old.APIPath+"state"); status != http.StatusOK || !strings.HasPrefix(body, version+":") {
			t.Fatalf("replacement %s = %d %q", version, status, body)
		}
		if err := h.Disable(ctx, old.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpgradeRetainsSecretRedactionAcrossSchemaChanges(t *testing.T) {
	ctx := testContext(t)
	h, dataDir := newTestHost(t, ctx)
	installUpgradeFixture(t, ctx, h)
	handler := h.Handler(host.HTTPOptions{})
	const secret = "retained-value"
	update := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, upgradePath+"/config", strings.NewReader(body)))
		return response
	}
	checkRedacted := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) {
			t.Fatalf("management response status = %d, saved secret present = %t",
				response.Code, strings.Contains(response.Body.String(), secret))
		}
	}
	checkViews := func() {
		t.Helper()
		for _, path := range []string{upgradePath, "/api/v1/extensions"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			checkRedacted(response)
		}
	}
	checkRedacted(update(`{"token":"retained-value"}`))
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Model an installation written before sensitive keys had a dedicated column.
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE plugins DROP COLUMN sensitive_keys`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("prepare legacy database: %v, close: %v", err, closeErr)
	}
	h = openHost(t, ctx, dataDir)
	handler = h.Handler(host.HTTPOptions{})
	checkViews()

	replacement := func(version, tokenField string) []byte {
		return upgradeArchive(t, upgradeID, version, "Replacement", upgradeScript("new"), map[string]string{
			"manifest.json": fmt.Sprintf(`{"schemaVersion":1,"id":%q,"name":"Replacement","version":%q,
				"hostApi":{"min":1,"max":1},"entry":{"lua":"init.lua"},
				"config":{"greeting":{"type":"string"}%s}}`, upgradeID, version, tokenField),
		})
	}
	ordinary := `,"token":{"type":"string","required":true}`
	for _, archive := range [][]byte{replacement("3.0.0", ordinary), replacement("2.0.0", ""), replacement("1.0.0", ordinary)} {
		checkRedacted(uploadPlugin(handler, upgradePath+"/upgrade", archive))
		checkViews()
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	h = openHost(t, ctx, dataDir)
	handler = h.Handler(host.HTTPOptions{})
	checkViews()
	if err := h.Enable(ctx, upgradeID); err != nil {
		t.Fatal(err)
	}
	checkState := func(want string) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/extensions/"+upgradeID+"/api/state", nil))
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("instance state = %d %q, want %q", response.Code, response.Body.String(), want)
		}
	}
	checkState("new:default:secret-ok:missing:missing:missing")
	saved := update(`{"greeting":"changed"}`)
	checkRedacted(saved)
	savedPlugin := decodeUpgrade(t, saved)
	if _, visible := savedPlugin.Config["token"]; visible || !savedPlugin.RedactedConfig["token"] {
		t.Fatal("saving greeting did not retain the token's redaction marker")
	}
	checkViews()
	if err := h.Stop(ctx, upgradeID); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx, upgradeID); err != nil {
		t.Fatal(err)
	}
	checkState("new:changed:secret-ok:missing:new:new")

	var updated host.Plugin
	response := update(`{"token":"ordinary-value"}`)
	checkRedacted(response)
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil || updated.Config["token"] != "ordinary-value" || updated.RedactedConfig["token"] {
		t.Fatalf("explicit overwrite did not clear redaction: %v", err)
	}
}

func TestUpgradeRollbackAfterPublicationFailure(t *testing.T) {
	ctx := testContext(t)
	h, dataDir := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	server := startServer(t, h)
	if status := httpJSON(t, http.MethodPut, server.URL+upgradePath+"/config",
		map[string]any{"greeting": "saved", "token": "retained-value"}, nil); status != http.StatusOK {
		t.Fatalf("configure = %d", status)
	}
	if err := h.Enable(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	running := getPlugin(t, h, old.ID)

	db, err := sql.Open("sqlite", filepath.Join(dataDir, "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Fail after the metadata update, when the transaction publishes the directory.
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_upgrade BEFORE INSERT ON development_packages
		BEGIN SELECT RAISE(ABORT, 'injected package publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	archive := upgradeArchive(t, old.ID, "3.0.0", "Unpublished", upgradeScript("new"), nil)
	response := uploadPlugin(h.Handler(host.HTTPOptions{}), upgradePath+"/upgrade", archive)
	checkExtensionError(t, response, http.StatusInternalServerError, "internal_error")
	if !strings.Contains(response.Body.String(), "injected package publication failure") {
		t.Fatalf("upgrade did not reach publication: %s", response.Body)
	}
	current := getPlugin(t, h, old.ID)
	if running.StartedAt == nil || current.StartedAt == nil || !current.StartedAt.After(*running.StartedAt) {
		t.Fatal("previous instance was not restarted")
	}
	running.StartedAt = current.StartedAt
	if !reflect.DeepEqual(current, running) {
		t.Fatalf("rollback changed plugin metadata: %+v", current)
	}
	// The old startup hook can see the previous instance's stop marker.
	if status, body := httpGet(t, server.URL+old.APIPath+"state"); status != http.StatusOK || body != "old:saved:secret-ok:missing:old:old" {
		t.Fatalf("restarted instance = %d %q", status, body)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "packages"))
	if err != nil || len(entries) != 1 || entries[0].Name() != old.ID {
		t.Fatalf("package directories after rollback = %v, %v", entries, err)
	}

	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened := openHost(t, ctx, dataDir)
	if err := reopened.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	restored := getPlugin(t, reopened, old.ID)
	running.StartedAt = restored.StartedAt
	if !reflect.DeepEqual(restored, running) {
		t.Fatalf("rollback changed persisted metadata: %+v", restored)
	}
	reopenedServer := startServer(t, reopened)
	if status, body := httpGet(t, reopenedServer.URL+old.APIPath+"state"); status != http.StatusOK || body != "old:saved:secret-ok:missing:old:old" {
		t.Fatalf("reopened instance = %d %q", status, body)
	}
}

func TestUpgradeInstallConflictPrecedesHostAPICompatibility(t *testing.T) {
	ctx := testContext(t)
	h, _ := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	handler := h.Handler(host.HTTPOptions{})
	files := map[string]string{
		"manifest.json": fmt.Sprintf(`{"schemaVersion":1,"id":%q,"name":"Future","version":"3.0.0",
			"hostApi":{"min":200,"max":200},"entry":{"lua":"init.lua"}}`, old.ID),
		"init.lua": upgradeScript("future"),
	}
	archive := zipFiles(t, files)
	response := uploadPlugin(handler, "/api/v1/extensions", archive)
	checkExtensionError(t, response, http.StatusConflict, "already_installed")
	var conflict struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &conflict); err != nil || conflict.PluginID != old.ID {
		t.Fatalf("conflict = %s", response.Body)
	}
	checkExtensionError(t, uploadPlugin(handler, upgradePath+"/upgrade", archive), http.StatusBadRequest, "incompatible_package")

	delete(files, "init.lua")
	checkExtensionError(t, uploadPlugin(handler, "/api/v1/extensions", zipFiles(t, files)), http.StatusBadRequest, "invalid_package")
	files["init.lua"] = upgradeScript("future")
	files["manifest.json"] = strings.Replace(files["manifest.json"], old.ID, "future", 1)
	checkExtensionError(t, uploadPlugin(handler, "/api/v1/extensions", zipFiles(t, files)), http.StatusBadRequest, "incompatible_package")
}

func TestRejectedUpgradeLeavesRunningInstanceAndPackageIntact(t *testing.T) {
	ctx := testContext(t)
	h, dataDir := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	if err := h.Enable(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	running := getPlugin(t, h, old.ID)
	handler := h.Handler(host.HTTPOptions{})
	server := startServer(t, h)
	path := filepath.Join(dataDir, "packages", old.ID, "manifest.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		target  string
		archive []byte
		status  int
		code    string
	}{
		{"corrupt ZIP", upgradePath, []byte("broken"), 400, "invalid_package"},
		{"invalid manifest", upgradePath, zipFiles(t, map[string]string{"manifest.json": "{}"}), 400, "invalid_package"},
		{"invalid structure", upgradePath, upgradeArchive(t, old.ID, "3.0.0", "Bad", upgradeScript("bad"), map[string]string{"file": "a", "file/child": "b"}), 400, "invalid_package"},
		{"incompatible host API", upgradePath, zipFiles(t, map[string]string{
			"manifest.json": strings.ReplaceAll(string(original), `"min":1,"max":1`, `"min":200,"max":200`),
			"init.lua":      upgradeScript("bad"),
		}), 400, "incompatible_package"},
		{"ID mismatch", upgradePath, upgradeArchive(t, "other", "3.0.0", "Bad", upgradeScript("bad"), nil), 400, "id_mismatch"},
		{"unknown target", "/api/v1/extensions/other", upgradeArchive(t, "other", "3.0.0", "Bad", upgradeScript("bad"), nil), 404, "not_found"},
		{"extraction failure", upgradePath, upgradeArchive(t, old.ID, "3.0.0", "Bad", upgradeScript("bad"), map[string]string{strings.Repeat("x", 300): "too long"}), 500, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := uploadPlugin(handler, tc.target+"/upgrade", tc.archive)
			checkExtensionError(t, response, tc.status, tc.code)
			if current := getPlugin(t, h, old.ID); !reflect.DeepEqual(current, running) {
				t.Fatalf("upgrade changed running instance: %+v", current)
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatalf("upgrade changed old manifest: %s, %v", current, err)
			}
			if status, body := httpGet(t, server.URL+old.APIPath+"state"); status != http.StatusOK || body != "old:default:secret-missing:missing:missing:missing" {
				t.Fatalf("previous instance = %d %q", status, body)
			}
			entries, err := os.ReadDir(filepath.Dir(filepath.Dir(path)))
			if err != nil || len(entries) != 1 || entries[0].Name() != old.ID {
				t.Fatalf("package directories after rejection = %v, %v", entries, err)
			}
		})
	}
}

func TestUpgradeRecordsStartupFailureAndKeepsEnableIntent(t *testing.T) {
	ctx := testContext(t)
	h, _ := newTestHost(t, ctx)
	old := installUpgradeFixture(t, ctx, h)
	if err := h.Enable(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	handler := h.Handler(host.HTTPOptions{})
	broken := upgradeArchive(t, old.ID, "3.0.0", "Broken", `error("startup failed")`, nil)
	plugin := decodeUpgrade(t, uploadPlugin(handler, upgradePath+"/upgrade", broken))
	if !plugin.Enabled || plugin.Version != "3.0.0" || plugin.Status != host.StatusFailed || !strings.Contains(plugin.Error, "startup failed") {
		t.Fatalf("failed upgrade state = %+v", plugin)
	}
	fixed := upgradeArchive(t, old.ID, plugin.Version, "Fixed", upgradeScript("fixed"), nil)
	plugin = decodeUpgrade(t, uploadPlugin(handler, upgradePath+"/upgrade", fixed))
	if !plugin.Enabled || plugin.Status != host.StatusRunning || plugin.Error != "" {
		t.Fatalf("recovered upgrade state = %+v", plugin)
	}
}

func installUpgradeFixture(t *testing.T, ctx context.Context, h *host.Host) host.Plugin {
	t.Helper()
	archive := upgradeArchive(t, upgradeID, "2.0.0", "Old", upgradeScript("old"), nil)
	plugin, err := h.Install(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

func upgradeArchive(t *testing.T, id, version, name, lua string, extra map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"manifest.json": fmt.Sprintf(`{
			"schemaVersion":1,"id":%q,"name":%q,"version":%q,
			"hostApi":{"min":1,"max":1},"entry":{"lua":"init.lua"},
			"config":{"greeting":{"type":"string","default":"default"},"token":{"type":"string","secret":true}}
		}`, id, name, version),
		"init.lua": lua,
	}
	for path, content := range extra {
		files[path] = content
	}
	return zipFiles(t, files)
}

func upgradeScript(label string) string {
	return fmt.Sprintf(`
local label = %q
local app, storage = host.app, host.storage
local stoppedBeforeStart
app.on("start", function()
  stoppedBeforeStart = storage.get("stopped") or "missing"
end)
app.on("stop", function()
  storage.set("stopped", label)
end)
app.route("GET", "/seed", function()
  storage.set("business", "business-data")
  return {body = "seeded"}
end)
app.route("GET", "/state", function()
  local token = host.config.get("token") == "retained-value" and "secret-ok" or "secret-missing"
  local greeting = host.config.get("greeting")
  return {body = table.concat({label, greeting, token, storage.get("business") or "missing",
    storage.get("stopped") or "missing", stoppedBeforeStart}, ":")}
end)
`, label)
}

func uploadPlugin(handler http.Handler, url string, archive []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(archive))
	request.Header.Set("Content-Type", "application/zip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeUpgrade(t *testing.T, response *httptest.ResponseRecorder) host.Plugin {
	t.Helper()
	var plugin host.Plugin
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &plugin) != nil {
		t.Fatalf("upgrade = %d %s", response.Code, response.Body)
	}
	return plugin
}
