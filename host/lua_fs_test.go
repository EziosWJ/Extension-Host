package host_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
)

func TestLuaFSModuleEndToEnd(t *testing.T) {
	ctx := testContext(t)
	extensionHost, root := newTestHost(t, ctx)

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.fs", "name": "FS test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.route("POST", "/workflow", function()
			local base = host.dataDir .. "/stacks"
			local emptyDir = base .. "/empty"
			assert(host.fs.mkdirAll(emptyDir, "0700"))

			local emptyEntries = assert(host.fs.listDir(emptyDir))
			local emptyJSON = host.json.encode(emptyEntries)
			assert(emptyJSON == "[]", "expected [], got " .. emptyJSON)

			local stackDir = base .. "/demo"
			assert(host.fs.writeFile(stackDir .. "/compose.yaml", "services:\n  web:\n    image: nginx\n", {
				mkdir = true,
				mode = "0644",
				atomic = true,
			}))
			assert(host.fs.writeFile(stackDir .. "/.env", "SECRET=top-secret\n", {
				mode = "0600",
			}))

			local envContents = assert(host.fs.readFile(stackDir .. "/.env"))
			local limited, limitErr = host.fs.readFile(stackDir .. "/.env", { maxBytes = 4 })
			assert(limited == nil and type(limitErr) == "string", "expected readFile limit error")
			assert(string.find(limitErr, "maxBytes", 1, true), limitErr)

			local envStat = assert(host.fs.stat(stackDir .. "/.env"))
			assert(envStat.isFile and not envStat.isDir and envStat.mode == "0600")
			assert(envStat.size == #"SECRET=top-secret\n")

			local entries = assert(host.fs.listDir(stackDir))
			local names = {}
			for i, entry in ipairs(entries) do
				names[i] = entry.name
			end

			local missing, missingErr = host.fs.stat(stackDir .. "/nonexistent")
			assert(missing == nil and type(missingErr) == "string")

			assert(host.fs.removeAll(emptyDir))
			local afterRemove = host.fs.stat(emptyDir)
			assert(afterRemove == nil)

			return {
				headers = { ["content-type"] = "application/json; charset=utf-8" },
				body = host.json.encode({
					names = names,
					envMode = envStat.mode,
					envContents = envContents,
				}),
			}
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}

	handler := extensionHost.Handler(host.HTTPOptions{})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, http.MethodPost, plugin.APIPath+"workflow", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("POST workflow = %d, %q", res.Code, res.Body)
	}

	var payload struct {
		Names       []string `json:"names"`
		EnvMode     string   `json:"envMode"`
		EnvContents string   `json:"envContents"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.EnvMode != "0600" {
		t.Fatalf("envMode = %q, want 0600", payload.EnvMode)
	}
	if payload.EnvContents != "SECRET=top-secret\n" {
		t.Fatalf("readFile contents = %q, want %q", payload.EnvContents, "SECRET=top-secret\n")
	}
	if len(payload.Names) != 2 || payload.Names[0] != ".env" || payload.Names[1] != "compose.yaml" {
		t.Fatalf("names = %v, want [.env compose.yaml]", payload.Names)
	}

	envPath := filepath.Join(root, "plugins", plugin.ID, "data", "stacks", "demo", ".env")
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat .env: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %04o, want 0600", info.Mode().Perm())
	}
}
