package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestHTTPControlAPIReportsVersionMetricsAndActions(t *testing.T) {
	action := executable(t, "#!/bin/sh\nprintf '%s' action-ok\n")
	cfg := config.DaemonConfig{
		ID:                "host-1",
		Version:           "v1.2.3",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
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
	webhookRequest.Header.Set("Authorization", "Bearer metrics-secret")
	webhookResponse, err := http.DefaultClient.Do(webhookRequest)
	if err != nil {
		t.Fatal(err)
	}
	webhookResponse.Body.Close()
	if webhookResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics secret accepted for webhook: status = %d", webhookResponse.StatusCode)
	}
	webhookRequest, err = http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/webhooks/hook",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	webhookRequest.Header.Set("Authorization", "Bearer webhook-secret")
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
}
