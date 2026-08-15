package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func executable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestWebhookExecutesOpaqueBodyAndRejectsBadSecret(t *testing.T) {
	output := filepath.Join(t.TempDir(), "body")
	script := executable(t, "#!/bin/sh\ncat > "+output+"\nprintf '%s' safe\n")
	cfg := config.DaemonConfig{ScriptTimeout: time.Second, MaxBodyBytes: 1024, MaxConcurrentRuns: 1, Webhooks: []config.WebhookConfig{{Name: "hook", Secret: "secret", Command: script, Enabled: true}}}
	executor := NewWebhookExecutor(cfg)
	server := httptest.NewServer(executor)
	defer server.Close()
	body := `{"x":"$(touch ` + filepath.Join(t.TempDir(), "sentinel") + `)"}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/hook", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("stdin mismatch: %q", got)
	}
	bad, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/hook", strings.NewReader(body))
	bad.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad secret status %d", resp.StatusCode)
	}
}
func TestWebhookFailureTimeoutAndBodyLimit(t *testing.T) {
	sleep := executable(t, "#!/bin/sh\nsleep 1\n")
	cfg := config.DaemonConfig{ScriptTimeout: 30 * time.Millisecond, MaxBodyBytes: 4, MaxConcurrentRuns: 1, Webhooks: []config.WebhookConfig{{Name: "slow", Secret: "s", Command: sleep, Enabled: true}}}
	executor := NewWebhookExecutor(cfg)
	server := httptest.NewServer(executor)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/slow", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer s")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("timeout status %d", resp.StatusCode)
	}
	large, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/slow", strings.NewReader("large"))
	large.Header.Set("Authorization", "Bearer s")
	resp, err = http.DefaultClient.Do(large)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body status %d", resp.StatusCode)
	}
	var payload executionResponse
	_ = json.NewDecoder(resp.Body).Decode(&payload)
}
func TestHealthDoesNotRevealConfiguration(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                "host-1",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		ScriptTimeout:     time.Second,
		RequestTimeout:    time.Second,
		MetricsInterval:   time.Hour,
		MaxBodyBytes:      100,
		MaxConcurrentRuns: 1,
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
	unauthorized, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized health status = %d", unauthorized.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "command") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("health leaked config: %s", encoded)
	}
}
