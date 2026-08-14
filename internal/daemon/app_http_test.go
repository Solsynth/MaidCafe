package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
		MetricsInterval:   time.Hour,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{
			{Name: "backup", Command: action, Enabled: true},
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
	health, err := http.Get(baseURL + "/health")
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

	metrics, err := http.Get(baseURL + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", metrics.StatusCode)
	}

	actionResponse, err := http.Post(
		baseURL+"/api/v1/actions/backup",
		"application/json",
		strings.NewReader(`{"job":"incremental"}`),
	)
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
