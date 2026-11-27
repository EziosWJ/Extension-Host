package lua

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"
)

func newTestHTTPState(t *testing.T, ctx context.Context, address string) *glua.LState {
	t.Helper()
	L := glua.NewState()
	t.Cleanup(L.Close)

	r := &Runtime{execCtx: ctx}
	host := L.NewTable()
	host.RawSetString("http", r.httpModule(L))
	L.SetGlobal("host", host)
	L.SetGlobal("url", glua.LString(address))
	return L
}

func runHTTPTestScript(t *testing.T, L *glua.LState, script string) {
	t.Helper()
	if err := L.DoString(script); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Request-Method", r.Method)
		w.Header().Set("X-Request-Header", r.Header.Get("X-Plugin-Header"))
		w.Header().Add("X-Repeat", "first")
		w.Header().Add("X-Repeat", "second")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	L := newTestHTTPState(t, t.Context(), server.URL)
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url})
		assert(response, err)
		assert(err == nil and response.status == 200)
		assert(response.headers["x-request-method"] == "GET")
		assert(response.headers["X-Request-Method"] == nil)
		assert(response.headers["x-repeat"] == "first, second")
		assert(response.body == "" and response.bodyTruncated == false)
		assert(type(response.latencyMs) == "number" and response.latencyMs >= 0)
		assert(response.latencyMs % 1 == 0)

		response, err = host.http.request({
			url = url,
			method = "post",
			headers = {["x-plugin-header"] = "custom"},
			body = "payload\0bytes",
		})
		assert(response, err)
		assert(err == nil and response.status == 201)
		assert(response.headers["x-request-method"] == "POST")
		assert(response.headers["x-request-header"] == "custom")
		assert(response.body == "payload\0bytes")
	`)
}

func TestHTTPRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "target")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	L := newTestHTTPState(t, t.Context(), server.URL+"/redirect")
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url, followRedirects = true})
		assert(response, err)
		assert(response.status == 200 and response.body == "target")

		response, err = host.http.request({url = url})
		assert(response, err)
		assert(response.status == 200 and response.body == "target")

		response, err = host.http.request({url = url, followRedirects = false})
		assert(response, err)
		assert(response.status == 302 and response.headers.location == "/target")
	`)
}

func TestHTTPTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tls")
	}))
	defer server.Close()

	L := newTestHTTPState(t, t.Context(), server.URL)
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url, insecureSkipVerify = false})
		assert(response == nil and type(err) == "string")
		assert(string.find(err, "certificate", 1, true), err)

		response, err = host.http.request({url = url, insecureSkipVerify = true})
		assert(response, err)
		assert(response.status == 200 and response.body == "tls")
	`)
}

func TestHTTPResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "abcdef")
	}))
	defer server.Close()

	L := newTestHTTPState(t, t.Context(), server.URL)
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url, maxResponseBytes = 3})
		assert(response, err)
		assert(response.body == "abc" and response.bodyTruncated == true)

		response, err = host.http.request({url = url, maxResponseBytes = 6})
		assert(response, err)
		assert(response.body == "abcdef" and response.bodyTruncated == false)
	`)
}

func TestHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	L := newTestHTTPState(t, t.Context(), server.URL)
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url, timeoutMs = 50})
		assert(response == nil and type(err) == "string")
		assert(string.find(err, "context deadline exceeded", 1, true), err)
	`)
}

func TestHTTPCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()

	L := newTestHTTPState(t, ctx, server.URL)
	L.SetGlobal("stoppedError", glua.LString(ErrStopped.Error()))
	runHTTPTestScript(t, L, `
		local response, err = host.http.request({url = url})
		assert(response == nil and type(err) == "string")
		assert(string.find(err, "context canceled", 1, true), err)

		response, err = host.http.request({url = url})
		assert(response == nil and err == stoppedError)
	`)
}

func TestHTTPInvalidOptions(t *testing.T) {
	L := newTestHTTPState(t, t.Context(), "http://localhost")
	cases := []struct {
		options string
		field   string
	}{
		{`nil`, "options"},
		{`"http://localhost"`, "options"},
		{`{}`, "url"},
		{`{url = 123}`, "url"},
		{`{url = "file:///tmp"}`, "url"},
		{`{url = "https:///path"}`, "url"},
		{`{url = "http://%zz"}`, "url"},
		{`{url = url, method = ""}`, "method"},
		{`{url = url, method = "GET!"}`, "method"},
		{`{url = url, method = 1}`, "method"},
		{`{url = url, headers = false}`, "headers"},
		{`{url = url, headers = {[1] = "value"}}`, "headers"},
		{`{url = url, headers = {[""] = "value"}}`, "headers"},
		{`{url = url, headers = {name = 123}}`, "headers"},
		{`{url = url, body = 123}`, "body"},
		{`{url = url, timeoutMs = 0}`, "timeoutMs"},
		{`{url = url, timeoutMs = 1.5}`, "timeoutMs"},
		{`{url = url, timeoutMs = 120001}`, "timeoutMs"},
		{`{url = url, timeoutMs = "10"}`, "timeoutMs"},
		{`{url = url, insecureSkipVerify = "true"}`, "insecureSkipVerify"},
		{`{url = url, followRedirects = 1}`, "followRedirects"},
		{`{url = url, maxResponseBytes = 0}`, "maxResponseBytes"},
		{`{url = url, maxResponseBytes = 0 / 0}`, "maxResponseBytes"},
		{`{url = url, maxResponseBytes = 1.5}`, "maxResponseBytes"},
		{`{url = url, maxResponseBytes = 8388609}`, "maxResponseBytes"},
		{`{url = url, maxResponseBytes = "10"}`, "maxResponseBytes"},
	}
	for _, tc := range cases {
		t.Run(tc.options, func(t *testing.T) {
			err := L.DoString("host.http.request(" + tc.options + ")")
			if err == nil || !strings.Contains(err.Error(), "bad argument #1") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("got %v, want argument #1 error for %s", err, tc.field)
			}
		})
	}
}
