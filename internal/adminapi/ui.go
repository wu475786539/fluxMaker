package adminapi

import (
	"embed"
	"net/http"
)

//go:embed web/*
var webFiles embed.FS

func (s *Server) registerUI() {
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveWebFile(w, "web/index.html", "text/html; charset=utf-8")
	})
	s.mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		serveWebFile(w, "web/app.js", "text/javascript; charset=utf-8")
	})
	s.mux.HandleFunc("GET /styles.css", func(w http.ResponseWriter, r *http.Request) {
		serveWebFile(w, "web/styles.css", "text/css; charset=utf-8")
	})
}

func serveWebFile(w http.ResponseWriter, path, contentType string) {
	data, err := webFiles.ReadFile(path)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}
