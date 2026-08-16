package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
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
	if len(paths) != 2 || paths[0] != "/api/daemons/host-1/metrics" || paths[1] != "/api/daemons/host-1/notifications" {
		t.Fatalf("paths %#v", paths)
	}
	if headers[0] != "Bearer secret" || headers[1] != "Bearer secret" {
		t.Fatalf("auth %#v", headers)
	}
	if payloads[0]["uptime_seconds"] != float64(2) {
		t.Fatalf("metrics %#v", payloads[0])
	}
}
