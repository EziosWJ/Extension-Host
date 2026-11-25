// Package native runs executable plugins and serves their local Host API.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	logger *slog.Logger
	lookup func(id string) *Process

	mu       sync.Mutex
	dir      string
	http     *http.Server
	sessions map[string]*Process
	closed   bool
}

// New creates a server which listens when its first native instance starts.
func New(logger *slog.Logger, lookup func(id string) *Process) *Server {
	return &Server{logger: logger, lookup: lookup, sessions: make(map[string]*Process)}
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
	mux.HandleFunc("POST /v1/services/{unit}/restart", s.restart)
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
	items, err := caller(r).options.Services.List(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	item, err := caller(r).options.Services.Detail(r.Context(), r.PathValue("unit"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if err := caller(r).options.Services.Restart(r.Context(), r.PathValue("unit")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeServiceError(w http.ResponseWriter, err error) {
	var accessErr *serviceaccess.Error
	var serviceErr *services.Error
	switch {
	case errors.As(err, &accessErr):
		status := http.StatusBadRequest
		if accessErr.Code == "forbidden" {
			status = http.StatusForbidden
		}
		writeError(w, status, accessErr.Code, accessErr.Message)
	case errors.As(err, &serviceErr):
		status := http.StatusBadGateway
		switch serviceErr.Code {
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
