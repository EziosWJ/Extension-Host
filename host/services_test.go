package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/hostsys/services"
)

const servicesPluginID = "com.godesk.services.granted"

var servicesAPIPath = "/extensions/" + servicesPluginID + "/api"

func TestServicesGrantFiltersListAndReturnsDetail(t *testing.T) {
	ctx := testContext(t)
	mainPID := 4242
	memoryBytes := int64(8 * 1024 * 1024)
	cpuUsageNs := int64(987654321)
	backend := &fakeServiceBackend{
		services: []services.Service{
			{
				Name:        "alpha.service",
				Description: "Alpha",
				State:       services.StateRunning,
				StateDetail: "running",
				BootState:   services.BootEnabled,
				Actions:     []services.Action{services.ActionRestart},
			},
			{
				Name:        "beta.service",
				Description: "Beta",
				State:       services.StateStopped,
				StateDetail: "dead",
				BootState:   services.BootDisabled,
				Actions:     []services.Action{services.ActionStart},
			},
			{
				Name:        "gamma.service",
				Description: "Gamma",
				State:       services.StateFailed,
				StateDetail: "failed",
				BootState:   services.BootStatic,
				Actions:     []services.Action{services.ActionRestart},
			},
			{
				Name:        "delta.service",
				Description: "Delta",
				State:       services.StateStopped,
				StateDetail: "dead",
				BootState:   services.BootMasked,
			},
		},
		detail: services.ServiceDetail{
			Name:                 "alpha.service",
			State:                services.StateRunning,
			StateDetail:          "running",
			BootState:            services.BootEnabled,
			MainPID:              &mainPID,
			MemoryBytes:          &memoryBytes,
			CPUUsageNs:           &cpuUsageNs,
			ActiveEnterTimestamp: "Mon 2026-11-24 10:00:00 CST",
			FragmentPath:         "/etc/systemd/system/alpha.service",
		},
	}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service", "beta.service", "delta.service"},
			Actions: []string{"list", "get", "restart"},
			Elevate: []string{"restart"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusOK {
		t.Fatalf("list = %d, %q", status, body)
	}
	var listed []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		State       string   `json:"state"`
		StateDetail string   `json:"stateDetail"`
		BootState   string   `json:"bootState"`
		Actions     []string `json:"actions"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode list %q: %v", body, err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed = %d, %q", len(listed), body)
	}
	if listed[0].Name != "alpha.service" || listed[1].Name != "beta.service" || listed[2].Name != "delta.service" {
		t.Fatalf("listed names = %#v", listed)
	}
	if listed[0].Description != "Alpha" || listed[0].State != "running" || listed[0].BootState != "enabled" {
		t.Fatalf("alpha fields = %#v", listed[0])
	}
	if len(listed[0].Actions) != 1 || listed[0].Actions[0] != "restart" {
		t.Fatalf("alpha actions = %#v", listed[0].Actions)
	}
	if listed[2].Actions == nil || len(listed[2].Actions) != 0 {
		t.Fatalf("delta actions = %#v, want an empty array", listed[2].Actions)
	}

	status, body = httpGet(t, server.URL+servicesAPIPath+"/get?unit=alpha.service")
	if status != http.StatusOK {
		t.Fatalf("get = %d, %q", status, body)
	}
	var detail struct {
		Name                 string `json:"name"`
		State                string `json:"state"`
		StateDetail          string `json:"stateDetail"`
		BootState            string `json:"bootState"`
		MainPID              *int   `json:"mainPid"`
		MemoryBytes          *int64 `json:"memoryBytes"`
		CPUUsageNs           *int64 `json:"cpuUsageNs"`
		ActiveEnterTimestamp string `json:"activeEnterTimestamp"`
		FragmentPath         string `json:"fragmentPath"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail %q: %v", body, err)
	}
	if detail.Name != "alpha.service" || detail.State != "running" || detail.StateDetail != "running" || detail.BootState != "enabled" {
		t.Fatalf("detail = %#v", detail)
	}
	if detail.MainPID == nil || *detail.MainPID != mainPID {
		t.Fatalf("mainPid = %#v", detail.MainPID)
	}
	if detail.MemoryBytes == nil || *detail.MemoryBytes != memoryBytes {
		t.Fatalf("memoryBytes = %#v", detail.MemoryBytes)
	}
	if detail.CPUUsageNs == nil || *detail.CPUUsageNs != cpuUsageNs {
		t.Fatalf("cpuUsageNs = %#v", detail.CPUUsageNs)
	}
	if detail.ActiveEnterTimestamp != "Mon 2026-11-24 10:00:00 CST" || detail.FragmentPath != "/etc/systemd/system/alpha.service" {
		t.Fatalf("detail strings = %#v", detail)
	}
}

