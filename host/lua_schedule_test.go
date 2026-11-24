package host_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
)

// robfig/cron rounds @every durations below one second up to one second, so
// every schedule used by these tests first fires within about a second.
const scheduleTickTimeout = 3 * time.Second

// getScheduleNumber performs one GET through the extension handler and parses
// the body as an integer. It reports false when the handler is not serving the
// route or the body is not a number yet.
func getScheduleNumber(t *testing.T, handler http.Handler, url string) (int, bool) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
	if response.Code != http.StatusOK {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimSpace(response.Body.String()))
	if err != nil {
		return 0, false
	}
	return value, true
}

// waitForScheduleNumber polls url with fresh recorders until the served number
// is greater than above, or fails the test at the deadline.
func waitForScheduleNumber(t *testing.T, handler http.Handler, url string, above int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := 0
	for time.Now().Before(deadline) {
		if value, ok := getScheduleNumber(t, handler, url); ok {
			last = value
			if value > above {
				return value
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not exceed %d within %s (last value %d)", url, above, timeout, last)
	return 0
}

func TestLuaScheduleRunsAndSurvivesRestart(t *testing.T) {
	ctx := testContext(t)
	dataDir := t.TempDir()
	extensionHost := openHost(t, ctx, dataDir)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.schedule.restart", "name": "Schedule restart test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local function tick()
			local n = tonumber(host.storage.get("ticks") or "0") + 1
			host.storage.set("ticks", tostring(n))
		end
		host.app.on("start", function()
			assert(host.schedule.register("tick", "@every 20ms", tick))
		end)
		host.app.route("GET", "/ticks", function() return { body = host.storage.get("ticks") or "0" } end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	ticksURL := plugin.APIPath + "ticks"
	first := waitForScheduleNumber(t, handler, ticksURL, 0, scheduleTickTimeout)

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := extensionHost.Close(closeCtx); err != nil {
		cancel()
		t.Fatalf("Close: %v", err)
	}
	cancel()

	restarted := openHost(t, ctx, dataDir)
	if err := restarted.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := getPlugin(t, restarted, plugin.ID)
	if !restored.Enabled || restored.Status != host.StatusRunning {
		t.Fatalf("restored plugin = enabled %v, status %s", restored.Enabled, restored.Status)
	}
	restartedHandler := restarted.Handler(host.HTTPOptions{})
	second := waitForScheduleNumber(t, restartedHandler, restored.APIPath+"ticks", first, scheduleTickTimeout)
	if second <= first {
		t.Fatalf("ticks after restart = %d, want more than %d", second, first)
	}
}

func TestLuaScheduleRejectsInvalidExpression(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.schedule.invalid", "name": "Schedule invalid test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.route("GET", "/register", function()
			local ok, err = host.schedule.register("bad", "not a schedule", function() end)
			assert(ok == nil)
			assert(type(err) == "string" and #err > 0)
			return { body = host.json.encode({ ok = false, error = err }) }
		end)
		host.app.route("GET", "/health", function() return { body = "healthy" } end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, plugin.APIPath+"register", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("register response = %d, %q", response.Code, response.Body)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode register response %q: %v", response.Body, err)
	}
	if result.OK || result.Error == "" {
		t.Fatalf("register result = %+v, want ok=false and a non-empty error", result)
	}
	if body := response.Body.String(); !strings.Contains(body, "not a schedule") {
		t.Fatalf("register body does not include the schedule error: %q", body)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, plugin.APIPath+"health", nil))
	if health.Code != http.StatusOK || strings.TrimSpace(health.Body.String()) != "healthy" {
		t.Fatalf("health after rejected schedule = %d, %q", health.Code, health.Body)
	}
	plugin = getPlugin(t, extensionHost, plugin.ID)
	if !plugin.Enabled || plugin.Status != host.StatusRunning {
		t.Fatalf("plugin after rejected schedule = enabled %v, status %s", plugin.Enabled, plugin.Status)
	}
}

func TestLuaScheduleUnregisterStops(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.schedule.unregister", "name": "Schedule unregister test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local function tick()
			local n = tonumber(host.storage.get("count") or "0") + 1
			host.storage.set("count", tostring(n))
		end
		host.app.on("start", function()
			assert(host.schedule.register("tick", "@every 10ms", tick))
		end)
		host.app.route("GET", "/count", function() return { body = host.storage.get("count") or "0" } end)
		host.app.route("POST", "/stop-schedule", function()
			return { body = tostring(host.schedule.unregister("tick")) }
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	countURL := plugin.APIPath + "count"
	first := waitForScheduleNumber(t, handler, countURL, 0, scheduleTickTimeout)

	stop := httptest.NewRecorder()
	handler.ServeHTTP(stop, httptest.NewRequest(http.MethodPost, plugin.APIPath+"stop-schedule", nil))
	if stop.Code != http.StatusOK || strings.TrimSpace(stop.Body.String()) != "true" {
		t.Fatalf("stop-schedule response = %d, %q", stop.Code, stop.Body)
	}

	// robfig/cron rounds @every durations below one second up to one second.
	// Wait a full period so a callback triggered before the unregister lands,
	// then observe another full period.
	time.Sleep(1200 * time.Millisecond)
	baseline, ok := getScheduleNumber(t, handler, countURL)
	if !ok {
		t.Fatal("count is not readable after unregister")
	}
	if baseline < first {
		t.Fatalf("count went backwards after unregister: %d -> %d", first, baseline)
	}
	time.Sleep(1200 * time.Millisecond)
	after, ok := getScheduleNumber(t, handler, countURL)
	if !ok {
		t.Fatal("count is not readable after the settle window")
	}
	if after != baseline {
		t.Fatalf("count kept increasing after unregister: %d -> %d", baseline, after)
	}
}
