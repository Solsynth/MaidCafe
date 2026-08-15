package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"testing"
	"time"
)

func TestMetricsHistoryPersistsAndFiltersRange(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                   "history-test",
		MetricsHistoryPath:   filepath.Join(t.TempDir(), "metrics"),
		MetricsRetentionDays: 30,
		MetricsInterval:      time.Minute,
		RequestTimeout:       time.Second,
		ScriptTimeout:        time.Second,
		MaxBodyBytes:         1,
		MaxConcurrentRuns:    1,
	}
	executor := NewWebhookExecutor(cfg)
	collector, err := NewMetricsCollector(cfg, executor)
	if err != nil {
		t.Fatal(err)
	}
	first := collector.Record()
	stale := first
	stale.SentAt = first.SentAt.Add(-31 * 24 * time.Hour)
	staleData, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(cfg.MetricsHistoryPath, stale.SentAt.Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(stalePath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewMetricsCollector(cfg, executor)
	if err != nil {
		t.Fatal(err)
	}
	from := first.SentAt.Add(-time.Second)
	to := first.SentAt.Add(time.Second)
	entries, err := os.ReadDir(cfg.MetricsHistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != first.SentAt.Format("2006-01-02")+".jsonl" {
		t.Fatalf("daily history files = %#v", entries)
	}
	history := reloaded.History(MetricsHistoryQuery{From: &from, To: &to, Limit: 10})
	if len(history) != 1 ||
		!history[0].SentAt.Equal(first.SentAt) ||
		history[0].NetRxBytes != first.NetRxBytes ||
		history[0].DiskTotalKb != first.DiskTotalKb ||
		history[0].Load1 != first.Load1 {
		t.Fatalf("persisted history = %#v", history)
	}
}