func TestServicesListEmptyEncodesAsArray(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service"},
			Actions: []string{"list"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusOK {
		t.Fatalf("list = %d, %q", status, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list body = %q, want []", body)
	}
}

func TestServicesRestartElevation(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		elevate []string
		want    bool
	}{
		{name: "elevated", elevate: []string{"restart"}, want: true},
		{name: "host account", want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := testContext(t)
			backend := &fakeServiceBackend{}
			extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
				servicesPluginID: {
					Units:   []string{"alpha.service"},
					Actions: []string{"restart"},
					Elevate: testCase.elevate,
				},
			})
			installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
			server := startServer(t, extensionHost)

			status, body := httpJSONStatus(t, http.MethodPost, server.URL+servicesAPIPath+"/restart?unit=alpha.service")
			if status != http.StatusOK || !strings.Contains(body, `"ok":true`) {
				t.Fatalf("restart = %d, %q", status, body)
			}
			calls := backend.restartCalls()
			if len(calls) != 1 {
				t.Fatalf("restart calls = %#v", calls)
			}
			if calls[0].unit != "alpha.service" || calls[0].elevated != testCase.want {
				t.Fatalf("restart call = %#v, want elevated %v", calls[0], testCase.want)
			}
		})
	}
}

func TestServicesDeniesUngrantedAndInvalidUnits(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		services: []services.Service{{Name: "alpha.service", State: services.StateRunning}},
	}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service"},
			Actions: []string{"list", "get", "restart"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/get?unit=beta.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.get is not authorized for unit beta.service") {
		t.Fatalf("ungranted get = %d, %q", status, body)
	}
	status, body = httpJSONStatus(t, http.MethodPost, server.URL+servicesAPIPath+"/restart?unit=beta.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.restart is not authorized for unit beta.service") {
		t.Fatalf("ungranted restart = %d, %q", status, body)
	}
	status, body = httpGet(t, server.URL+servicesAPIPath+"/get?unit=not-a-unit")
	if status != http.StatusInternalServerError || !strings.Contains(body, "invalid unit") {
		t.Fatalf("invalid unit = %d, %q", status, body)
	}
	if calls := backend.restartCalls(); len(calls) != 0 {
		t.Fatalf("restart calls = %#v", calls)
	}
}

func TestServicesMissingGrantIsDenied(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		services: []services.Service{{Name: "alpha.service"}},
	}
	extensionHost := openHostWithServices(t, ctx, backend, nil)
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.list is not authorized: no capability grant is configured") {
		t.Fatalf("list = %d, %q", status, body)
	}
	status, body = httpGet(t, server.URL+servicesAPIPath+"/get?unit=alpha.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.get is not authorized: no capability grant is configured") {
		t.Fatalf("get = %d, %q", status, body)
	}
}

func TestServicesListRequiresListPermission(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		services: []services.Service{{Name: "alpha.service", State: services.StateRunning}},
		detail:   services.ServiceDetail{Name: "alpha.service", State: services.StateRunning},
	}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service"},
			Actions: []string{"get"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.list is not authorized: no service grant is configured") {
		t.Fatalf("list = %d, %q", status, body)
	}
	status, body = httpGet(t, server.URL+servicesAPIPath+"/get?unit=alpha.service")
	if status != http.StatusOK || !strings.Contains(body, `"name":"alpha.service"`) {
		t.Fatalf("get = %d, %q", status, body)
	}
}

