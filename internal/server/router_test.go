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
)

type routePublisher struct{}

func (routePublisher) Publish(context.Context, cloud.NotificationEvent) error { return nil }

// routeWorkspaces grants account-a member access to ws-a only, mirroring the
// production DyWorkspaceService contract.
type routeWorkspaces struct{}

func (routeWorkspaces) IsMemberWithRole(_ context.Context, workspaceID, accountID string, _ []int32) (bool, error) {
	return workspaceID == "ws-a" && accountID == "account-a", nil
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
