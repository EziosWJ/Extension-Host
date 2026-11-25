package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	source := caller(r)
	id, method := r.PathValue("id"), r.PathValue("method")
	target := s.lookup(id)
	if target == nil || target.ctx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "plugin_unavailable", "target plugin is not running")
		return
	}
	if !target.supports(method) {
		writeError(w, http.StatusNotFound, "method_not_found", "target plugin does not export that method")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessage))
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "JSON request exceeds 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_body", "could not read request")
		}
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request must contain one JSON value")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(target.ctx, cancel)
	defer stop()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin/rpc/"+method, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_method", "invalid RPC method")
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+target.token)
	request.Header.Set("X-Godesk-Caller", source.options.ID)
	// Forward each call once. A transport retry with a body must be impossible.
	request.GetBody = nil
	s.logger.Info("native plugin call", "caller", source.options.ID, "plugin", id, "method", method)
	response, err := target.transport.RoundTrip(request)
	if err != nil {
		writeForwardError(w, ctx, target)
		return
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, maxMessage+1))
	if err != nil {
		writeForwardError(w, ctx, target)
		return
	}
	if len(body) > maxMessage || !json.Valid(body) ||
		response.StatusCode < 200 || response.StatusCode >= 600 || response.StatusCode/100 == 3 {
		writeError(w, http.StatusBadGateway, "invalid_provider_response", "provider must return JSON and a 2xx, 4xx or 5xx status")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func writeForwardError(w http.ResponseWriter, ctx context.Context, target *Process) {
	switch {
	case target.ctx.Err() != nil:
		writeError(w, http.StatusServiceUnavailable, "plugin_unavailable", "target plugin stopped")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "plugin request timed out")
	default:
		writeError(w, http.StatusBadGateway, "provider_unreachable", "could not complete the plugin request")
	}
}

// ServeWeb forwards a browser API request to the opted-in backend.
func (p *Process) ServeWeb(w http.ResponseWriter, r *http.Request, routePath, rawPath string) {
	if !p.options.WebAPI {
		writeError(w, http.StatusNotFound, "no_web_api", "plugin does not provide a web API")
		return
	}
	for _, segment := range strings.Split(routePath, "/") {
		if segment == "." || segment == ".." {
			writeError(w, http.StatusBadRequest, "invalid_path", "invalid plugin API path")
			return
		}
	}
	if p.ctx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "plugin_unavailable", "plugin stopped")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	proxy := httputil.ReverseProxy{
		Transport: p.transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(&url.URL{Scheme: "http", Host: "plugin"})
			request.Out.URL.Path = "/api" + routePath
			request.Out.URL.RawPath = "/api" + rawPath
			request.Out.Header.Del("Cookie")
			request.Out.Header.Del("X-Godesk-Caller")
			request.Out.Header.Set("Authorization", "Bearer "+p.token)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "plugin API request body is too large")
				return
			}
			writeForwardError(w, r.Context(), p)
		},
	}
	r = r.Clone(ctx)
	r.Header.Del("Upgrade")
	proxy.ServeHTTP(w, r)
}