func TestServicesRequiresManifestCapability(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		services: []services.Service{{Name: "alpha.service"}},
		detail:   services.ServiceDetail{Name: "alpha.service"},
	}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service"},
			Actions: []string{"list", "get", "restart"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, nil)
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.list is not authorized: no capability grant is configured") {
		t.Fatalf("list = %d, %q", status, body)
	}
	status, body = httpJSONStatus(t, http.MethodPost, server.URL+servicesAPIPath+"/restart?unit=alpha.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.restart is not authorized: no capability grant is configured") {
		t.Fatalf("restart = %d, %q", status, body)
	}
	if calls := backend.restartCalls(); len(calls) != 0 {
		t.Fatalf("restart calls = %#v", calls)
	}
}

func TestServicesBackendErrorsSurfaceWithCode(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		listErr:    &services.Error{Code: "operation_failed", Message: "操作失败"},
		detailErr:  &services.Error{Code: "not_found", Message: "服务不存在"},
		restartErr: &services.Error{Code: "permission", Message: "需要权限"},
	}
	extensionHost := openHostWithServices(t, ctx, backend, map[string]host.ServiceGrant{
		servicesPluginID: {
			Units:   []string{"alpha.service"},
			Actions: []string{"list", "get", "restart"},
		},
	})
	installServicesPlugin(t, ctx, extensionHost, servicesPluginID, []string{"services"})
	server := startServer(t, extensionHost)

	status, body := httpGet(t, server.URL+servicesAPIPath+"/list")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.list: operation_failed: 操作失败") {
		t.Fatalf("list = %d, %q", status, body)
	}
	status, body = httpGet(t, server.URL+servicesAPIPath+"/get?unit=alpha.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.get: not_found: 服务不存在") {
		t.Fatalf("get = %d, %q", status, body)
	}
	status, body = httpJSONStatus(t, http.MethodPost, server.URL+servicesAPIPath+"/restart?unit=alpha.service")
	if status != http.StatusInternalServerError || !strings.Contains(body, "host.services.restart: permission: 需要权限") {
		t.Fatalf("restart = %d, %q", status, body)
	}
}

func TestServiceGrantValidation(t *testing.T) {
	ctx := testContext(t)
	testCases := []struct {
		name   string
		grants map[string]host.ServiceGrant
		want   string
	}{
		{
			name:   "empty plugin id",
			grants: map[string]host.ServiceGrant{"": {Units: []string{"alpha.service"}, Actions: []string{"list"}}},
			want:   "empty plugin id",
		},
		{
			name:   "invalid unit",
			grants: map[string]host.ServiceGrant{"com.godesk.bad": {Units: []string{"alpha"}, Actions: []string{"list"}}},
			want:   `invalid unit "alpha"`,
		},
		{
			name:   "option-like unit",
			grants: map[string]host.ServiceGrant{"com.godesk.bad": {Units: []string{"-alpha.service"}, Actions: []string{"list"}}},
			want:   `invalid unit "-alpha.service"`,
		},
		{
			name:   "invalid action",
			grants: map[string]host.ServiceGrant{"com.godesk.bad": {Units: []string{"alpha.service"}, Actions: []string{"stop"}}},
			want:   `invalid action "stop"`,
		},
		{
			name: "elevate action not granted",
			grants: map[string]host.ServiceGrant{
				"com.godesk.bad": {Units: []string{"alpha.service"}, Actions: []string{"list"}, Elevate: []string{"restart"}},
			},
			want: `elevate action "restart" is not granted`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			extensionHost, err := host.Open(ctx, host.Options{
				DataDir:       t.TempDir(),
				ServiceGrants: testCase.grants,
			})
			if err == nil {
				_ = extensionHost.Close(ctx)
				t.Fatal("Open accepted an invalid service grant")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %q, want substring %q", err, testCase.want)
			}
		})
	}

	valid := openHostWithServices(t, ctx, &fakeServiceBackend{}, map[string]host.ServiceGrant{
		"com.godesk.valid": {
			Units:   []string{"alpha.service", "beta@instance.service"},
			Actions: []string{"list", "get", "restart"},
			Elevate: []string{"restart"},
		},
	})
	if valid == nil {
		t.Fatal("Open rejected a valid service grant")
	}
}

