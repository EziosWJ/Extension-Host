//go:build linux && (amd64 || arm64)

package host_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/hostsys/services"
)

const (
	nativeProvider = "com.example.provider"
	nativeConsumer = "com.example.consumer"
)

func TestNativePluginLifecycleAndCalls(t *testing.T) {
	ctx := context.Background()
	executable := buildNativeFixture(t)
	dataDir := t.TempDir()
	backend := &fakeServiceBackend{
		services: []services.Service{{Name: "alpha.service"}, {Name: "beta.service"}},
		detail:   services.ServiceDetail{Name: "alpha.service"},
	}
	grants := map[string]host.ServiceGrant{
		nativeConsumer: {
			Units: []string{"alpha.service"}, Actions: []string{"list", "get", "restart"}, Elevate: []string{"restart"},
		},
	}
	open := func() *host.Host {
		t.Helper()
		h, err := host.Open(ctx, host.Options{DataDir: dataDir, ServiceBackend: backend, ServiceGrants: grants})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close(ctx) })
		return h
	}
	h := open()
	provider := nativeZip(t, executable, nativeProvider, host.Version, []string{"v1.getStatus"}, false, nil)
	consumer := nativeZip(t, executable, nativeConsumer, host.Version, nil, true, map[string]any{
		"private": map[string]any{"type": "string", "secret": true, "default": "test-secret"},
	})
	for _, archive := range [][]byte{consumer, provider} {
		view, err := h.Install(ctx, bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		if !view.HasNative || view.HasLua || view.EventsPath != "" || view.Status != host.StatusStopped || view.Enabled {
			t.Fatalf("unexpected installed view: %+v", view)
		}
		if _, ok := view.Config["private"]; ok {
			t.Fatal("secret escaped into management view")
		}
		if err := h.Enable(ctx, view.ID); err != nil {
			t.Fatal(err)
		}
	}
	mode, err := os.Stat(filepath.Join(dataDir, "packages", nativeProvider, "native", "fixture"))
	if err != nil || mode.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary was not extracted as executable: %v, %v", mode, err)
	}

	source := readNativeConnection(t, dataDir, nativeConsumer)
	target := readNativeConnection(t, dataDir, nativeProvider)
	if source.Socket != target.Socket || source.Token == target.Token {
		t.Fatal("instances must share a socket and use separate credentials")
	}
	assertNativeRequest(t, source, "GET", "/v1/context", "", 200, "test-secret")
	result := assertNativeRequest(t, source, "GET", "/v1/services", "", 200, "alpha.service")
	if strings.Contains(result, "beta.service") {
		t.Fatalf("unexpected service in list: %s", result)
	}
	assertNativeRequest(t, source, "GET", "/v1/services/alpha.service", "", 200, "alpha.service")
	assertNativeRequest(t, source, "GET", "/v1/services/beta.service", "", 403, "forbidden")
	assertNativeRequest(t, source, "POST", "/v1/services/beta.service/restart", "", 403, "forbidden")
	assertNativeRequest(t, source, "POST", "/v1/services/alpha.service/restart", "", 204, "")
	assertNativeRequest(t, target, "GET", "/v1/services", "", 403, "forbidden")
	if calls := backend.restartCalls(); len(calls) != 1 || calls[0] != (fakeRestartCall{unit: "alpha.service", elevated: true}) {
		t.Fatalf("restart calls: %+v", calls)
	}
	base := "/v1/plugins/" + nativeProvider + "/methods/"
	assertNativeRequest(t, source, "POST", base+"missing", "{}", 404, "method_not_found")
	assertNativeRequest(t, source, "POST", base+"v1.getStatus", "{", 400, "invalid_json")
	assertNativeRequest(t, source, "POST", base+"v1.getStatus", "{}", 200, nativeConsumer)
	assertNativeRequest(t, source, "POST", base+"v1.getStatus", `{"slow":true}`, 504, "timeout")

	handler := h.Handler(host.HTTPOptions{RequireSession: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Cookie") != "session=valid" {
				http.Error(w, "session required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	browser := func(path string, authenticated bool, status int, expected string) {
		t.Helper()
		request := httptest.NewRequest("GET", "/extensions/"+nativeConsumer+"/api/"+path, nil)
		request.Header.Set("X-Godesk-Caller", "browser-supplied")
		if authenticated {
			request.Header.Set("Cookie", "session=valid")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status || !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("web API %s = %d %s", path, response.Code, response.Body.String())
		}
	}
	browser("status", false, 401, "session required")
	browser("status", true, 200, nativeConsumer)
	browser("details/a%2Fb?q=test", true, 200, `"caller":"","cookie":"","item":"a/b","query":"test"`)
	browser("%2e%2e/rpc/v1.getStatus", true, 400, "invalid_path")

	if err := h.Stop(ctx, nativeProvider); err != nil {
		t.Fatal(err)
	}
	browser("status", true, 503, "plugin_unavailable")
	assertNativeRequest(t, target, "GET", "/v1/context", "", 401, "unauthorized")
	if err := h.Start(ctx, nativeProvider); err != nil {
		t.Fatal(err)
	}
	restarted := readNativeConnection(t, dataDir, nativeProvider)
	if restarted.Token == target.Token {
		t.Fatal("the restarted instance reused its token")
	}
	browser("status", true, 200, nativeConsumer)

	if err := syscall.Kill(restarted.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForNativeStatus(t, h, nativeProvider, host.StatusFailed)
	browser("status", true, 503, "plugin_unavailable")
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}

	h = open()
	if err := h.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	waitForNativeStatus(t, h, nativeProvider, host.StatusRunning)
	waitForNativeStatus(t, h, nativeConsumer, host.StatusRunning)
	source = readNativeConnection(t, dataDir, nativeConsumer)
	assertNativeRequest(t, source, "POST", base+"v1.getStatus", "{}", 200, nativeConsumer)

	if err := h.Disable(ctx, nativeProvider); err != nil {
		t.Fatal(err)
	}
	if err := h.Uninstall(ctx, nativeConsumer, host.UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	assertNativeRequest(t, source, "GET", "/v1/context", "", 401, "unauthorized")
}

func TestNativePluginRejectsIncompatiblePackageAndFailedStart(t *testing.T) {
	ctx := context.Background()
	h := openHost(t, ctx, t.TempDir())
	executable := buildNativeFixture(t)
	future := nativeZip(t, executable, nativeProvider, "99.0.0", nil, false, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/v1/extensions", bytes.NewReader(future))
	request.Header.Set("Content-Type", "application/zip")
	h.Handler(host.HTTPOptions{}).ServeHTTP(response, request)
	var problem struct{ Error string }
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil || response.Code != 400 ||
		!strings.Contains(problem.Error, "requires Extension Host >= 99.0.0") {
		t.Fatalf("future minimum = %d %s", response.Code, response.Body)
	}
	if _, ok := h.Get(nativeProvider); ok {
		t.Fatal("incompatible package was installed")
	}

	for _, test := range []struct {
		id, flag, message string
		timeout           time.Duration
	}{
		{nativeProvider, "earlyExit", "exit status 7", 10 * time.Second},
		{nativeConsumer, "holdReady", "did not report ready", 300 * time.Millisecond},
	} {
		config := map[string]any{test.flag: map[string]any{"type": "boolean", "default": true}}
		archive := nativeZip(t, executable, test.id, host.Version, nil, false, config)
		if _, err := h.Install(ctx, bytes.NewReader(archive)); err != nil {
			t.Fatal(err)
		}
		start, cancel := context.WithTimeout(ctx, test.timeout)
		err := h.Enable(start, test.id)
		cancel()
		if err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("enable %s: %v", test.id, err)
		}
		view := waitForNativeStatus(t, h, test.id, host.StatusFailed)
		if !view.Enabled || !strings.Contains(view.Error, test.message) {
			t.Fatalf("failed startup did not preserve enable intent or error: %+v", view)
		}
	}
}

type nativeConnection struct {
	Socket string
	Token  string
	PID    int
}

func readNativeConnection(t *testing.T, dir, id string) nativeConnection {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "plugins", id, "data", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info nativeConnection
	if err := json.Unmarshal(data, &info); err != nil || info.Socket == "" || info.Token == "" || info.PID <= 1 {
		t.Fatalf("invalid native fixture connection: %v", err)
	}
	return info
}

func assertNativeRequest(t *testing.T, info nativeConnection, method, path, payload string, status int, expected string) string {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", info.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second}
	request, err := http.NewRequest(method, "http://host"+path, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+info.Token)
	result, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	if err != nil || result.StatusCode != status || !strings.Contains(string(data), expected) {
		t.Fatalf("%s %s = %d, %s, %v", method, path, result.StatusCode, data, err)
	}
	return string(data)
}

func waitForNativeStatus(t *testing.T, h *host.Host, id string, status host.Status) host.Plugin {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		view, _ := h.Get(id)
		if view.Status == status {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin %s has status %s: %s, want %s", id, view.Status, view.Error, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func buildNativeFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture")
	command := exec.Command("go", "build", "-o", path, "./testdata/native")
	command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func nativeZip(t *testing.T, binary []byte, id, version string, methods []string, web bool, config map[string]any) []byte {
	t.Helper()
	entry := map[string]string{"native": "native/fixture"}
	metadata := map[string]any{
		"schemaVersion": 1, "id": id, "name": id, "version": "0.1.0", "minHostVersion": version,
		"hostApi": map[string]int{"min": 1, "max": 1}, "entry": entry, "capabilities": []string{"services"},
		"native": map[string]any{"platform": "linux-" + runtime.GOARCH, "exports": methods, "webApi": web}, "config": config,
	}
	files := map[string][]byte{"native/fixture": binary}
	if web {
		entry["web"] = "web/index.html"
		files["web/index.html"] = []byte("<!doctype html><title>Native test fixture</title>")
	}
	files["manifest.json"], _ = json.Marshal(metadata)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, data := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
