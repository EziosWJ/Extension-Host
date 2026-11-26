// Package native runs executable plugins and serves their local Host API.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EziosWJ/Extension-Host/hostsys/services"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
)

const (
	callTimeout = 5 * time.Second
	maxMessage  = 1 << 20
)

// Server owns the Unix listener and the identity of each child instance.
type Server struct {
	logger   *slog.Logger
	lookup   func(id string) *Process
	services *serviceaccess.Access

	mu       sync.Mutex
	dir      string
	http     *http.Server
	sessions map[string]*Process
	closed   bool
}

// New creates a server which listens when its first native instance starts.
func New(logger *slog.Logger, lookup func(id string) *Process, serviceAPI *serviceaccess.Access) *Server {
	return &Server{logger: logger, lookup: lookup, services: serviceAPI, sessions: make(map[string]*Process)}
}

func (s *Server) register(p *Process) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("native Host API is closed")
	}
	if s.http == nil {
		if err := s.listen(); err != nil {
			return "", err
		}
	}
	if p.options.WebAPI || len(p.options.Exports) != 0 {
		dir, err := os.MkdirTemp(s.dir, "p-")
		if err != nil {
			return "", err
		}
		p.socket = filepath.Join(dir, "api.sock")
		p.transport = unixTransport(p.socket)
	}
	s.sessions[p.token] = p
	return filepath.Join(s.dir, "host.sock"), nil
}

func (s *Server) listen() error {
	dir, err := os.MkdirTemp("", "ext-")
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "host.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ready", s.ready)
	mux.HandleFunc("GET /v1/context", s.pluginContext)
	mux.HandleFunc("GET /v1/services", s.list)
	mux.HandleFunc("GET /v1/services/{unit}", s.detail)
	mux.HandleFunc("POST /v1/services/{unit}/start", serviceAction(s.services.Start))
	mux.HandleFunc("POST /v1/services/{unit}/stop", serviceAction(s.services.Stop))
	mux.HandleFunc("POST /v1/services/{unit}/restart", serviceAction(s.services.Restart))
	mux.HandleFunc("GET /v1/services/{unit}/logs", s.logs)
	mux.HandleFunc("POST /v1/plugins/{id}/methods/{method}", s.call)
	server := &http.Server{
		Handler:           s.authenticate(mux),
		ReadHeaderTimeout: callTimeout,
		ReadTimeout:       callTimeout,
		WriteTimeout:      2 * callTimeout,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	s.dir, s.http = dir, server
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("native Host API stopped", "error", err)
		}
	}()
	return nil
}

func (s *Server) unregister(p *Process) {
	p.cancel()
	s.mu.Lock()
	delete(s.sessions, p.token)
	s.mu.Unlock()
	if p.transport != nil {
		p.transport.CloseIdleConnections()
		_ = os.RemoveAll(filepath.Dir(p.socket))
	}
}

// Close releases the Host API listener.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.http == nil {
		return nil
	}
	return errors.Join(s.http.Close(), os.RemoveAll(s.dir))
}

type processKey struct{}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "instance token is required")
			return
		}
		s.mu.Lock()
		p := s.sessions[token]
		s.mu.Unlock()
		if p == nil || p.ctx.Err() != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid instance token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
		defer cancel()
		stop := context.AfterFunc(p.ctx, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, processKey{}, p)))
	})
}

func caller(r *http.Request) *Process {
	return r.Context().Value(processKey{}).(*Process)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	if p.socket != "" {
		connection, err := (&net.Dialer{}).DialContext(r.Context(), "unix", p.socket)
		if err != nil {
			writeError(w, http.StatusConflict, "provider_not_listening", "plugin API socket must listen before readiness")
			return
		}
		_ = connection.Close()
	}
	p.readyOnce.Do(func() { close(p.ready) })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pluginContext(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.options.ID, "dataDir": p.options.DataDir, "config": p.options.Config(),
		"hostApiVersion": manifest.HostAPIVersion, "hostVersion": manifest.Version,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	items, err := s.services.List(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if items == nil {
		items = []services.Service{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	item, err := s.services.Detail(r.Context(), r.PathValue("unit"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func serviceAction(action func(context.Context, string, bool) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", "查询参数格式无效")
			return
		}
		sudo := false
		if values := query["sudo"]; len(values) > 0 {
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				writeError(w, http.StatusBadRequest, "invalid_sudo", "sudo 必须是 true 或 false，且只能提供一次")
				return
			}
			sudo = values[0] == "true"
		}
		if err := action(r.Context(), r.PathValue("unit"), sudo); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "查询参数格式无效")
		return
	}
	lines := serviceaccess.DefaultLogLines
	if values := query["lines"]; len(values) > 0 {
		lines, err = strconv.Atoi(values[0])
		if err != nil || len(values) != 1 || lines < 1 || lines > serviceaccess.MaxLogLines {
			writeError(w, http.StatusBadRequest, "invalid_lines", fmt.Sprintf("lines 必须是 1 到 %d 之间的整数，且只能提供一次", serviceaccess.MaxLogLines))
			return
		}
	}
	items, err := s.services.Logs(r.Context(), r.PathValue("unit"), lines)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if items == nil {
		items = []string{}
	}
	writeJSON(w, http.StatusOK, items)
}

func writeServiceError(w http.ResponseWriter, err error) {
	var serviceErr *services.Error
	switch {
	case errors.As(err, &serviceErr):
		status := http.StatusBadGateway
		switch serviceErr.Code {
		case "invalid_unit":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "permission":
			status = http.StatusForbidden
		}
		writeError(w, status, serviceErr.Code, serviceErr.Message)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "service request timed out")
	default:
		writeError(w, http.StatusBadGateway, "operation_failed", "system service operation failed")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