func TestLoadServiceGrantsFile(t *testing.T) {
	if grants, err := host.LoadServiceGrantsFile(""); err != nil || grants != nil {
		t.Fatalf("empty path = %#v, %v", grants, err)
	}

	path := filepath.Join(t.TempDir(), "grants.json")
	content := `{
		"com.godesk.hello": {
			"units": ["ssh.service", "cron.service"],
			"actions": ["list", "get"],
			"elevate": ["get"]
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write grants: %v", err)
	}
	grants, err := host.LoadServiceGrantsFile(path)
	if err != nil {
		t.Fatalf("LoadServiceGrantsFile: %v", err)
	}
	grant, ok := grants["com.godesk.hello"]
	if !ok {
		t.Fatalf("grants = %#v", grants)
	}
	if len(grant.Units) != 2 || grant.Units[0] != "ssh.service" || grant.Units[1] != "cron.service" {
		t.Fatalf("units = %#v", grant.Units)
	}
	if len(grant.Actions) != 2 || grant.Actions[0] != "list" || grant.Actions[1] != "get" {
		t.Fatalf("actions = %#v", grant.Actions)
	}
	if len(grant.Elevate) != 1 || grant.Elevate[0] != "get" {
		t.Fatalf("elevate = %#v", grant.Elevate)
	}

	if _, err := host.LoadServiceGrantsFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing grants file was accepted")
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatalf("write broken grants: %v", err)
	}
	if _, err := host.LoadServiceGrantsFile(broken); err == nil {
		t.Fatal("malformed grants file was accepted")
	}
}

type fakeRestartCall struct {
	unit     string
	elevated bool
}

type fakeServiceBackend struct {
	services   []services.Service
	detail     services.ServiceDetail
	listErr    error
	detailErr  error
	restartErr error

	mu       sync.Mutex
	restarts []fakeRestartCall
}

func (f *fakeServiceBackend) List(context.Context) ([]services.Service, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.services, nil
}

func (f *fakeServiceBackend) Detail(context.Context, string) (services.ServiceDetail, error) {
	if f.detailErr != nil {
		return services.ServiceDetail{}, f.detailErr
	}
	return f.detail, nil
}

func (f *fakeServiceBackend) Restart(_ context.Context, unit string, elevated bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, fakeRestartCall{unit: unit, elevated: elevated})
	return f.restartErr
}

func (f *fakeServiceBackend) restartCalls() []fakeRestartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRestartCall(nil), f.restarts...)
}

func openHostWithServices(t *testing.T, ctx context.Context, backend services.PluginBackend, grants map[string]host.ServiceGrant) *host.Host {
	t.Helper()
	extensionHost, err := host.Open(ctx, host.Options{
		DataDir:        t.TempDir(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServiceGrants:  grants,
		ServiceBackend: backend,
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

const servicesTestLua = `
local function reply(value)
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode(value),
  }
end

host.app.route("GET", "/list", function()
  return reply(host.services.list())
end)

host.app.route("GET", "/get", function(request)
  return reply(host.services.get(request.query.unit))
end)

host.app.route("POST", "/restart", function(request)
  host.services.restart(request.query.unit)
  return reply({ ok = true })
end)
`

func installServicesPlugin(t *testing.T, ctx context.Context, extensionHost *host.Host, id string, capabilities []string) {
	t.Helper()
	capabilitiesJSON, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatalf("encode capabilities: %v", err)
	}
	manifest := fmt.Sprintf(`{
		"schemaVersion": 1,
		"id": %q,
		"name": "Services Test",
		"version": "0.1.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"},
		"capabilities": %s
	}`, id, capabilitiesJSON)
	installPackage(t, ctx, extensionHost, manifest, map[string]string{"init.lua": servicesTestLua})
	if err := extensionHost.Enable(ctx, id); err != nil {
		t.Fatalf("Enable %s: %v", id, err)
	}
}

func httpJSONStatus(t *testing.T, method, url string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response.StatusCode, string(body)
}
