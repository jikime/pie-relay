package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed admin/*
var adminAssets embed.FS

func adminHandler() http.Handler {
	root, _ := fs.Sub(adminAssets, "admin")
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, "/admin/")
		files.ServeHTTP(w, clone)
	})
}
