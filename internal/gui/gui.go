// Package gui embeds the single-page management UI.
package gui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed layout is fixed at compile time
	}
	return http.FileServer(http.FS(sub))
}
