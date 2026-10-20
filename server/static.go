package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// web holds the compiled client, embedded from the repo-root web/
// directory at build time, so a single binary serves both the game
// server and the client.
//
//go:embed all:web
var web embed.FS

// FileServer returns an http.Handler serving the embedded client.
func FileServer() http.Handler {
	sub, err := fs.Sub(web, "web")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
