// Package gateway exposes the management API, plugin web assets and plugin runtime routes.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/lifecycle"
	"github.com/EziosWJ/Extension-Host/internal/lua"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
)

const (
	maxRequestBody = 8 << 20
	maxConfigBody  = 1 << 20
	operationLimit = 30 * time.Second
)

var errInvalidPackage = errors.New("invalid plugin package")

// Options configures the HTTP surface.
type Options struct {
	// RequireSession wraps every extension route with the embedder's session check.
	RequireSession func(http.Handler) http.Handler
	// AllowedOrigins lists extra WebSocket origins for embedded development proxies.
	AllowedOrigins      []string
	DevelopmentRevision func(pluginID string) string
}

// Gateway serves extension HTTP routes.
type Gateway struct {
	manager             *lifecycle.Manager
	logger              *slog.Logger
	allowedOrigins      []string
	developmentRevision func(pluginID string) string
}

// New builds the extension HTTP handler.
func New(manager *lifecycle.Manager, logger *slog.Logger, opts Options) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	gateway := &Gateway{
		manager: manager, logger: logger, allowedOrigins: opts.AllowedOrigins,
		developmentRevision: opts.DevelopmentRevision,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/extensions", gateway.list)
	mux.HandleFunc("POST /api/v1/extensions", gateway.install)
	mux.HandleFunc("GET /api/v1/extensions/{id}", gateway.get)
	mux.HandleFunc("GET /api/v1/extensions/{id}/logs", gateway.logs)
	mux.HandleFunc("POST /api/v1/extensions/{id}/upgrade", gateway.upgrade)
	mux.HandleFunc("POST /api/v1/extensions/{id}/enable", gateway.enable)
	mux.HandleFunc("POST /api/v1/extensions/{id}/start", gateway.start)
	mux.HandleFunc("POST /api/v1/extensions/{id}/stop", gateway.stop)
	mux.HandleFunc("POST /api/v1/extensions/{id}/disable", gateway.disable)
	mux.HandleFunc("DELETE /api/v1/extensions/{id}", gateway.uninstall)
	mux.HandleFunc("PUT /api/v1/extensions/{id}/config", gateway.updateConfig)

	mux.HandleFunc("GET /extensions/{id}/icon", gateway.icon)
	mux.HandleFunc("GET /extensions/{id}/web", gateway.redirectWeb)
	mux.HandleFunc("GET /extensions/{id}/web/", gateway.web)
	mux.HandleFunc("/extensions/{id}/api", gateway.api)
	mux.HandleFunc("/extensions/{id}/api/", gateway.api)
	mux.HandleFunc("GET /extensions/{id}/events", gateway.events)

	if opts.RequireSession != nil {
		return opts.RequireSession(mux)
	}
	return mux
}

func (g *Gateway) list(w http.ResponseWriter, r *http.Request) {
	views := g.manager.List()
	if views == nil {
		views = []lifecycle.View{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": views})
}

func (g *Gateway) get(w http.ResponseWriter, r *http.Request) {
	view, ok := g.manager.Get(r.PathValue("id"))
	if !ok {
		writeCodedError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%v: %s", lifecycle.ErrNotFound, r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (g *Gateway) install(w http.ResponseWriter, r *http.Request) {
	g.publishPackage(w, r, http.StatusCreated, bundle.LoadZipStructure, g.manager.Install)
}

func (g *Gateway) upgrade(w http.ResponseWriter, r *http.Request) {
	g.publishPackage(w, r, http.StatusOK, bundle.LoadZip, func(ctx context.Context, pkg *bundle.Bundle) (lifecycle.View, error) {
		return g.manager.Upgrade(ctx, r.PathValue("id"), pkg)
	})
}

func (g *Gateway) publishPackage(w http.ResponseWriter, r *http.Request, status int, load func([]byte) (*bundle.Bundle, error), publish func(context.Context, *bundle.Bundle) (lifecycle.View, error)) {
	ctx, cancel := operationContext(r)
	defer cancel()

	data, err := readPackage(w, r)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_package", err.Error())
		return
	}
	pkg, err := load(data)
	if errors.Is(err, manifest.ErrHostAPIIncompatible) {
		g.fail(w, err)
		return
	}
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_package", fmt.Sprintf("%v: %v", errInvalidPackage, err))
		return
	}
	view, err := publish(ctx, pkg)
	if errors.Is(err, lifecycle.ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code": "already_installed", "message": err.Error(), "pluginId": pkg.Manifest.ID,
		})
		return
	}
	if err != nil {
		g.fail(w, err)
		return
	}
	writeJSON(w, status, view)
}

func (g *Gateway) enable(w http.ResponseWriter, r *http.Request) {
	g.lifecycleAction(w, r, g.manager.Enable)
}

func (g *Gateway) start(w http.ResponseWriter, r *http.Request) {
	g.lifecycleAction(w, r, g.manager.Start)
}

