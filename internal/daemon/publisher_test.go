package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	publisherBox := &atomic.Pointer[CloudPublisher]{}
	publisherBox.Store(publisher)
	relay := NewWebhookRelay(publisherBox, NewWebhookExecutor(cfg), nil, logger)

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

	// Metric ingest is paced by the 3600s window: the first publish passes,
	// the second is skipped client-side instead of burning a guaranteed 429.
	// Relay pickup and notifications are unthrottled by design.
	publisher.PublishMetrics(ctx, MetricsPayload{SentAt: time.Now().UTC(), UptimeSeconds: 1})
	relay.pollOnce(ctx)
	publisher.PublishMetrics(ctx, MetricsPayload{SentAt: time.Now().UTC(), UptimeSeconds: 2})
	publisher.PublishNotification(ctx, notificationPayload{Kind: "daemon.notification", Title: "still flows", Body: "unthrottled"})

	mu.Lock()
	defer mu.Unlock()
	if metricPosts != 1 {
		t.Fatalf("metric posts = %d, want 1 (second publish should be paced)", metricPosts)
	}
	if pendingGets != 1 {
		t.Fatalf("pending gets = %d, want 1 (relay pickup is not paced)", pendingGets)
	}
	if notificationPosts != 1 {
		t.Fatalf("notification posts = %d, want 1 (unthrottled path must not be paced)", notificationPosts)
	}
}
func TestCloudPublisherPacesMetricsInsideQuotaWindow(t *testing.T) {
	publisher := &CloudPublisher{
		pollInterval:   time.Hour,
		lastQuotaFetch: time.Now(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	if !publisher.pacedOK(ctx) {
		t.Fatal("first publish should claim an unused pace slot")
	}
	publisher.pacedDone(true)

	if publisher.pacedOK(ctx) {
		t.Fatal("second publish inside the window should be paced")
	}

	publisher.paceMu.Lock()
	publisher.lastPaced = time.Now().Add(-2 * time.Hour)
	publisher.paceMu.Unlock()
	if !publisher.pacedOK(ctx) {
		t.Fatal("publish should claim the next available slot")
	}
	publisher.pacedDone(true)
}

func TestCloudPublisherDoesNotConsumePaceSlotOnFailedMetric(t *testing.T) {
	metricPosts := 0
	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemons/host-1/quota":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quotas":{"polling_interval_seconds":3600}}`))
		case "/api/daemons/host-1/metrics":
			metricPosts++
			if metricPosts == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer cloudServer.Close()

	publisher, err := NewCloudPublisher(config.DaemonConfig{
		ID:             "host-1",
		CloudURL:       cloudServer.URL,
		CloudSecret:    "secret",
		RequestTimeout: time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	publisher.PublishMetrics(context.Background(), MetricsPayload{SentAt: time.Now().UTC()})
	publisher.PublishMetrics(context.Background(), MetricsPayload{SentAt: time.Now().UTC()})
	if metricPosts != 2 {
		t.Fatalf("metric posts after transient failure = %d, want 2", metricPosts)
	}
}

// TestCloudPublisherUploadsHealthScore proves the whole upload path end to
// end: the daemon posts a metric carrying its health score and the cloud's
// strict decoder accepts the health fields, stores them, and serves them back.
func TestCloudPublisherUploadsHealthScore(t *testing.T) {
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, nil, relayWorkspaces{})
	cloudServer := httptest.NewServer(server.NewRouter(nil, svc, nil))
	t.Cleanup(cloudServer.Close)
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}

	cfg := relayDaemonConfig(daemon.ID, cloudServer.URL, daemon.Secret, "/bin/true")
	publisher, err := NewCloudPublisher(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	publisher.PublishMetrics(ctx, MetricsPayload{
		SentAt: time.Now().UTC(), HostID: "host", UptimeSeconds: 10,
		HealthScore: 63, HealthStatus: HealthDegraded,
	})

	health, err := svc.DaemonHealth(ctx, "account-a", daemon.ID)
	if err != nil {
		t.Fatalf("uploaded health not stored: %v", err)
	}
	if health.Score != 63 || health.Status != HealthDegraded {
		t.Fatalf("stored health = %+v, want 63/degraded", health)
	}
	history, err := svc.ListMetrics(ctx, "account-a", daemon.ID, 100, nil)
	if err != nil || len(history) != 1 {
		t.Fatalf("metric history: %v %#v", err, history)
	}
	if history[0].HealthScore != 63 || history[0].HealthStatus != HealthDegraded {
		t.Fatalf("metric lost health fields: %+v", history[0])
	}
}

// TestContainerStatusBatchCoversEnumeratedRuntimes pins what the daemon tells
// the cloud a snapshot is: every managed container carries its runtime, and
// only the runtimes that answered are covered, so a runtime whose listing
// failed keeps its containers' last known state instead of losing them to a
// hiccup.
func TestContainerStatusBatchCoversEnumeratedRuntimes(t *testing.T) {
	failure := "list containers: exit status 125"
	rt := &reloadableConfig{managedComposes: []string{"drasl"}}
	payload := containersPayload{Runtimes: []containersRuntimePayload{
		{Runtime: "podman", Available: true, Containers: []containerEntry{
			{ID: "abc", Name: "drasl_drasl_1", State: "running", ComposeProject: "drasl"},
			{ID: "def", Name: "other_thing_1", State: "running", ComposeProject: "other"},
		}},
		{Runtime: "docker", Available: true, Error: &failure},
	}}

	entries, covered := containerStatusBatch(payload, rt)
	if len(covered) != 1 || covered[0] != "podman" {
		t.Fatalf("covered = %#v, want only the runtime that answered", covered)
	}
	if len(entries) != 1 || entries[0].ContainerID != "abc" || entries[0].Runtime != "podman" {
		t.Fatalf("entries = %#v, want just the managed podman container", entries)
	}

	// A runtime that answered with nothing is still covered: that empty set is
	// how the cloud learns the containers it holds for that runtime are gone.
	empty := containersPayload{Runtimes: []containersRuntimePayload{{Runtime: "podman", Available: true}}}
	entries, covered = containerStatusBatch(empty, rt)
	if len(entries) != 0 || len(covered) != 1 || covered[0] != "podman" {
		t.Fatalf("empty snapshot = %#v covered by %#v", entries, covered)
	}
}

// TestPublishContainerStatusPostsCoverage is the smoke for the publish path
// itself: a snapshot that covers a runtime is posted to the cloud in the shape
// the cloud decodes, and a snapshot that covers none is not posted at all — it
// would tell the cloud nothing about what is absent.
func TestPublishContainerStatusPostsCoverage(t *testing.T) {
	var mu sync.Mutex
	bodies := []cloud.ContainerStatusBatchInput{}
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var batch cloud.ContainerStatusBatchInput
		_ = json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, batch)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/quota") {
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
	ctx := t.Context()

	// Nothing enumerated: no publish, so the cloud keeps what it has.
	publisher.PublishContainerStatus(ctx, containerStatusPayload{
		SentAt: time.Now().UTC(), Runtimes: nil,
		Containers: []containerStatusEntry{{ContainerID: "abc", Runtime: "podman"}},
	})

	publisher.PublishContainerStatus(ctx, containerStatusPayload{
		SentAt:   time.Now().UTC(),
		Runtimes: []string{"podman"},
		Containers: []containerStatusEntry{{
			ContainerID: "abc", Runtime: "podman", Name: "drasl_drasl_1", State: "running", ComposeProject: "drasl",
		}},
	})

	mu.Lock()
	defer mu.Unlock()
	posted := 0
	for i, path := range paths {
		if !strings.HasSuffix(path, "/containers") {
			continue
		}
		posted++
		if len(bodies[i].Runtimes) != 1 || bodies[i].Runtimes[0] != "podman" {
			t.Fatalf("posted batch lost its coverage: %+v", bodies[i])
		}
		if len(bodies[i].Containers) != 1 || bodies[i].Containers[0].Runtime != "podman" || bodies[i].Containers[0].ContainerID != "abc" {
			t.Fatalf("posted batch lost a container: %+v", bodies[i])
		}
	}
	if posted != 1 {
		t.Fatalf("container publishes = %d in %#v, want exactly the covering snapshot", posted, paths)
	}
}
