package host_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
)

func TestLuaHTTPModuleEndToEnd(t *testing.T) {
	const responseDelay = 10 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Plugin-Request") != "lua" {
			http.Error(w, "missing plugin request header", http.StatusBadRequest)
			return
		}

		w.Header().Add("X-Upstream", "first")
		w.Header().Add("X-Upstream", "second")
		time.Sleep(responseDelay)
		switch {
		case r.Method == http.MethodGet && len(body) == 0:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "get response")
		case r.Method == http.MethodPost && string(body) == "request payload":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "created resource")
		default:
			http.Error(w, "unexpected upstream method or body", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)

	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.http", "name": "HTTP test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": fmt.Sprintf(`
		local upstream = %q
		local function jsonResponse(value)
			return {
				headers = { ["content-type"] = "application/json" },
				body = host.json.encode(value),
			}
		end

		host.app.route("GET", "/upstream", function()
			return jsonResponse(assert(host.http.request({
				url = upstream,
				headers = { ["X-Plugin-Request"] = "lua" },
			})))
		end)

		host.app.route("POST", "/upstream", function(request)
			return jsonResponse(assert(host.http.request({
				url = upstream,
				method = "POST",
				headers = { ["X-Plugin-Request"] = "lua" },
				body = request.body,
				maxResponseBytes = 7,
			})))
		end)

		host.app.route("GET", "/empty-array", function()
			return jsonResponse(host.json.array())
		end)

		host.app.route("GET", "/environment", function()
			return jsonResponse({
				shell = assert(host.process.lookPath("sh")),
				os = host.user.os,
				arch = host.user.arch,
				uid = assert(host.user.uid),
				gid = assert(host.user.gid),
				homeDir = assert(host.user.homeDir),
			})
		end)
	`, upstream.URL)})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}

	handler := extensionHost.Handler(host.HTTPOptions{})
	requestJSON := func(t *testing.T, method, path, body string) []byte {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequestWithContext(ctx, method, plugin.APIPath+path, strings.NewReader(body)))
		if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s %s = %d, Content-Type %q, body %q", method, path, res.Code, res.Header().Get("Content-Type"), res.Body)
		}
		return res.Body.Bytes()
	}

	for _, test := range []struct {
		method      string
		requestBody string
		status      int
		body        string
		truncated   bool
	}{
		{http.MethodGet, "", http.StatusOK, "get response", false},
		{http.MethodPost, "request payload", http.StatusCreated, "created", true},
	} {
		t.Run(test.method, func(t *testing.T) {
			body := requestJSON(t, test.method, "upstream", test.requestBody)
			var response struct {
				Status        int               `json:"status"`
				Headers       map[string]string `json:"headers"`
				Body          string            `json:"body"`
				BodyTruncated *bool             `json:"bodyTruncated"`
				LatencyMs     int64             `json:"latencyMs"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatalf("decode upstream response %q: %v", body, err)
			}
			if response.Status != test.status || response.Body != test.body ||
				response.BodyTruncated == nil || *response.BodyTruncated != test.truncated ||
				response.Headers["x-upstream"] != "first, second" ||
				response.LatencyMs < responseDelay.Milliseconds() {
				t.Errorf("unexpected upstream response: %s", body)
			}
		})
	}

	t.Run("empty JSON array", func(t *testing.T) {
		if body := requestJSON(t, http.MethodGet, "empty-array", ""); string(body) != "[]" {
			t.Errorf("empty array body = %q, want []", body)
		}
	})

	t.Run("host environment", func(t *testing.T) {
		shell, err := exec.LookPath("sh")
		if err != nil {
			t.Fatal(err)
		}
		shell, err = filepath.Abs(shell)
		if err != nil {
			t.Fatal(err)
		}
		homeDir, _ := os.UserHomeDir()
		type environment struct {
			Shell   string `json:"shell"`
			OS      string `json:"os"`
			Arch    string `json:"arch"`
			UID     int    `json:"uid"`
			GID     int    `json:"gid"`
			HomeDir string `json:"homeDir"`
		}
		var got environment
		body := requestJSON(t, http.MethodGet, "environment", "")
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode host environment %q: %v", body, err)
		}
		want := environment{Shell: shell, OS: runtime.GOOS, Arch: runtime.GOARCH, UID: os.Getuid(), GID: os.Getgid(), HomeDir: homeDir}
		if got != want {
			t.Errorf("host environment = %+v, want %+v", got, want)
		}
	})
}