func (g *Gateway) stop(w http.ResponseWriter, r *http.Request) {
	g.lifecycleAction(w, r, g.manager.Stop)
}

func (g *Gateway) disable(w http.ResponseWriter, r *http.Request) {
	g.lifecycleAction(w, r, g.manager.Disable)
}

func (g *Gateway) lifecycleAction(w http.ResponseWriter, r *http.Request, action func(context.Context, string) error) {
	ctx, cancel := operationContext(r)
	defer cancel()
	id := r.PathValue("id")
	if err := action(ctx, id); err != nil {
		g.fail(w, err)
		return
	}
	view, ok := g.manager.Get(id)
	if !ok {
		writeCodedError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%v: %s", lifecycle.ErrNotFound, id))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (g *Gateway) uninstall(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := operationContext(r)
	defer cancel()
	purge := r.URL.Query().Get("purge")
	if err := g.manager.Uninstall(ctx, r.PathValue("id"), purge == "true" || purge == "1"); err != nil {
		g.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) updateConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := operationContext(r)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, maxConfigBody)
	var values map[string]any
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&values); err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	view, err := g.manager.UpdateConfig(ctx, r.PathValue("id"), values)
	if err != nil {
		if errors.Is(err, lifecycle.ErrNotFound) || errors.Is(err, lifecycle.ErrClosed) {
			g.fail(w, err)
			return
		}
		writeCodedError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (g *Gateway) icon(w http.ResponseWriter, r *http.Request) {
	dir, name, err := g.manager.IconInfo(r.PathValue("id"))
	if err != nil {
		g.fail(w, err)
		return
	}
	local := filepath.FromSlash(name)
	if !withinDir(dir, filepath.Join(dir, local)) {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	defer root.Close()
	info, err := root.Lstat(local)
	if err != nil || !info.Mode().IsRegular() {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	file, err := root.Open(local)
	if err != nil {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}

	var contentType string
	switch strings.ToLower(filepath.Ext(name)) {
	case ".svg":
		contentType = "image/svg+xml"
		w.Header().Set("Content-Security-Policy", "sandbox")
	case ".png":
		contentType = "image/png"
	case ".webp":
		contentType = "image/webp"
	default:
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(name), info.ModTime(), file)
}

func (g *Gateway) redirectWeb(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

func (g *Gateway) web(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var revision string
	if g.developmentRevision != nil {
		revision = g.developmentRevision(id)
	}
	dir, index, err := g.manager.WebInfo(id)
	if err != nil {
		g.fail(w, err)
		return
	}
	prefix := "/extensions/" + id + "/web/"
	relative := strings.TrimPrefix(r.URL.Path, prefix)
	clean := strings.TrimPrefix(path.Clean("/"+relative), "/")
	if clean == "." {
		clean = ""
	}
	if clean == "" {
		clean = index
	}
	target := filepath.Join(dir, filepath.FromSlash(clean))
	if !withinDir(dir, target) {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if revision != "" {
		w.Header().Set("Cache-Control", "no-store")
		switch strings.ToLower(filepath.Ext(target)) {
		case ".html", ".htm":
			serveDevelopmentHTML(w, r, target, revision)
			return
		}
	}
	http.ServeFile(w, r, target)
}

func serveDevelopmentHTML(w http.ResponseWriter, r *http.Request, target, revision string) {
	page, err := os.ReadFile(target)
	if err != nil {
		writeCodedError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	script := []byte("<script defer src=\"/__godesk_dev/reload.js\" data-revision=\"" + html.EscapeString(revision) + "\"></script>")
	index := bytes.LastIndex(bytes.ToLower(page), []byte("</body>"))
	if index == -1 {
		index = len(page)
	}
	content := make([]byte, 0, len(page)+len(script))
	content = append(content, page[:index]...)
	content = append(content, script...)
	content = append(content, page[index:]...)
	http.ServeContent(w, r, filepath.Base(target), time.Time{}, bytes.NewReader(content))
}

func (g *Gateway) api(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	instance, err := g.manager.RunningInstance(id)
	if err != nil {
		g.fail(w, err)
		return
	}
	prefix := "/extensions/" + id + "/api"
	routePath := strings.TrimPrefix(r.URL.Path, prefix)
	if routePath == "" {
		routePath = "/"
	}
	if process := instance.Native(); process != nil {
		if r.ContentLength > maxRequestBody {
			writeCodedError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return
		}
		// Remove the mount prefix while preserving encoded path components.
		segments := strings.SplitN(r.URL.EscapedPath(), "/", 5)
		rawRoute := "/"
		if len(segments) == 5 {
			rawRoute += segments[4]
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		process.ServeWeb(w, r, routePath, rawRoute)
		return
	}
	if !instance.HasLua() {
		g.fail(w, fmt.Errorf("%w: %s", lifecycle.ErrNoLua, id))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeCodedError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	headers := make(map[string]string, len(r.Header))
	for name, values := range r.Header {
		if len(values) > 0 {
			headers[strings.ToLower(name)] = values[0]
		}
	}
	query := make(map[string]string, len(r.URL.Query()))
	for name, values := range r.URL.Query() {
		if len(values) > 0 {
			query[name] = values[0]
		}
	}
	response, err := instance.Handle(r.Context(), lua.Request{
		Method:     r.Method,
		Path:       routePath,
		Query:      query,
		Headers:    headers,
		Body:       body,
		RemoteAddr: r.RemoteAddr,
	})
	if errors.Is(err, lua.ErrStopped) {
		g.fail(w, err)
		return
	}
	if err != nil {
		writeCodedError(w, http.StatusInternalServerError, "internal_error", "plugin handler failed: "+err.Error())
		return
	}
	for name, value := range response.Headers {
		w.Header().Set(name, value)
	}
	if len(response.Body) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(response.Status)
	_, _ = w.Write(response.Body)
}

func (g *Gateway) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	instance, err := g.manager.RunningInstance(id)
	if err != nil {
		g.fail(w, err)
		return
	}
	if !instance.HasLua() {
		g.fail(w, fmt.Errorf("%w: %s", lifecycle.ErrNoLua, id))
		return
	}
	subscription, err := instance.Subscribe()
	if err != nil {
		g.fail(w, err)
		return
	}
	defer subscription.Close()

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: g.allowedOrigins})
	if err != nil {
		g.logger.Warn("reject plugin event websocket", "plugin", id, "error", err)
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(4096)

	readCtx, stopReading := context.WithCancel(context.Background())
	defer stopReading()
	go func() {
		defer stopReading()
		for {
			if _, _, err := connection.Read(readCtx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case payload := <-subscription.C:
			writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := connection.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return
			}
		case <-subscription.Done:
			_ = connection.Close(websocket.StatusNormalClosure, "plugin stopped")
			return
		case <-readCtx.Done():
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (g *Gateway) fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, lifecycle.ErrIncompatible), errors.Is(err, manifest.ErrHostAPIIncompatible):
		status, code = http.StatusBadRequest, "incompatible_package"
	case errors.Is(err, lifecycle.ErrIDMismatch):
		status, code = http.StatusBadRequest, "id_mismatch"
	case errors.Is(err, lifecycle.ErrNotFound), errors.Is(err, lifecycle.ErrNoLua), errors.Is(err, lifecycle.ErrNoWeb), errors.Is(err, lifecycle.ErrNoIcon):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, lifecycle.ErrNotEnabled), errors.Is(err, lifecycle.ErrNotRunning), errors.Is(err, lua.ErrStopped):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, lifecycle.ErrClosed):
		status, code = http.StatusServiceUnavailable, "host_closed"
	}
	if status >= 500 {
		g.logger.Error("extension request failed", "error", err)
	}
	writeCodedError(w, status, code, err.Error())
}

func readPackage(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	mediaType := ""
	if header := r.Header.Get("Content-Type"); header != "" {
		parsed, _, err := mime.ParseMediaType(header)
		if err != nil {
			return nil, fmt.Errorf("invalid Content-Type: %w", err)
		}
		mediaType = parsed
	}
	if mediaType == "multipart/form-data" {
		reader, err := r.MultipartReader()
		if err != nil {
			return nil, fmt.Errorf("read multipart body: %w", err)
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil, fmt.Errorf("multipart body has no plugin package")
			}
			if err != nil {
				return nil, fmt.Errorf("read multipart body: %w", err)
			}
			if part.FormName() != "package" && part.FormName() != "file" {
				_ = part.Close()
				continue
			}
			data, err := io.ReadAll(io.LimitReader(part, bundle.MaxPackageSize+1))
			_ = part.Close()
			if err != nil {
				return nil, fmt.Errorf("read plugin package: %w", err)
			}
			if len(data) > bundle.MaxPackageSize {
				return nil, fmt.Errorf("plugin package exceeds %d bytes", bundle.MaxPackageSize)
			}
			return data, nil
		}
	}
	switch mediaType {
	case "", "application/zip", "application/x-zip-compressed", "application/octet-stream":
	default:
		return nil, fmt.Errorf("unsupported Content-Type %q; send a ZIP package", mediaType)
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, bundle.MaxPackageSize+1))
	if err != nil {
		return nil, fmt.Errorf("read plugin package: %w", err)
	}
	if len(data) > bundle.MaxPackageSize {
		return nil, fmt.Errorf("plugin package exceeds %d bytes", bundle.MaxPackageSize)
	}
	return data, nil
}

func operationContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), operationLimit)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func withinDir(root, name string) bool {
	relative, err := filepath.Rel(root, name)
	if err != nil {
		return false
	}
	return filepath.IsLocal(relative)
}
