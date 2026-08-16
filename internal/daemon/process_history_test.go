package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func sample(name string, ts time.Time, cpu float64, rss int64, count int) processHistorySample {
	return processHistorySample{Name: name, TS: ts, CPUPercent: cpu, RSSKb: rss, ProcessCount: count}
}

func TestProcessHistoryStoreAppendAndQuery(t *testing.T) {
	store := newProcessHistoryStore(t.TempDir(), 7)
	now := time.Now().UTC().Truncate(time.Second)
	store.Append(sample("nginx", now.Add(-2*time.Minute), 1.0, 1024, 2))
	store.Append(sample("nginx", now.Add(-time.Minute), 2.5, 2048, 3))
	store.Append(sample("postgres", now, 4.0, 4096, 1))

	// Name filter.
	got := store.Query("nginx", nil, nil, 0)
	if len(got) != 2 || got[0].CPUPercent != 1.0 || got[1].CPUPercent != 2.5 {
		t.Fatalf("nginx query = %#v", got)
	}
	// Time range.
	from := now.Add(-90 * time.Second)
	to := now.Add(-30 * time.Second)
	got = store.Query("nginx", &from, &to, 0)
	if len(got) != 1 || got[0].CPUPercent != 2.5 {
		t.Fatalf("range query = %#v", got)
	}
	// Limit keeps the newest.
	got = store.Query("nginx", nil, nil, 1)
	if len(got) != 1 || got[0].CPUPercent != 2.5 {
		t.Fatalf("limited query = %#v", got)
	}
	// Unknown name.
	if got := store.Query("redis", nil, nil, 0); len(got) != 0 {
		t.Fatalf("unknown name query = %#v", got)
	}
}

func TestProcessHistoryStoreMemoryRing(t *testing.T) {
	store := newProcessHistoryStore("", 7)
	now := time.Now()
	for range processHistoryMaxSamples + 50 {
		store.Append(sample("nginx", now, 1, 1, 1))
	}
	got := store.Query("nginx", nil, nil, 5000)
	if len(got) != processHistoryMaxSamples {
		t.Fatalf("ring kept %d samples, want %d", len(got), processHistoryMaxSamples)
	}
}

func TestProcessHistoryStorePrunesExpiredDays(t *testing.T) {
	dir := t.TempDir()
	store := newProcessHistoryStore(dir, 1)
	old := time.Now().UTC().Add(-48 * time.Hour)
	store.Append(sample("nginx", old, 1, 1, 1))
	store.Append(sample("nginx", time.Now(), 2, 2, 1))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 day file after prune, got %d: %v", len(entries), entries)
	}
	// The surviving file holds only the fresh sample.
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 1 {
		t.Fatalf("expected 1 sample line, got %d", lines)
	}
}

func TestProcessHistorySampleWireShape(t *testing.T) {
	// The sample JSON field names must match the client contract.
	data, err := json.Marshal(sample("nginx", time.Now(), 1.5, 2048, 2))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "ts", "cpu_percent", "rss_kb", "process_count", "threads"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("sample missing %q: %s", key, data)
		}
	}
}

func TestProcessHistoryAPI(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                 "history-host",
		Version:            "v9.9.9",
		Transport:          "http",
		Listen:             "127.0.0.1:0",
		MetricsSecret:      "metrics-secret",
		MetricsHistoryPath: t.TempDir(),
		MetricsInterval:    time.Hour,
		StreamInterval:     time.Second,
		Runtimes:           []string{"java", "dotnet", "python"},
		WatchedProcesses:   []string{"nginx"},
		ProcessesLimit:     50,
		RequestTimeout:     5 * time.Second,
		ScriptTimeout:      time.Second,
		MaxBodyBytes:       1024,
		MaxConcurrentRuns:  1,
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

	// Seed samples directly so the test does not depend on ticker timing.
	now := time.Now()
	app.runtimes.history.Append(sample("nginx", now.Add(-time.Minute), 1.5, 1024, 2))
	app.runtimes.history.Append(sample("nginx", now, 3.0, 2048, 3))

	do := func(path string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer metrics-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, payload
	}

	status, payload := do("/api/v1/process-history?name=nginx")
	if status != http.StatusOK {
		t.Fatalf("query status = %d", status)
	}
	samples, _ := payload["samples"].([]any)
	if payload["name"] != "nginx" || len(samples) != 2 {
		t.Fatalf("payload = %#v", payload)
	}

	status, _ = do("/api/v1/process-history?name=bad%20name")
	if status != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d", status)
	}
	status, _ = do("/api/v1/process-history?name=nginx&from=yesterday")
	if status != http.StatusBadRequest {
		t.Fatalf("bad from status = %d", status)
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("app.Run returned: %v", err)
	}
}
