package host_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
)

const luaRouteParamsManifest = `{
	"schemaVersion": 1, "id": "test.lua.route-params", "name": "Route parameters",
	"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
	"entry": {"lua": "init.lua"}
}`

func TestLuaRouteParams(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, luaRouteParamsManifest, map[string]string{"init.lua": `
		host.app.route("GET", "/containers/{id}", function(request)
			assert(request.params.tab == nil)
			return {body = request.params.id}
		end)
		host.app.route("GET", "/containers/{id}/{tab}", function(request)
			return {body = request.params.id .. "|" .. request.params.tab}
		end)
		host.app.route("GET", "/{kind}/{_id2}/test", function(request)
			return {body = "suffix:" .. request.params._id2}
		end)
		host.app.route("GET", "/containers/{id}/test", function(request)
			return {body = "container test:" .. request.params.id}
		end)
		host.app.route("GET", "/registries/{id}/{tab}", function(request)
			return {body = "registry:" .. request.params.id .. "|" .. request.params.tab}
		end)
		host.app.route("POST", "/containers/{name}", function(request)
			return {body = request.params.name}
		end)
		local function exact(request)
			assert(type(request.params) == "table")
			assert(next(request.params) == nil and request.params.id == nil)
			return {body = request.path}
		end
		host.app.route("GET", "/containers/prune", exact)
		host.app.route("GET", "/hello", exact)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	handler := extensionHost.Handler(host.HTTPOptions{})
	for _, test := range []struct {
		method string
		path   string
		status int
		body   string
	}{
		{http.MethodGet, "containers/abc", http.StatusOK, "abc"},
		{http.MethodGet, "containers/abc/logs", http.StatusOK, "abc|logs"},
		{http.MethodGet, "containers/a%20b/x%3Ay", http.StatusOK, "a b|x:y"},
		{http.MethodGet, "containers/a%2520b", http.StatusOK, "a%20b"},
		{http.MethodGet, "containers/a%25oops", http.StatusOK, "a%oops"},
		{http.MethodGet, "containers/abc/test", http.StatusOK, "container test:abc"},
		{http.MethodGet, "registries/abc/test", http.StatusOK, "registry:abc|test"},
		{http.MethodGet, "images/abc/test", http.StatusOK, "suffix:abc"},
		{http.MethodPost, "containers/abc", http.StatusOK, "abc"},
		{http.MethodGet, "containers/prune", http.StatusOK, "/containers/prune"},
		{http.MethodGet, "containers/%70rune", http.StatusOK, "/containers/prune"},
		{http.MethodGet, "hello", http.StatusOK, "/hello"},
		{http.MethodPut, "containers/abc", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "containers/prune", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "hello", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "containers/", http.StatusNotFound, ""},
		{http.MethodGet, "containers/abc/logs/extra", http.StatusNotFound, ""},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, plugin.APIPath+test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %q", response.Code, test.status, response.Body.String())
			}
			if test.status == http.StatusOK && response.Body.String() != test.body {
				t.Fatalf("body = %q, want %q", response.Body.String(), test.body)
			}
		})
	}
}

func TestLuaRouteParamsRegistrationErrors(t *testing.T) {
	for _, test := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"duplicate", []string{"/containers/{id}", "/containers/{id}"}, "duplicate route"},
		{"conflicting structure", []string{"/containers/{id}", "/containers/{name}"}, "duplicate route"},
		{"invalid name", []string{"/containers/{1id}"}, "invalid parameter name"},
		{"empty name", []string{"/containers/{}"}, "invalid parameter name"},
		{"repeated name", []string{"/a/{id}/b/{id}"}, "duplicate parameter name"},
		{"partial prefix", []string{"/prefix-{id}"}, "parameter must occupy an entire path segment"},
		{"partial suffix", []string{"/{id}.json"}, "parameter must occupy an entire path segment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := testContext(t)
			extensionHost, _ := newTestHost(t, ctx)
			var script strings.Builder
			for _, path := range test.paths {
				fmt.Fprintf(&script, "host.app.route('GET', %q, function() return {} end)\n", path)
			}
			plugin := installPackage(t, ctx, extensionHost, luaRouteParamsManifest, map[string]string{"init.lua": script.String()})
			err := extensionHost.Enable(ctx, plugin.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Enable error = %v, want %q", err, test.want)
			}
		})
	}
}
