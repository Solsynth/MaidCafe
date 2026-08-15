package daemon

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func signedHeader(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHTTPControlAPIReportsVersionMetricsAndActions(t *testing.T) {
	action := executable(t, "#!/bin/sh\nprintf '%s' action-ok\n")
	cfg := config.DaemonConfig{
		ID:                "host-1",
		Version:           "v1.2.3",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		ProcessesLimit:    50,
		RequestTimeout:    5 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		AuditPath:         filepath.Join(t.TempDir(), "audit.jsonl"),
		Actions: []config.WebhookConfig{
			{Name: "backup", Command: action, Enabled: true},
		},
		Webhooks: []config.WebhookConfig{
			{Name: "hook", Secret: "webhook-secret", Command: action, Enabled: true},
		},
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	baseURL := "http://" + app.ListenAddr()
	unauthorizedMetrics, err := http.Get(baseURL + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedMetrics.Body.Close()
	if unauthorizedMetrics.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized metrics status = %d", unauthorizedMetrics.StatusCode)
	}
	webhookRequest, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/webhooks/hook",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	webhookRequest.Header.Set(
		"X-MaidCafe-Signature",
		signedHeader("metrics-secret", []byte("payload")),
	)
	webhookResponse, err := http.DefaultClient.Do(webhookRequest)
	if err != nil {
		t.Fatal(err)
	}
	webhookResponse.Body.Close()
	if webhookResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong webhook signature accepted: status = %d", webhookResponse.StatusCode)
	}
	webhookRequest, err = http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/webhooks/hook",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	webhookRequest.Header.Set(
		"X-MaidCafe-Signature",
		signedHeader("webhook-secret", []byte("payload")),
	)
	webhookResponse, err = http.DefaultClient.Do(webhookRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer webhookResponse.Body.Close()
	if webhookResponse.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d", webhookResponse.StatusCode)
	}
	healthRequest, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	healthRequest.Header.Set("Authorization", "Bearer metrics-secret")
	health, err := http.DefaultClient.Do(healthRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	var healthBody map[string]any
	if err := json.NewDecoder(health.Body).Decode(&healthBody); err != nil {
		t.Fatal(err)
	}
	if healthBody["version"] != "v1.2.3" {
		t.Fatalf("health version = %v", healthBody["version"])
	}

	metricsRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	metricsRequest.Header.Set("Authorization", "Bearer metrics-secret")
	metrics, err := http.DefaultClient.Do(metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", metrics.StatusCode)
	}
	sample := app.metrics.Record()
	historyURL := baseURL + "/api/v1/metrics/history?limit=1&from=" +
		url.QueryEscape(sample.SentAt.Add(-time.Second).Format(time.RFC3339Nano)) +
		"&to=" + url.QueryEscape(sample.SentAt.Add(time.Second).Format(time.RFC3339Nano))
	historyRequest, err := http.NewRequest(
		http.MethodGet,
		historyURL,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	historyRequest.Header.Set("Authorization", "Bearer metrics-secret")
	history, err := http.DefaultClient.Do(historyRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Body.Close()
	if history.StatusCode != http.StatusOK {
		t.Fatalf("metrics history status = %d", history.StatusCode)
	}
	var historyBody struct {
		Metrics []MetricsPayload `json:"metrics"`
	}
	if err := json.NewDecoder(history.Body).Decode(&historyBody); err != nil {
		t.Fatal(err)
	}
	if len(historyBody.Metrics) != 1 {
		t.Fatalf("metrics history length = %d", len(historyBody.Metrics))
	}

	actionRequest, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/actions/backup",
		strings.NewReader(`{"job":"incremental"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	actionRequest.Header.Set("Authorization", "Bearer metrics-secret")
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.Header.Set(
		"X-MaidCafe-Signature",
		signedHeader("wrong-secret", []byte(`{"job":"incremental"}`)),
	)
	badSignature, err := http.DefaultClient.Do(actionRequest)
	if err != nil {
		t.Fatal(err)
	}
	badSignature.Body.Close()
	if badSignature.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong action signature accepted: status = %d", badSignature.StatusCode)
	}

	actionRequest, err = http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/actions/backup",
		strings.NewReader(`{"job":"incremental"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	actionRequest.Header.Set("Authorization", "Bearer metrics-secret")
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.Header.Set(
		"X-MaidCafe-Signature",
		signedHeader("metrics-secret", []byte(`{"job":"incremental"}`)),
	)
	actionResponse, err := http.DefaultClient.Do(actionRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer actionResponse.Body.Close()
	if actionResponse.StatusCode != http.StatusOK {
		t.Fatalf("action status = %d", actionResponse.StatusCode)
	}
	body, err := io.ReadAll(actionResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "action-ok") {
		t.Fatalf("action response = %s", body)
	}

	// The run is durably recorded and readable over the audit endpoint.
	auditRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	auditRequest.Header.Set("Authorization", "Bearer metrics-secret")
	auditResponse, err := http.DefaultClient.Do(auditRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer auditResponse.Body.Close()
	if auditResponse.StatusCode != http.StatusOK {
		t.Fatalf("audit status = %d", auditResponse.StatusCode)
	}
	var auditBody struct {
		Entries []auditEntry `json:"entries"`
	}
	if err := json.NewDecoder(auditResponse.Body).Decode(&auditBody); err != nil {
		t.Fatal(err)
	}
	// Both the earlier webhook run and this action run are durably recorded;
	// the newest entry (the action) comes first.
	if len(auditBody.Entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(auditBody.Entries))
	}
	entry := auditBody.Entries[0]
	if entry.Name != "backup" || entry.Source != "http" || !entry.OK {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
	webhookEntry := auditBody.Entries[1]
	if webhookEntry.Name != "hook" || webhookEntry.Source != "http" {
		t.Fatalf("unexpected webhook audit entry: %+v", webhookEntry)
	}
}

// readSSEFrame reads one SSE frame (event line, data line, blank line) from
// the buffered response body, failing the test if no complete frame arrives
// within timeout.
func readSSEFrame(t *testing.T, reader *bufio.Reader, timeout time.Duration) (string, []byte) {
	t.Helper()
	type frame struct {
		event string
		data  []byte
		err   error
	}
	result := make(chan frame, 1)
	go func() {
		var event string
		var data []byte
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- frame{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				result <- frame{event: event, data: data}
				return
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("read SSE frame: %v", got.err)
		}
		return got.event, got.data
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for SSE frame", timeout)
		return "", nil
	}
}

func TestSSEStreamHelloAndMetricFrames(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                "stream-host",
		Version:           "v9.9.9",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		StreamInterval:    50 * time.Millisecond,
		ProcessesLimit:    50,
		RequestTimeout:    5 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	baseURL := ""
	deadline := time.Now().Add(2 * time.Second)
	for baseURL == "" {
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start listening")
		}
		if addr := app.ListenAddr(); addr != "" {
			baseURL = "http://" + addr
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Missing secret is rejected before any streaming begins.
	unauthorized, err := http.Get(baseURL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream without secret status = %d", unauthorized.StatusCode)
	}

	// Unknown event names are rejected with 400.
	badEvents, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/stream?events=bogus", nil)
	if err != nil {
		t.Fatal(err)
	}
	badEvents.Header.Set("Authorization", "Bearer metrics-secret")
	badResponse, err := http.DefaultClient.Do(badEvents)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("stream with unknown event status = %d", badResponse.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/stream?events=metric", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("stream Content-Type = %q", contentType)
	}
	reader := bufio.NewReader(response.Body)

	event, data := readSSEFrame(t, reader, 2*time.Second)
	if event != "hello" {
		t.Fatalf("first frame event = %q, want hello", event)
	}
	var hello struct {
		Stream    string         `json:"stream"`
		Version   string         `json:"version"`
		Intervals map[string]int `json:"intervals"`
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("hello frame data = %q: %v", data, err)
	}
	if hello.Stream != "v1" || hello.Version != "v9.9.9" {
		t.Fatalf("hello = %#v", hello)
	}
	if _, ok := hello.Intervals["metric"]; !ok {
		t.Fatalf("hello intervals = %#v", hello.Intervals)
	}

	event, data = readSSEFrame(t, reader, 2*time.Second)
	if event != "metric" {
		t.Fatalf("second frame event = %q, want metric (data %q)", event, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("metric frame data = %q: %v", data, err)
	}
	if _, ok := payload["sent_at"]; !ok {
		t.Fatalf("metric frame missing sent_at: %#v", payload)
	}
	// Close the stream first so the handler returns and the connection goes
	// idle; only then cancel the Run context so Shutdown completes promptly.
	response.Body.Close()
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("app.Run returned: %v", err)
	}
}

func TestSnapshotEndpointsReturnPayloadsAndRequireAuth(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                "snapshot-host",
		Version:           "v9.9.9",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		ProcessesLimit:    50,
		RequestTimeout:    5 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	baseURL := ""
	deadline := time.Now().Add(2 * time.Second)
	for baseURL == "" {
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start listening")
		}
		if addr := app.ListenAddr(); addr != "" {
			baseURL = "http://" + addr
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	for _, endpoint := range []string{"/api/v1/containers", "/api/v1/images", "/api/v1/processes", "/api/v1/systemd"} {
		unauthorized, err := http.Get(baseURL + endpoint)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		unauthorized.Body.Close()
		if unauthorized.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without secret status = %d", endpoint, unauthorized.StatusCode)
		}

		request, err := http.NewRequest(http.MethodGet, baseURL+endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer metrics-secret")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d: %s", endpoint, response.StatusCode, body)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s body %q: %v", endpoint, body, err)
		}
		// Shape-only assertions: content depends on the host's runtimes/tools.
		switch endpoint {
		case "/api/v1/containers":
			if _, ok := payload["runtimes"]; !ok {
				t.Fatalf("containers payload missing runtimes: %#v", payload)
			}
		case "/api/v1/images":
			if _, ok := payload["runtimes"]; !ok {
				t.Fatalf("images payload missing runtimes: %#v", payload)
			}
		case "/api/v1/processes":
			if _, ok := payload["processes"]; !ok {
				t.Fatalf("processes payload missing processes: %#v", payload)
			}
		case "/api/v1/systemd":
			if _, ok := payload["available"]; !ok {
				t.Fatalf("systemd payload missing available: %#v", payload)
			}
		}
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("app.Run returned: %v", err)
	}
}
