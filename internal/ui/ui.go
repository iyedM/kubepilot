// Package ui serves the self-hosted, zero-external-dependency Web UI for
// KubePilot (Phase P6: incident list and correlated evidence view).
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/*
var embeddedAssets embed.FS

// NewHandler returns an http.Handler serving "/" (index.html) and "/assets/*"
// from the compiled-in embedded filesystem.
func NewHandler() http.Handler {
	sub, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		raw, err := embeddedAssets.ReadFile("assets/index.html")
		if err != nil {
			http.Error(w, "ui asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
	return mux
}
