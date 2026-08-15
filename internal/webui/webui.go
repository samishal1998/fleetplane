// Package webui serves the embedded web dashboard at /ui/ on the main API
// listener. The dashboard is a static Vue single-page app compiled into the
// binary (no build step, no external requests): it talks to the same /v1
// API the CLI uses, authenticating with a pasted API token. Assets are
// public; every piece of fleet data still goes through the authenticated
// API, so the static handler itself needs no auth.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var distFS embed.FS

// Handler serves the dashboard under the /ui/ prefix.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded dist missing: " + err.Error()) // impossible: compile-time embed
	}
	files := http.StripPrefix("/ui/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 'unsafe-eval' is required by Vue's in-browser template compiler
		// (the vendored global build); everything else stays same-origin.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		if strings.HasPrefix(r.URL.Path, "/ui/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
