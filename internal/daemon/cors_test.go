package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		origin   string
		host     string
		want     bool
	}{
		{
			name:   "the daemon's own host needs no list",
			origin: "http://127.0.0.1:8747",
			host:   "127.0.0.1:8747",
			want:   true,
		},
		{
			name:   "the same host in another case is the same host",
			origin: "https://C01.example",
			host:   "c01.example",
			want:   true,
		},
		{
			name:     "a listed host",
			patterns: []string{"mkw.solsynth.dev"},
			origin:   "https://mkw.solsynth.dev",
			want:     true,
		},
		{
			name:     "a listed host does not cover another port",
			patterns: []string{"mkw.solsynth.dev"},
			origin:   "https://mkw.solsynth.dev:8443",
			want:     false,
		},
		{
			// The scheme is only part of the pattern when the pattern says so,
			// which is how an operator pins one origin to https.
			name:     "a host pattern ignores the scheme",
			patterns: []string{"mkw.solsynth.dev"},
			origin:   "http://mkw.solsynth.dev",
			want:     true,
		},
		{
			name:     "a scheme-qualified pattern only matches that scheme",
			patterns: []string{"https://mkw.solsynth.dev"},
			origin:   "http://mkw.solsynth.dev",
			want:     false,
		},
		{
			name:     "a wildcard host",
			patterns: []string{"*.solsynth.dev"},
			origin:   "https://mkw.solsynth.dev",
			want:     true,
		},
		{
			name:     "an unlisted origin",
			patterns: []string{"mkw.solsynth.dev"},
			origin:   "https://evil.example",
			want:     false,
		},
		{
			name:   "an empty list allows only the daemon's own host",
			origin: "https://mkw.solsynth.dev",
			want:   false,
		},
		{
			name:     "an origin that is not a URL",
			patterns: []string{"*"},
			origin:   "null",
			want:     false,
		},
		{
			name:     "an unparseable pattern matches nothing",
			patterns: []string{"["},
			origin:   "https://mkw.solsynth.dev",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := originAllowed(tc.patterns, tc.origin, tc.host)
			if got != tc.want {
				t.Fatalf("originAllowed(%v, %q, %q) = %v, want %v", tc.patterns, tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

// The middleware is exercised through a real engine, because the interesting
// path is a preflight for a route that has no OPTIONS handler: gin routes it to
// its not-found chain, and the answer has to come from the middleware there.
func TestCorsMiddlewareAnswersPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	policy := config.TerminalConfig{AllowedOrigins: []string{"mkw.solsynth.dev"}}
	engine := gin.New()
	engine.Use(corsMiddleware(func() config.TerminalConfig { return policy }))
	engine.GET("/api/v1/metrics", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	head := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("an allowed origin gets the preflight answer", func(t *testing.T) {
		recorder := head(http.MethodOptions, "/api/v1/metrics", map[string]string{
			"Origin":                        "https://mkw.solsynth.dev",
			"Access-Control-Request-Method": "GET",
		})

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
		}
		header := recorder.Header()
		if got := header.Get("Access-Control-Allow-Origin"); got != "https://mkw.solsynth.dev" {
			t.Fatalf("allow-origin = %q", got)
		}
		if got := header.Get("Access-Control-Allow-Methods"); got != corsAllowedMethods {
			t.Fatalf("allow-methods = %q", got)
		}
		if got := header.Get("Access-Control-Allow-Headers"); got != corsAllowedHeaders {
			t.Fatalf("allow-headers = %q", got)
		}
		if got := header.Get("Access-Control-Max-Age"); got != corsMaxAge {
			t.Fatalf("max-age = %q", got)
		}
		// A browser never attaches a cookie here, and the daemon never says it
		// may: every credential this API takes is an Authorization header.
		if got := header.Get("Access-Control-Allow-Credentials"); got != "" {
			t.Fatalf("allow-credentials = %q, want empty", got)
		}
		if got := header.Get("Vary"); got != "Origin" {
			t.Fatalf("vary = %q, want Origin", got)
		}
	})

	t.Run("a client's own headers are echoed back", func(t *testing.T) {
		recorder := head(http.MethodOptions, "/api/v1/metrics", map[string]string{
			"Origin":                         "https://mkw.solsynth.dev",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "Authorization, X-MaidCafe-Signature",
		})

		if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, X-MaidCafe-Signature" {
			t.Fatalf("allow-headers = %q", got)
		}
	})

	t.Run("an unlisted origin is told nothing", func(t *testing.T) {
		recorder := head(http.MethodOptions, "/api/v1/metrics", map[string]string{
			"Origin":                        "https://evil.example",
			"Access-Control-Request-Method": "GET",
		})

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("allow-origin = %q, want empty", got)
		}
		// The answer still varies by origin, so a cache cannot hand an allowed
		// caller the answer given to this one.
		if got := recorder.Header().Get("Vary"); got != "Origin" {
			t.Fatalf("vary = %q, want Origin", got)
		}
	})

	t.Run("a real request carries the origin and exports its headers", func(t *testing.T) {
		recorder := head(http.MethodGet, "/api/v1/metrics", map[string]string{
			"Origin":        "https://mkw.solsynth.dev",
			"Authorization": "Bearer metrics-secret",
		})

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		header := recorder.Header()
		if got := header.Get("Access-Control-Allow-Origin"); got != "https://mkw.solsynth.dev" {
			t.Fatalf("allow-origin = %q", got)
		}
		if got := header.Get("Access-Control-Expose-Headers"); got != corsExposedHeaders {
			t.Fatalf("expose-headers = %q", got)
		}
		if got := header.Get("Access-Control-Allow-Methods"); got != "" {
			t.Fatalf("allow-methods = %q, want empty on a real request", got)
		}
	})

	t.Run("a request without an origin is untouched", func(t *testing.T) {
		recorder := head(http.MethodGet, "/api/v1/metrics", map[string]string{
			"Authorization": "Bearer metrics-secret",
		})

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("allow-origin = %q, want empty", got)
		}
		if got := recorder.Header().Get("Vary"); got != "" {
			t.Fatalf("vary = %q, want empty", got)
		}
	})
}

// The list is the terminal's, so a reload that changes it has to change this
// answer too, without a restart.
func TestCorsMiddlewareFollowsReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	policy := config.TerminalConfig{}
	engine := gin.New()
	engine.Use(corsMiddleware(func() config.TerminalConfig { return policy }))
	engine.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Header.Set("Origin", "https://mkw.solsynth.dev")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, r)
		return recorder
	}

	if got := request().Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("before the reload allow-origin = %q, want empty", got)
	}
	policy.AllowedOrigins = []string{"mkw.solsynth.dev"}
	if got := request().Header().Get("Access-Control-Allow-Origin"); got != "https://mkw.solsynth.dev" {
		t.Fatalf("after the reload allow-origin = %q", got)
	}
}
