package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	dyauth "src.solsynth.dev/sosys/lighthouse/shared/pkg/auth"
	gen "src.solsynth.dev/sosys/lighthouse/shared/proto"
)

func TestConsoleMountsFromDist(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<!doctype html><div id=app>console</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(&config.Config{Web: config.WebConfig{DistPath: dist}}, nil, nil)

	for _, path := range []string{"/", "/daemons/d-1", "/notifications"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id=app") {
			t.Fatalf("%s = %d %q", path, response.Code, response.Body.String())
		}
	}

	api := httptest.NewRecorder()
	router.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if api.Code != http.StatusNotFound || strings.Contains(api.Body.String(), "id=app") {
		t.Fatalf("unknown API route = %d %q", api.Code, api.Body.String())
	}
}

// handoffAuthenticator accepts exactly the tokens the fake issuer mints, so a
// test cannot pass by setting a cookie the auth service would reject.
type handoffAuthenticator struct{}

func (handoffAuthenticator) Authenticate(_ context.Context, token dyauth.TokenInfo, _ *http.Request) (*dyauth.AuthResult, error) {
	if token.Token != "issued-token" && token.Token != "renewed-token" {
		return nil, context.Canceled
	}
	return &dyauth.AuthResult{Account: &gen.DyAccount{
		Id:   "account-a",
		Name: "maid",
		Nick: "Maid",
	}}, nil
}

func handoffIssuer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/handoff/redeem":
			_, _ = w.Write([]byte(`{"token":"issued-token","refresh_token":"refresh-a","expires_in":900,"refresh_expires_in":86400}`))
		case "/api/auth/handoff/renew":
			_, _ = w.Write([]byte(`{"token":"renewed-token","refresh_token":"refresh-b","expires_in":900,"refresh_expires_in":86400}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHandoffSessionLifecycle(t *testing.T) {
	issuer := handoffIssuer(t)
	defer issuer.Close()
	cfg := &config.Config{Handoff: config.HandoffConfig{
		BaseURL:  issuer.URL,
		ClientID: "maidkit-test",
	}}
	router := NewRouter(cfg, nil, handoffAuthenticator{})

	configResp := httptest.NewRecorder()
	router.ServeHTTP(configResp, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
	if configResp.Code != http.StatusOK {
		t.Fatalf("auth config = %d %s", configResp.Code, configResp.Body)
	}
	var signIn struct {
		Configured bool   `json:"configured"`
		SignInURL  string `json:"sign_in_url"`
	}
	if err := json.Unmarshal(configResp.Body.Bytes(), &signIn); err != nil {
		t.Fatal(err)
	}
	if !signIn.Configured || signIn.SignInURL != issuer.URL+"/sign-in" {
		t.Fatalf("auth config = %#v", signIn)
	}

	redeem := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"code":"code-a"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(redeem, req)
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s", redeem.Code, redeem.Body)
	}
	cookies := redeem.Result().Cookies()
	authCookie := cookieNamed(cookies, "AuthToken")
	refreshCookie := cookieNamed(cookies, "RefreshToken")
	if authCookie == nil || authCookie.Value != "issued-token" {
		t.Fatalf("AuthToken cookie = %#v", authCookie)
	}
	if refreshCookie == nil || refreshCookie.Value != "refresh-a" {
		t.Fatalf("RefreshToken cookie = %#v", refreshCookie)
	}
	if !authCookie.HttpOnly {
		t.Fatal("AuthToken cookie is not HttpOnly")
	}

	session := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.AddCookie(authCookie)
	router.ServeHTTP(session, req)
	if session.Code != http.StatusOK || !strings.Contains(session.Body.String(), `"name":"maid"`) {
		t.Fatalf("session = %d %s", session.Code, session.Body)
	}

	refresh := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.AddCookie(refreshCookie)
	router.ServeHTTP(refresh, req)
	if refresh.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", refresh.Code, refresh.Body)
	}
	authCookie = cookieNamed(refresh.Result().Cookies(), "AuthToken")
	if authCookie == nil || authCookie.Value != "renewed-token" {
		t.Fatalf("renewed AuthToken cookie = %#v", authCookie)
	}

	logout := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	router.ServeHTTP(logout, req)
	cleared := cookieNamed(logout.Result().Cookies(), "AuthToken")
	if logout.Code != http.StatusOK || cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("logout = %d %#v", logout.Code, cleared)
	}
}

func TestHandoffDisabledIsNotASignInSurface(t *testing.T) {
	router := NewRouter(&config.Config{}, nil, handoffAuthenticator{})

	configResp := httptest.NewRecorder()
	router.ServeHTTP(configResp, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
	if configResp.Code != http.StatusOK || !strings.Contains(configResp.Body.String(), `"configured":false`) {
		t.Fatalf("unconfigured auth config = %d %s", configResp.Code, configResp.Body)
	}
	redeem := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"code":"code-a"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(redeem, req)
	if redeem.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured redeem = %d %s", redeem.Code, redeem.Body)
	}
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
