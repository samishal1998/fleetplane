package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestServesIndex(t *testing.T) {
	rec := get(t, "/ui/")
	if rec.Code != 200 {
		t.Fatalf("GET /ui/ = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "app.js") {
		t.Fatal("index.html does not reference app.js")
	}
	// theme.js applies the saved theme before first paint. It has to stay a
	// separate file: the CSP below allows 'self' but not inline scripts.
	if !strings.Contains(rec.Body.String(), "theme.js") {
		t.Fatal("index.html does not reference theme.js")
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatal("index.html has an inline script; the CSP blocks it")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("missing CSP, got %q", csp)
	}
}

func TestServesAssets(t *testing.T) {
	for path, ctPrefix := range map[string]string{
		"/ui/style.css":                 "text/css",
		"/ui/app.js":                    "text/javascript",
		"/ui/theme.js":                  "text/javascript",
		"/ui/vendor/vue.global.prod.js": "text/javascript",
		"/ui/logo.svg":                  "image/svg+xml",
	} {
		rec := get(t, path)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, ctPrefix) {
			t.Fatalf("GET %s content type = %q, want prefix %q", path, ct, ctPrefix)
		}
	}
}

func TestVendorIsCacheable(t *testing.T) {
	if cc := get(t, "/ui/vendor/vue.global.prod.js").Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Fatalf("vendor Cache-Control = %q", cc)
	}
	if cc := get(t, "/ui/").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control = %q", cc)
	}
}

func TestMissingAssetIs404(t *testing.T) {
	if rec := get(t, "/ui/nope.js"); rec.Code != 404 {
		t.Fatalf("GET /ui/nope.js = %d", rec.Code)
	}
}
