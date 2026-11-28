package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/web"
)

func standaloneHandler(extensionHost *host.Host, dev *developmentSession) http.Handler {
	mux := http.NewServeMux()
	options := host.HTTPOptions{}
	if dev != nil {
		options.DevelopmentRevision = dev.revisionFor
		mux.HandleFunc("GET /__godesk_dev/revision", dev.serveRevision)
		mux.HandleFunc("GET /__godesk_dev/reload.js", serveReloadScript)
	}
	extensions := extensionHost.Handler(options)
	mux.Handle("/api/v1/extensions", extensions)
	mux.Handle("/api/v1/extensions/", extensions)
	mux.Handle("/extensions/", extensions)
	mux.Handle("GET /host/", http.StripPrefix("/host", web.Handler()))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/host/favicon.svg", http.StatusFound)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		target := "/host/"
		id, err := homePluginID(r, extensionHost, dev)
		if err != nil {
			slog.Error("read home plugin", "error", err)
		}
		if plugin, ok := extensionHost.Get(id); ok && plugin.Enabled && plugin.Status == host.StatusRunning && plugin.HasWeb {
			target = plugin.WebPath
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	})
	mux.HandleFunc("GET /api/v1/host/home", func(w http.ResponseWriter, r *http.Request) {
		id, err := homePluginID(r, extensionHost, dev)
		if err != nil {
			slog.Error("read home plugin", "error", err)
			writeHostJSON(w, http.StatusInternalServerError, map[string]string{"code": "home_read_failed", "message": "无法读取首页设置"})
			return
		}
		writeHostJSON(w, http.StatusOK, struct {
			PluginID    string `json:"pluginId"`
			Development bool   `json:"development"`
		}{PluginID: id, Development: dev != nil})
	})
	mux.HandleFunc("PUT /api/v1/host/home", func(w http.ResponseWriter, r *http.Request) {
		if dev != nil {
			writeHostJSON(w, http.StatusConflict, map[string]string{"code": "development_mode", "message": "开发模式自动选择首页"})
			return
		}
		var body struct {
			PluginID *string `json:"pluginId"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if err := decoder.Decode(&body); err != nil || body.PluginID == nil || decoder.Decode(new(any)) != io.EOF {
			writeHostJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "需要提供 pluginId"})
			return
		}
		if err := extensionHost.SetHomePlugin(r.Context(), *body.PluginID); err != nil {
			if errors.Is(err, host.ErrInvalidHomePlugin) {
				writeHostJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_home_plugin", "message": err.Error()})
			} else {
				slog.Error("save home plugin", "error", err)
				writeHostJSON(w, http.StatusInternalServerError, map[string]string{"code": "home_save_failed", "message": "无法保存首页设置"})
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func homePluginID(r *http.Request, extensionHost *host.Host, dev *developmentSession) (string, error) {
	if dev != nil {
		return dev.id, nil
	}
	return extensionHost.HomePlugin(r.Context())
}

func writeHostJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
