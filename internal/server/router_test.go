package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
)

type routePublisher struct{}

func (routePublisher) Publish(context.Context, cloud.NotificationEvent) error { return nil }

// routeAuthenticator authenticates every Solar token as account-a, so user
// routes can be exercised without a live auth service.
type routeAuthenticator struct{}

func (routeAuthenticator) Authenticate(_ context.Context, _ dyauth.TokenInfo, _ *http.Request) (*dyauth.AuthResult, error) {
	return &dyauth.AuthResult{Account: &gen.DyAccount{Id: "account-a"}}, nil
}

// routeWorkspaces grants account-a member access to ws-a only, mirroring the
// production DyWorkspaceService contract.
type routeWorkspaces struct{}

func (routeWorkspaces) IsMemberWithRole(_ context.Context, workspaceID, accountID string, _ []int32) (bool, error) {
	return workspaceID == "ws-a" && accountID == "account-a", nil
}

func (routeWorkspaces) GetPlanQuota(_ context.Context, workspaceID string) (map[string]int64, error) {
	return map[string]int64{"max_daemons": 10}, nil
}

func TestCloudHealthAndCredentialBoundary(t *testing.T) {
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, routePublisher{}, routeWorkspaces{})
	router := NewRouter(nil, svc, nil)

	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health %d", health.Code)
	}
	var healthJSON map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &healthJSON); err != nil || healthJSON["mode"] != "cloud" {
		t.Fatalf("health body %s", health.Body)
	}
	landing := httptest.NewRecorder()
	router.ServeHTTP(landing, httptest.NewRequest(http.MethodGet, "/", nil))
	if landing.Code != http.StatusOK ||
		!strings.Contains(landing.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(landing.Body.String(), "MaidKit Cloud is up and running") {
		t.Fatalf("landing page response %d %q", landing.Code, landing.Body.String())
	}

	favicon := httptest.NewRecorder()
	router.ServeHTTP(favicon, httptest.NewRequest(http.MethodGet, "/favicon.png", nil))
	if favicon.Code != http.StatusOK || !strings.Contains(favicon.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("favicon response %d %q", favicon.Code, favicon.Header().Get("Content-Type"))
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		unauth := httptest.NewRecorder()
		router.ServeHTTP(unauth, httptest.NewRequest(method, "/api/daemons", strings.NewReader(`{"name":"host"}`)))
		if unauth.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s user route %d", method, unauth.Code)
		}
	}

	daemon, err := svc.CreateDaemon(context.Background(), "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	metric := httptest.NewRequest(http.MethodPost, "/api/daemons/"+daemon.ID+"/metrics", strings.NewReader(`{"sent_at":"2026-08-12T00:00:00Z"}`))
	metric.Header.Set("Authorization", "Bearer "+daemon.Secret)
	got := httptest.NewRecorder()
	router.ServeHTTP(got, metric)
	if got.Code != http.StatusNoContent {
		t.Fatalf("daemon metric route %d %s", got.Code, got.Body)
	}
}

func TestWorkspaceQuotaUserRoute(t *testing.T) {
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, routePublisher{}, routeWorkspaces{})
	router := NewRouter(nil, svc, routeAuthenticator{})

	unauth := httptest.NewRecorder()
	router.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/workspaces/ws-a/quota", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated quota route %d", unauth.Code)
	}

	// A user-level API credential authenticates as its account, so no Solar
	// auth service is needed for the member and non-member cases.
	cred, err := svc.CreateCredential(context.Background(), "account-a", "ci", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+cred.Token)
		got := httptest.NewRecorder()
		router.ServeHTTP(got, req)
		return got
	}

	member := get("/api/workspaces/ws-a/quota")
	if member.Code != http.StatusOK {
		t.Fatalf("member quota route %d %s", member.Code, member.Body)
	}
	var view cloud.WorkspaceQuotaView
	if err := json.Unmarshal(member.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.WorkspaceID != "ws-a" || view.Quotas["max_daemons"] != 10 {
		t.Fatalf("quota view %#v", view)
	}

	if got := get("/api/workspaces/ws-b/quota"); got.Code != http.StatusForbidden {
		t.Fatalf("non-member quota route %d", got.Code)
	}
}
