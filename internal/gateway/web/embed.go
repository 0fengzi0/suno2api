package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html assets/app.css assets/app.js
var files embed.FS

func Handler() http.Handler {
	assets, _ := fs.Sub(files, "assets")
	assetHandler := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "same-origin")
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch request.URL.Path {
		case "/":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			writer.Header().Set("Cache-Control", "no-store")
			contents, _ := files.ReadFile("index.html")
			if request.Method != http.MethodHead {
				_, _ = writer.Write(contents)
			}
		case "/assets/app.css", "/assets/app.js":
			assetHandler.ServeHTTP(writer, request)
		default:
			http.NotFound(writer, request)
		}
	})
}
