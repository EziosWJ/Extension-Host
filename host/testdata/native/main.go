// This executable exercises the native plugin protocol in host integration tests.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)
	socket, token := os.Getenv("GODESK_HOST_SOCKET"), os.Getenv("GODESK_HOST_TOKEN")
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 7 * time.Second,
	}
	request := func(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, "http://host"+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return client.Do(req)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	response, err := request(ctx, "GET", "/v1/context", nil)
	if err != nil {
		log.Fatal(err)
	}
	var info struct {
		DataDir string
		Config  map[string]any
	}
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil || response.StatusCode != http.StatusOK {
		log.Fatalf("context: HTTP %d: %v", response.StatusCode, err)
	}
	_ = response.Body.Close()
	if info.Config["earlyExit"] == true {
		os.Exit(7)
	}
	if info.Config["holdReady"] == true {
		<-ctx.Done()
		return
	}
	// Expose the private connection only to the test harness through its temporary directory.
	data, _ := json.Marshal(map[string]any{"socket": socket, "token": token, "pid": os.Getpid()})
	if err := os.WriteFile(filepath.Join(info.DataDir, "connection.json"), data, 0o600); err != nil {
		log.Fatal(err)
	}

	if provider := os.Getenv("GODESK_PLUGIN_SOCKET"); provider != "" {
		listener, err := net.Listen("unix", provider)
		if err != nil {
			log.Fatal(err)
		}
		defer listener.Close()
		mux := http.NewServeMux()
		mux.HandleFunc("POST /rpc/v1.getStatus", func(w http.ResponseWriter, r *http.Request) {
			var params map[string]any
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if params["slow"] == true {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"caller": r.Header.Get("X-Godesk-Caller"), "params": params})
		})
		mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
			target, err := request(r.Context(), "POST", "/v1/plugins/com.example.provider/methods/v1.getStatus", []byte(`{"query":"ok"}`))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer target.Body.Close()
			w.Header().Set("Content-Type", target.Header.Get("Content-Type"))
			w.WriteHeader(target.StatusCode)
			_, _ = io.Copy(w, target.Body)
		})
		mux.HandleFunc("GET /api/details/{item}", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"item": r.PathValue("item"), "query": r.URL.Query().Get("q"),
				"cookie": r.Header.Get("Cookie"), "caller": r.Header.Get("X-Godesk-Caller"),
			})
		})
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing host token", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})}
		defer server.Close()
		go func() { _ = server.Serve(listener) }()
	}

	response, err = request(ctx, "POST", "/v1/ready", nil)
	if err != nil {
		log.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		log.Fatalf("ready: HTTP %d", response.StatusCode)
	}
	log.Print("fixture ready")
	<-ctx.Done()
}
