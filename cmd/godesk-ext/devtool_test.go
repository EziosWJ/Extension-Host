package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
)

func TestProjectTemplatesAndPackage(t *testing.T) {
	for index, kind := range projectTypes {
		t.Run(kind.label, func(t *testing.T) {
			t.Chdir(t.TempDir())
			id := "com.example.template"
			const name = "我的 插件"
			input := strings.NewReader(strconv.Itoa(index+1) + "\n" + name + "\n" + id + "\n")
			if err := initProject(nil, input, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := initProject(nil, strings.NewReader("1\n"+name+"\n"+id+"\n"), io.Discard); err == nil {
				t.Fatal("init replaced an existing project")
			}
			t.Chdir(name)
			writeDevTestFile(t, ".env", "local-only")
			// Repeat packaging to cover a pre-existing dist.
			for range 2 {
				if err := packageProject(nil); err != nil {
					t.Fatal(err)
				}
			}
			pkg, err := loadBundle(filepath.Join("dist", id+"-0.1.0.zip"))
			if err != nil {
				t.Fatal(err)
			}
			if pkg.Manifest.Name != name || (pkg.Manifest.Entry.Web != "") != kind.web || (pkg.Manifest.Entry.Lua != "") != kind.lua {
				t.Fatalf("unexpected manifest: %#v", pkg.Manifest)
			}
			for file := range pkg.Files {
				if file != "manifest.json" && !strings.HasPrefix(file, "web/") && !strings.HasPrefix(file, "lua/") {
					t.Errorf("unexpected file in ZIP: %s", file)
				}
			}
		})
	}
}

func TestDevelopmentReloadKeepsConfigurationAndData(t *testing.T) {
	ctx := t.Context()
	t.Chdir(t.TempDir())
	const id = "com.example.development"
	if err := createProject(".", id, "Plugin", projectTypes[0]); err != nil {
		t.Fatal(err)
	}
	writeDevTestFile(t, "manifest.json", `{
		"schemaVersion":1,"id":"com.example.development","name":"Plugin","version":"0.1.0",
		"hostApi":{"min":1,"max":1},"entry":{"web":"web/index.html","lua":"lua/init.lua"},
		"config":{"token":{"type":"string","secret":true}}
	}`)
	script := `
host.app.route("GET", "/hello", function()
  local count = tonumber(host.storage.get("count") or "0") + 1
  host.storage.set("count", count)
  return { body = host.config.get("token") .. ":" .. count .. ":first" }
end)`
	writeDevTestFile(t, "lua/init.lua", script)
	extensionHost, err := host.Open(ctx, host.Options{DataDir: ".godesk-ext"})
	if err != nil {
		t.Fatal(err)
	}
	defer extensionHost.Close(context.Background())
	dev := &developmentSession{host: extensionHost, id: id}
	handler := standaloneHandler(extensionHost, dev)
	apply := func() {
		t.Helper()
		pkg, err := bundle.LoadProject(".")
		if err != nil {
			t.Fatal(err)
		}
		dev.apply(ctx, pkg)
	}
	apply()
	plugin, _ := extensionHost.Get(id)
	if plugin.StartedAt == nil {
		t.Fatal("development plugin did not start")
	}
	requestDevTest(t, handler, "PUT", "/api/v1/extensions/"+id+"/config", `{"token":"saved"}`, 200)
	if body := requestDevTest(t, handler, "GET", plugin.APIPath+"hello", "", 200).Body.String(); body != "saved:1:first" {
		t.Fatal(body)
	}

	writeDevTestFile(t, "web/index.html", "<h1>Updated</h1>")
	apply()
	web, _ := extensionHost.Get(id)
	if web.StartedAt == nil || !web.StartedAt.Equal(*plugin.StartedAt) {
		t.Fatal("web changes restarted the Lua instance")
	}
	page := requestDevTest(t, handler, "GET", web.WebPath, "", 200).Body.String()
	if !strings.Contains(page, "<h1>Updated</h1>") || !strings.Contains(page, "data-revision") {
		t.Fatal(page)
	}

	writeDevTestFile(t, "lua/init.lua", "error('broken startup')")
	apply()
	if body := requestDevTest(t, handler, "GET", plugin.APIPath+"hello", "", 200).Body.String(); body != "saved:2:first" {
		t.Fatal(body)
	}
	writeDevTestFile(t, "lua/init.lua", strings.ReplaceAll(script, "first", "next"))
	apply()
	if body := requestDevTest(t, handler, "GET", plugin.APIPath+"hello", "", 200).Body.String(); body != "saved:3:next" {
		t.Fatal(body)
	}
	if err := extensionHost.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := host.Open(ctx, host.Options{DataDir: ".godesk-ext"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if err := reopened.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if body := requestDevTest(t, reopened.Handler(host.HTTPOptions{}), "GET", plugin.APIPath+"hello", "", 200).Body.String(); body != "saved:4:next" {
		t.Fatal(body)
	}
}

func TestStandaloneHomeSettingPersistsAndFallsBack(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	open := func() *host.Host {
		t.Helper()
		h, err := host.Open(ctx, host.Options{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close(context.Background()) })
		return h
	}
	h := open()
	pkg, err := bundle.LoadDir(filepath.Join("..", "..", "examples", "web-only"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := pkg.Zip()
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := h.Install(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	assertHome := func(h *host.Host, path string) {
		t.Helper()
		handler := standaloneHandler(h, nil)
		response := requestDevTest(t, handler, "GET", "/", "", 302)
		if location := response.Header().Get("Location"); location != path {
			t.Fatalf("home redirects to %q, want %q", location, path)
		}
	}
	assertHome(h, "/host/")
	handler := standaloneHandler(h, nil)
	requestDevTest(t, handler, "GET", "/host/", "", 200)
	body, err := json.Marshal(map[string]string{"pluginId": plugin.ID})
	if err != nil {
		t.Fatal(err)
	}
	requestDevTest(t, handler, "PUT", "/api/v1/host/home", string(body), 400)
	if err := h.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	requestDevTest(t, handler, "PUT", "/api/v1/host/home", string(body), 204)
	assertHome(h, plugin.WebPath)
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}

	h = open()
	if err := h.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	assertHome(h, plugin.WebPath)
	if err := h.Stop(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	assertHome(h, "/host/")
	if err := h.Start(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	assertHome(h, plugin.WebPath)
	requestDevTest(t, standaloneHandler(h, nil), "PUT", "/api/v1/host/home", `{"pluginId":""}`, 204)
	assertHome(h, "/host/")
}

func requestDevTest(t *testing.T, handler http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	if recorder.Code != status {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, recorder.Code, status, recorder.Body.String())
	}
	return recorder
}

func writeDevTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
