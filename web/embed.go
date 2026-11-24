// Package web serves the standalone godesk-ext management page.
package web

import (
	"embed"
	"net/http"
)

//go:embed index.html app.js style.css
var content embed.FS

// Handler serves the management page and its assets.
func Handler() http.Handler {
	fileServer := http.FileServer(http.FS(content))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
