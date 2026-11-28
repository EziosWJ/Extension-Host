package host_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/logtail"
)

func TestLogsTail(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	plugin := installExample(t, ctx, extensionHost, "hello")
	handler := extensionHost.Handler(host.HTTPOptions{
		RequireSession: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Session") != "allowed" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	url := "/api/v1/extensions/" + plugin.ID + "/logs"
	request := func(target string, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if authenticated {
			req.Header.Set("X-Session", "allowed")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	checkLines := func(query string, want []string) {
		t.Helper()
		response := request(url+query, true)
		var body struct {
			Lines []string `json:"lines"`
		}
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
			json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Lines == nil || !slices.Equal(body.Lines, want) {
			if response.Body.Len() > 400 {
				t.Fatalf("logs %q: status %d, unexpected response of %d bytes", query, response.Code, response.Body.Len())
			}
			t.Fatalf("logs %q: status %d, body %s", query, response.Code, response.Body)
		}
	}

	if response := request(url, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("logs without session: %d", response.Code)
	}
	checkLines("", nil) // Missing log.
	if err := os.MkdirAll(filepath.Dir(plugin.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.LogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	checkLines("?lines=200", nil) // Empty log.
	if err := os.WriteFile(plugin.LogPath, []byte("first\r\n\r\nlast"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkLines("?lines=3", []string{"first", "", "last"})
	checkLines("?lines=1", []string{"last"})

	file, err := os.Create(plugin.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	// Make a sparse prefix to exercise tailing without allocating the entire log.
	if _, err := file.Seek(512<<20, 0); err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	contents.WriteByte('\n')
	want := make([]string, 1001)
	for i := range want {
		want[i] = fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 42))
		contents.WriteString(want[i])
		contents.WriteByte('\n')
	}
	if _, err := file.WriteString(contents.String()); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	checkLines("", want[len(want)-200:])
	checkLines("?lines=200", want[len(want)-200:])
	checkLines("?lines=1000", want[1:])

	if err := os.Truncate(plugin.LogPath, 0); err != nil {
		t.Fatal(err)
	}
	// A sparse file with no newline still represents one oversized log line.
	if err := os.Truncate(plugin.LogPath, 512<<20); err != nil {
		t.Fatal(err)
	}
	checkExtensionError(t, request(url+"?lines=1", true), http.StatusRequestEntityTooLarge, "log_tail_too_large")

	// JSON escaping can exceed the response budget even when the raw line fits.
	if err := os.WriteFile(plugin.LogPath, bytes.Repeat([]byte{'<'}, logtail.MaxBytes/4), 0o600); err != nil {
		t.Fatal(err)
	}
	checkExtensionError(t, request(url+"?lines=1", true), http.StatusRequestEntityTooLarge, "log_tail_too_large")

	for _, query := range []string{"?lines=0", "?lines=-1", "?lines=1001", "?lines=1.5", "?lines=bad", "?lines=", "?lines=1&lines=2", "?lines=%XX"} {
		t.Run(query, func(t *testing.T) {
			checkExtensionError(t, request(url+query, true), http.StatusBadRequest, "invalid_lines")
		})
	}
	checkExtensionError(t, request("/api/v1/extensions/not-installed/logs", true), http.StatusNotFound, "not_found")
}

func TestLogsIsolatePackageCoordinates(t *testing.T) {
	ctx := testContext(t)
	h, dataDir := newTestHost(t, ctx)
	handler := h.Handler(host.HTTPOptions{})
	logs := func(id string) []string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/extensions/"+id+"/logs", nil))
		var body struct{ Lines []string }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("read logs for %s: %d %s", id, response.Code, response.Body)
		}
		return body.Lines
	}
	install := func(id, version, message string) host.Plugin {
		t.Helper()
		script := fmt.Sprintf(`host.app.on("start", function() print(%q) end)`, message)
		archive := upgradeArchive(t, id, version, id, script, nil)
		plugin, err := h.Install(ctx, bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		return plugin
	}
	first := install("x", "1.2.3-4.5.6", "first-coordinate")
	second := install("x-1.2.3", "4.5.6", "second-coordinate")
	for _, plugin := range []host.Plugin{first, second} {
		want := filepath.Join(dataDir, "logs", "by-plugin", plugin.ID, plugin.Version+".log")
		if plugin.LogPath != want {
			t.Fatalf("log path for %s = %q, want %q", plugin.ID, plugin.LogPath, want)
		}
		if err := h.Enable(ctx, plugin.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ id, own, other string }{
		{first.ID, "first-coordinate", "second-coordinate"},
		{second.ID, "second-coordinate", "first-coordinate"},
	} {
		body := strings.Join(logs(tc.id), "\n")
		if !strings.Contains(body, tc.own) || strings.Contains(body, tc.other) {
			t.Fatalf("mixed logs for %s: %q", tc.id, body)
		}
	}
}

func checkExtensionError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Code    string  `json:"code"`
		Message string  `json:"message"`
		Error   *string `json:"error"`
	}
	if response.Code != status || response.Header().Get("Cache-Control") != "no-store" ||
		json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != code || body.Message == "" || body.Error != nil {
		t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body, status, code)
	}
}
