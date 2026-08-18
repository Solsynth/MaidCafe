package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestPatchDaemonSection(t *testing.T) {
	input := `# top comment
[daemon]
id = "host-1"
metricsInterval = "1m"
# an inline comment
processesLimit = 50

[[daemon.actions]]
name = "backup"
command = "/usr/local/bin/backup"

[cloud]
target = "https://example.com"
`
	patched := patchDaemonSection(input, map[string]string{
		"metricsInterval": `"30s"`,
		"processesLimit":  "100",
		"newKey":          `"hello"`,
		"runtimes":        `["java", "dotnet"]`,
	})
	if strings.Contains(patched, `metricsInterval = "1m"`) {
		t.Fatalf("existing key not replaced:\n%s", patched)
	}
	if !strings.Contains(patched, `metricsInterval = "30s"`) {
		t.Fatalf("replacement missing:\n%s", patched)
	}
	if !strings.Contains(patched, `processesLimit = 100`) {
		t.Fatalf("int replacement missing:\n%s", patched)
	}
	if !strings.Contains(patched, `newKey = "hello"`) {
		t.Fatalf("new key not appended:\n%s", patched)
	}
	// The [[daemon.actions]] sub-table and other sections stay untouched.
	if !strings.Contains(patched, `command = "/usr/local/bin/backup"`) {
		t.Fatalf("sub-table mangled:\n%s", patched)
	}
	if !strings.Contains(patched, `target = "https://example.com"`) {
		t.Fatalf("other section mangled:\n%s", patched)
	}
	// Missing [daemon] section is created.
	created := patchDaemonSection("existing = 1\n", map[string]string{"processesLimit": "5"})
	if !strings.Contains(created, "[daemon]") || !strings.Contains(created, "processesLimit = 5") {
		t.Fatalf("missing section not created:\n%s", created)
	}
}

func TestHTTPConfigGetAndPatch(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfig := func(text string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(`[daemon]
id = "host-1"
transport = "http"
listen = "127.0.0.1:0"
metricsSecret = "metrics-secret"
metricsHistoryPath = ""
metricsInterval = "1m"
streamInterval = "1s"
containersInterval = "5s"
processesLimit = 50
requestTimeout = "5s"
scriptTimeout = "1s"
maxBodyBytes = 1024
maxConcurrentRuns = 2
runtimes = ["java", "dotnet", "python"]
`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(cfg.Daemon, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.SetConfigPath(configPath)
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	baseURL := "http://" + app.ListenAddr()

	get := func(path string, authorized bool) (*http.Response, map[string]any) {
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer metrics-secret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&body)
		}
		resp.Body.Close()
		return resp, body
	}

	// Unauthenticated GET is rejected.
	if resp, _ := get("/api/v1/config", false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized config GET status = %d", resp.StatusCode)
	}
	resp, body := get("/api/v1/config", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config GET status = %d", resp.StatusCode)
	}
	view, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config view missing: %+v", body)
	}
	if view["id"] != "host-1" || view["processes_limit"] != float64(50) {
		t.Fatalf("config view wrong: %+v", view)
	}
	if _, leaked := view["metrics_secret"]; leaked {
		t.Fatal("metrics secret leaked into config view")
	}

	// PATCH a reloadable key: write + hot reload.
	patch := `{"processesLimit": 77, "metricsInterval": "30s"}`
	req, err := http.NewRequest(http.MethodPatch, baseURL+"/api/v1/config", strings.NewReader(patch))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer metrics-secret")
	req.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", []byte(patch)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config PATCH status = %d", resp.StatusCode)
	}
	// The on-disk file and the reloaded state both reflect the patch.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "processesLimit = 77") {
		t.Fatalf("config file not patched:\n%s", raw)
	}
	if got := app.rt.Load().processesLimit; got != 77 {
		t.Fatalf("reloaded processesLimit = %d", got)
	}
	if got := app.rt.Load().intervals.metrics; got != 30*time.Second {
		t.Fatalf("reloaded metricsInterval = %v", got)
	}

	// Unsupported keys and bad values are rejected without touching the file.
	bad := []string{
		`{"metricsSecret": "x"}`,
		`{"transport": "stdio"}`,
		`{"processesLimit": 0}`,
		`{"processesLimit": "fifty"}`,
		`{"metricsInterval": "not-a-duration"}`,
	}
	for _, payload := range bad {
		req, err := http.NewRequest(http.MethodPatch, baseURL+"/api/v1/config", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer metrics-secret")
		req.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", []byte(payload)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad patch %q status = %d, want 400", payload, resp.StatusCode)
		}
	}
	rawAfter, _ := os.ReadFile(configPath)
	if !strings.Contains(string(rawAfter), "processesLimit = 77") {
		t.Fatalf("rejected patch modified the file:\n%s", rawAfter)
	}

	// A bad signature is rejected before any write.
	badSig := `{"processesLimit": 99}`
	req, err = http.NewRequest(http.MethodPatch, baseURL+"/api/v1/config", strings.NewReader(badSig))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer metrics-secret")
	req.Header.Set("X-MaidCafe-Signature", signedHeader("wrong", []byte(badSig)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature PATCH status = %d", resp.StatusCode)
	}
}

func TestApplyReloadSwapsExecutorHooksAndTimeout(t *testing.T) {
	oldAction := executable(t, "#!/bin/sh\nexit 0\n")
	newAction := executable(t, "#!/bin/sh\nexit 0\n")
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name: "old", Command: oldAction, Enabled: true,
		}},
	})
	ops := &nativeOpRunner{executor: executor}
	box := &atomic.Pointer[CloudPublisher]{}
	app := &App{
		cfg:       config.DaemonConfig{},
		executor:  executor,
		ops:       ops,
		watched:   newWatchedProcessStore("", nil),
		runtimes:  &RuntimesCollector{},
		jobs:      newJobRunner(executor, ops, box, nil),
		publisher: box,
		logger:    newTestLogger(),
	}
	app.rt.Store(newReloadableConfig(config.DaemonConfig{}))

	if _, requestErr := executor.RunAction(context.Background(), "old", nil, "test", "t"); requestErr != nil {
		t.Fatalf("old action missing: %+v", requestErr)
	}
	app.applyReload(config.DaemonConfig{
		ScriptTimeout:     2 * time.Second,
		MaxBodyBytes:      4096,
		MaxConcurrentRuns: 2,
		Runtimes:          []string{"java", "dotnet"},
		Actions: []config.WebhookConfig{{
			Name: "new", Command: newAction, Enabled: true,
		}},
	})
	if _, requestErr := executor.RunAction(context.Background(), "new", nil, "test", "t"); requestErr != nil {
		t.Fatalf("new action missing after reload: %+v", requestErr)
	}
	if _, requestErr := executor.RunAction(context.Background(), "old", nil, "test", "t"); requestErr == nil {
		t.Fatal("old action still present after reload")
	}
	if got := app.rt.Load().scriptTimeout; got != 2*time.Second {
		t.Fatalf("reloaded scriptTimeout = %v", got)
	}
	if got := time.Duration(executor.scriptTimeout.Load()); got != 2*time.Second {
		t.Fatalf("executor scriptTimeout = %v", got)
	}
	runtimes, _ := app.runtimes.settings()
	if len(runtimes) != 2 || runtimes[0] != "java" {
		t.Fatalf("runtimes not reloaded: %v", runtimes)
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
