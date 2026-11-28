package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/hostsys/services"
)

const servicesPluginID = "com.example.services.test"

var servicesAPIPath = "/extensions/" + servicesPluginID + "/api"

func TestServicesListAndDetail(t *testing.T) {
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
	extensionHost := openHostWithServices(t, ctx, backend)
	installServicesPlugin(t, ctx, extensionHost)
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
	if len(listed) != 4 {
		t.Fatalf("listed = %d, %q", len(listed), body)
	}
	if listed[0].Name != "alpha.service" || listed[1].Name != "beta.service" || listed[2].Name != "gamma.service" || listed[3].Name != "delta.service" {
		t.Fatalf("listed names = %#v", listed)
	}
	if listed[0].Description != "Alpha" || listed[0].State != "running" || listed[0].BootState != "enabled" {
		t.Fatalf("alpha fields = %#v", listed[0])
	}
	if len(listed[0].Actions) != 1 || listed[0].Actions[0] != "restart" {
		t.Fatalf("alpha actions = %#v", listed[0].Actions)
	}
	if listed[3].Actions == nil || len(listed[3].Actions) != 0 {
		t.Fatalf("delta actions = %#v, want an empty array", listed[3].Actions)
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
	extensionHost := openHostWithServices(t, ctx, backend)
	installServicesPlugin(t, ctx, extensionHost)
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
		name  string
		query string
		want  bool
	}{
		{name: "elevated", query: "&sudo=true", want: true},
		{name: "host account", want: false},
		{name: "sudo false", query: "&sudo=false", want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := testContext(t)
			backend := &fakeServiceBackend{}
			extensionHost := openHostWithServices(t, ctx, backend)
			installServicesPlugin(t, ctx, extensionHost)
			server := startServer(t, extensionHost)

			status, body := httpJSONStatus(t, http.MethodPost, server.URL+servicesAPIPath+"/restart?unit=alpha.service"+testCase.query)
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

func TestServicesBackendErrorsSurfaceWithCode(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{
		listErr:    &services.Error{Code: "operation_failed", Message: "操作失败"},
		detailErr:  &services.Error{Code: "not_found", Message: "服务不存在"},
		restartErr: &services.Error{Code: "permission", Message: "需要权限"},
	}
	extensionHost := openHostWithServices(t, ctx, backend)
	installServicesPlugin(t, ctx, extensionHost)
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

func TestLuaServicesStartStopAndLogs(t *testing.T) {
	ctx := testContext(t)
	backend := &fakeServiceBackend{logs: []string{"first", "second"}}
	extensionHost := openHostWithServices(t, ctx, backend)
	installServicesPlugin(t, ctx, extensionHost)
	server := startServer(t, extensionHost)
	for _, tc := range []struct {
		method, path, want string
		status             int
	}{
		{"POST", "start?unit=alpha.service&sudo=true", "true", 200},
		{"POST", "stop?unit=alpha.service", "true", 200},
		{"POST", "stop?unit=alpha.service&sudo=invalid", "sudo 必须是布尔值", 500},
		{"GET", "get?unit=invalid", "invalid unit", 500},
		{"GET", "logs?unit=alpha.service", `["first","second"]`, 200},
		{"GET", "logs?unit=alpha.service&lines=1", `["first","second"]`, 200},
		{"GET", "logs?unit=alpha.service&lines=2000", `["first","second"]`, 200},
		{"GET", "logs?unit=alpha.service&lines=0", "lines 必须是", 500},
		{"GET", "logs?unit=alpha.service&lines=2001", "lines 必须是", 500},
		{"GET", "logs?unit=alpha.service&lines=1.5", "lines 必须是", 500},
		{"GET", "logs?unit=alpha.service&lines=text", "lines 必须是", 500},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, body := httpJSONStatus(t, tc.method, server.URL+servicesAPIPath+"/"+tc.path)
			if status != tc.status || (tc.status == 200 && strings.TrimSpace(body) != tc.want) ||
				(tc.status != 200 && !strings.Contains(body, tc.want)) {
				t.Fatalf("response = %d, %q, want %d, %q", status, body, tc.status, tc.want)
			}
		})
	}

	for _, action := range []struct{ name, method string }{
		{"start", http.MethodPost}, {"stop", http.MethodPost}, {"logs", http.MethodGet},
	} {
		for _, tc := range []struct{ query, want string }{
			{"unit=invalid", "invalid unit"},
			{"unit=1&numericUnit=1", "string expected"},
		} {
			t.Run(action.name+"/"+tc.query, func(t *testing.T) {
				path := servicesAPIPath + "/" + action.name + "?" + tc.query
				status, body := httpJSONStatus(t, action.method, server.URL+path)
				if status != http.StatusInternalServerError || !strings.Contains(body, tc.want) {
					t.Fatalf("response = %d, %q, want 500, %q", status, body, tc.want)
				}
			})
		}
	}
	wantCalls := []fakeServiceCall{
		{action: "start", unit: "alpha.service", elevated: true},
		{action: "stop", unit: "alpha.service"},
		{action: "logs", unit: "alpha.service", lines: 200},
		{action: "logs", unit: "alpha.service", lines: 1},
		{action: "logs", unit: "alpha.service", lines: 2000},
	}
	if calls := backend.serviceCalls(); !slices.Equal(calls, wantCalls) {
		t.Fatalf("service calls = %+v, want %+v", calls, wantCalls)
	}
}

type fakeRestartCall struct {
	unit     string
	elevated bool
}

type fakeServiceCall struct {
	action   string
	unit     string
	elevated bool
	lines    int
}

type fakeServiceBackend struct {
	services   []services.Service
	detail     services.ServiceDetail
	listErr    error
	detailErr  error
	restartErr error
	logs       []string

	mu       sync.Mutex
	restarts []fakeRestartCall
	calls    []fakeServiceCall
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

func (f *fakeServiceBackend) Start(_ context.Context, unit string, elevated bool) error {
	f.recordCall(fakeServiceCall{action: "start", unit: unit, elevated: elevated})
	return nil
}

func (f *fakeServiceBackend) Stop(_ context.Context, unit string, elevated bool) error {
	f.recordCall(fakeServiceCall{action: "stop", unit: unit, elevated: elevated})
	return nil
}

func (f *fakeServiceBackend) Logs(_ context.Context, unit string, lines int) ([]string, error) {
	f.recordCall(fakeServiceCall{action: "logs", unit: unit, lines: lines})
	return f.logs, nil
}

func (f *fakeServiceBackend) recordCall(call fakeServiceCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeServiceBackend) serviceCalls() []fakeServiceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeServiceBackend) restartCalls() []fakeRestartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRestartCall(nil), f.restarts...)
}

