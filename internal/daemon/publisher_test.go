package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	"src.solsynth.dev/solsynth/maidcafe/internal/server"
)

func TestCloudPublisherPostsMetricsAndNotifications(t *testing.T) {
	var mu sync.Mutex
	paths := []string{}
	headers := []string{}
	payloads := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		headers = append(headers, r.Header.Get("Authorization"))
		payloads = append(payloads, payload)
		mu.Unlock()
		if r.URL.Path == "/api/daemons/host-1/quota" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"workspace_id":"ws-a","quotas":{"max_daemons":10}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg := config.DaemonConfig{ID: "host-1", CloudURL: server.URL, CloudSecret: "secret", RequestTimeout: time.Second}
	publisher, err := NewCloudPublisher(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	publisher.PublishMetrics(t.Context(), MetricsPayload{SentAt: time.Now().UTC(), UptimeSeconds: 2})
	publisher.PublishNotification(t.Context(), notificationPayload{Kind: "webhook.failure", Title: "failed", Body: "oops", Metadata: map[string]any{"name": "hook"}})
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 || paths[0] != "/api/daemons/host-1/quota" || paths[1] != "/api/daemons/host-1/metrics" || paths[2] != "/api/daemons/host-1/notifications" {
		t.Fatalf("paths %#v", paths)
	}
	if headers[0] != "Bearer secret" || headers[1] != "Bearer secret" || headers[2] != "Bearer secret" {
		t.Fatalf("auth %#v", headers)
	}
	if payloads[1]["uptime_seconds"] != float64(2) {
		t.Fatalf("metrics %#v", payloads[1])
	}
}

// pacingWorkspaces serves a workspace quota with a long polling interval so
// the daemon-side pace gate engages, mirroring a throttled plan.
type pacingWorkspaces struct{ relayWorkspaces }

func (pacingWorkspaces) GetPlanQuota(_ context.Context, _ string) (map[string]int64, error) {
	return map[string]int64{"max_daemons": 10, "polling_interval_seconds": 3600}, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCloudPublisherPacesThrottledTrafficByWorkspaceQuota(t *testing.T) {
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, nil, pacingWorkspaces{})
	cloudServer := httptest.NewServer(server.NewRouter(nil, svc, nil))
	t.Cleanup(cloudServer.Close)
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}

	cfg := relayDaemonConfig(daemon.ID, cloudServer.URL, daemon.Secret, "/bin/true")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher, err := NewCloudPublisher(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	relay := NewWebhookRelay(publisher, NewWebhookExecutor(cfg), nil, logger)

	var mu sync.Mutex
	metricPosts, pendingGets, notificationPosts := 0, 0, 0
	publisher.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/daemons/" + daemon.ID + "/metrics":
			mu.Lock()
			metricPosts++
			mu.Unlock()
		case "/api/daemons/" + daemon.ID + "/webhook-requests/pending":
			mu.Lock()
			pendingGets++
			mu.Unlock()
		case "/api/daemons/" + daemon.ID + "/notifications":
			mu.Lock()
			notificationPosts++
			mu.Unlock()
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	// Metric ingest and relay pickup share one pace slot: the first request
	// passes and records it, everything else inside the 3600s window is
	// skipped client-side instead of burning a guaranteed 429. Ungated paths
	// (notifications) keep flowing.
	publisher.PublishMetrics(ctx, MetricsPayload{SentAt: time.Now().UTC(), UptimeSeconds: 1})
	relay.pollOnce(ctx)
	publisher.PublishMetrics(ctx, MetricsPayload{SentAt: time.Now().UTC(), UptimeSeconds: 2})
	publisher.PublishNotification(ctx, notificationPayload{Kind: "daemon.notification", Title: "still flows", Body: "unthrottled"})

	mu.Lock()
	defer mu.Unlock()
	if metricPosts != 1 {
		t.Fatalf("metric posts = %d, want 1 (second publish should be paced)", metricPosts)
	}
	if pendingGets != 0 {
		t.Fatalf("pending gets = %d, want 0 (relay pickup inside the poll interval)", pendingGets)
	}
	if notificationPosts != 1 {
		t.Fatalf("notification posts = %d, want 1 (unthrottled path must not be paced)", notificationPosts)
	}
}
