package host_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/EziosWJ/Extension-Host/host"
)

func TestPluginLogs(t *testing.T) {
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

	for _, query := range []string{"?lines=0", "?lines=-1", "?lines=1001", "?lines=1.5", "?lines=bad", "?lines=", "?lines=1&lines=2", "?lines=%XX"} {
		t.Run(query, func(t *testing.T) {
			checkExtensionError(t, request(url+query, true), http.StatusBadRequest, "invalid_lines")
		})
	}
	checkExtensionError(t, request("/api/v1/extensions/not-installed/logs", true), http.StatusNotFound, "not_found")
}

func checkExtensionError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if response.Code != status || response.Header().Get("Cache-Control") != "no-store" ||
		json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != code || body.Message == "" || body.Error != body.Message {
		t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body, status, code)
	}
}