func openHostWithServices(t *testing.T, ctx context.Context, backend services.PluginBackend) *host.Host {
	t.Helper()
	extensionHost, err := host.Open(ctx, host.Options{
		DataDir:        t.TempDir(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
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

local function serviceOptions(request)
  local value = request.query.sudo
  if value == "true" then return { sudo = true } end
  if value == "false" then return { sudo = false } end
  if value then return { sudo = value } end
end

host.app.route("POST", "/restart", function(request)
  host.services.restart(request.query.unit, serviceOptions(request))
  return reply({ ok = true })
end)

local function serviceUnit(request)
  if request.query.numericUnit then
    return tonumber(request.query.unit)
  end
  return request.query.unit
end

host.app.route("POST", "/start", function(request)
  return reply(host.services.start(serviceUnit(request), serviceOptions(request)))
end)

host.app.route("POST", "/stop", function(request)
  return reply(host.services.stop(serviceUnit(request), serviceOptions(request)))
end)

host.app.route("GET", "/logs", function(request)
  local lines = request.query.lines
  return reply(host.services.logs(serviceUnit(request), tonumber(lines) or lines))
end)
`

func installServicesPlugin(t *testing.T, ctx context.Context, extensionHost *host.Host) {
	t.Helper()
	manifest := fmt.Sprintf(`{
		"schemaVersion": 1,
		"id": %q,
		"name": "Services Test",
		"version": "0.1.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, servicesPluginID)
	installPackage(t, ctx, extensionHost, manifest, map[string]string{"init.lua": servicesTestLua})
	if err := extensionHost.Enable(ctx, servicesPluginID); err != nil {
		t.Fatalf("Enable %s: %v", servicesPluginID, err)
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
