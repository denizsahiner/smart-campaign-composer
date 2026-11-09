package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// webDir lays out a built frontend like web/dist, with a secret file next to it that
// must never be served.
func webDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "dist")
	files := map[string]string{
		"index.html":           "<!doctype html><div id=app></div>",
		"favicon.svg":          "<svg/>",
		"assets/index-abc.js":  "console.log(1)",
		"assets/index-abc.css": "body{}",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// The handler is built without a database: none of these requests reach one.
func staticServer(t *testing.T) http.Handler {
	return NewServer(nil, nil, nil).WithStatic(webDir(t)).Handler()
}

func TestStaticServesFilesAndTheAppShell(t *testing.T) {
	h := staticServer(t)
	cases := []struct {
		path, body, cache string
	}{
		{"/", "<div id=app>", "no-cache"},
		{"/campaigns/7", "<div id=app>", "no-cache"}, // a client-side route, after a reload
		{"/monitoring", "<div id=app>", "no-cache"},
		{"/assets/index-abc.js", "console.log(1)", "public, max-age=31536000, immutable"},
		{"/favicon.svg", "<svg/>", ""},
	}
	for _, c := range cases {
		rec := serve(h, http.MethodGet, c.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", c.path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("GET %s: body %q, want it to contain %q", c.path, rec.Body.String(), c.body)
		}
		if got := rec.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("GET %s: Cache-Control %q, want %q", c.path, got, c.cache)
		}
	}
}

func TestStaticMissingAssetIs404NotHTML(t *testing.T) {
	rec := serve(staticServer(t), http.MethodGet, "/assets/index-old.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<div id=app>") {
		t.Fatal("a missing asset was answered with index.html")
	}
}

func TestStaticUnknownAPIPathIsJSON404(t *testing.T) {
	rec := serve(staticServer(t), http.MethodGet, "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	var body map[string]apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"].Code != "not_found" {
		t.Fatalf("body %q, want a JSON not_found error", rec.Body.String())
	}
}

func TestStaticKeepsMethodNotAllowedOnAPIRoutes(t *testing.T) {
	// GET-only route, wrong method: the mux's 405, not the frontend.
	rec := serve(staticServer(t), http.MethodPost, "/api/campaigns/7")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestStaticStaysInsideItsDirectory(t *testing.T) {
	h := staticHandler(webDir(t))
	for _, p := range []string{"/../secret.txt", "/assets/../../secret.txt", "/%2e%2e/secret.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = p // set directly: NewRequest and the mux would clean it first
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("GET %s served a file outside the web directory", p)
		}
	}
}

func TestNoStaticWithoutWebDir(t *testing.T) {
	rec := serve(NewServer(nil, nil, nil).Handler(), http.MethodGet, "/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (API only)", rec.Code)
	}
}
