package router

import (
	"embed"
	"io/fs"
	"log"
)

//go:embed web
var webFS embed.FS

// webAssetsFS returns the embedded frontend assets subtree.
func webAssetsFS() fs.FS {
	sub, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		log.Fatalf("embedded assets unavailable: %v", err)
	}
	return sub
}

// webIndex returns the embedded SPA index.html.
func webIndex() ([]byte, error) {
	return webFS.ReadFile("web/index.html")
}
