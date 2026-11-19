package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<!doctype html><div id=app>console</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app-abc.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := New(dist)
	if err != nil {
		t.Fatal(err)
	}
	if !handler.Enabled() {
		t.Fatal("handler is disabled")
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(handler.ServeStatic)
	router.NoRoute(handler.Serve)
	return router, dist
}

func TestSPAFallbackAndAssets(t *testing.T) {
	router, _ := newTestRouter(t)

	for _, path := range []string{"/", "/daemons/abc", "/notifications"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id=app") {
			t.Fatalf("%s = %d %q", path, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s cache-control = %q", path, response.Header().Get("Cache-Control"))
		}
	}

	asset := httptest.NewRecorder()
	router.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app-abc.js", nil))
	if asset.Code != http.StatusOK || asset.Body.String() != "console.log(1)" {
		t.Fatalf("asset = %d %q", asset.Code, asset.Body.String())
	}
	if !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset cache-control = %q", asset.Header().Get("Cache-Control"))
	}
}

func TestReservedAndMissingPathsStayJSON(t *testing.T) {
	router, _ := newTestRouter(t)

	for _, path := range []string{"/api/unknown", "/health/missing", "/assets/missing.js", "/..%2fsecret"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s = %d %q", path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"error":"not found"`) {
			t.Fatalf("%s body = %q", path, response.Body.String())
		}
	}
}

func TestMissingDistDisablesHandler(t *testing.T) {
	handler, err := New(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("expected an error for a missing dist")
	}
	if handler.Enabled() {
		t.Fatal("missing dist left the handler enabled")
	}
	empty, err := New("")
	if err != nil || empty.Enabled() {
		t.Fatalf("empty dist = %#v, %v", empty, err)
	}
}
