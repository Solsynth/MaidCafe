package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestAuditLoggerRecordsAndReadsNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := NewAuditLogger(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	for i := 1; i <= 3; i++ {
		logger.Record(auditEntry{
			Name:       "deploy",
			Source:     "http",
			OK:         i%2 == 0,
			ExitCode:   i % 2,
			DurationMS: int64(i),
		})
	}
	entries := logger.Recent(10)
	if len(entries) != 3 {
		t.Fatalf("recent entries = %d, want 3", len(entries))
	}
	if entries[0].DurationMS != 3 || entries[2].DurationMS != 1 {
		t.Fatalf("entries not newest first: %+v", entries)
	}
	if entries[0].OK || !entries[1].OK {
		t.Fatalf("ok flags wrong: %+v", entries)
	}

	limited := logger.Recent(2)
	if len(limited) != 2 || limited[0].DurationMS != 3 || limited[1].DurationMS != 2 {
		t.Fatalf("limit not honored: %+v", limited)
	}
	if logger.Recent(0) == nil || len(logger.Recent(0)) != 3 {
		t.Fatal("default limit should return all entries")
	}
}

func TestAuditLoggerRotatesAtMaxSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := NewAuditLogger(path, slog.Default())
	// Each entry is a few hundred bytes; 6000 entries far exceed 1 MiB.
	for i := 0; i < 6000; i++ {
		logger.Record(auditEntry{
			Name:        "deploy",
			DisplayName: "Deploy the app",
			Source:      "relay",
			OK:          true,
			ExitCode:    0,
			DurationMS:  12,
			Error:       "012345678901234567890123456789012345678901234567890123456789",
		})
	}
	entries := logger.Recent(500)
	if len(entries) != 500 {
		t.Fatalf("recent entries = %d, want 500", len(entries))
	}
	// The rotated file must exist and the active file must have been capped.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= auditMaxBytes {
		t.Fatalf("active audit file not capped: %d bytes", info.Size())
	}
}

func TestAuditLoggerDisabledWhenUnwritable(t *testing.T) {
	// A path under a regular file can never be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	logger := NewAuditLogger(filepath.Join(blocker, "audit.jsonl"), slog.Default())
	if logger != nil {
		t.Fatal("expected disabled logger for unwritable path")
	}
	// Empty path disables too, and a nil logger is a no-op.
	if NewAuditLogger("", slog.Default()) != nil {
		t.Fatal("expected nil logger for empty path")
	}
	var nilLogger *AuditLogger
	nilLogger.Record(auditEntry{Name: "x"})
	if got := nilLogger.Recent(5); len(got) != 0 {
		t.Fatalf("nil logger Recent = %v", got)
	}
}

func TestAuditLoggerClearWipesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := NewAuditLogger(path, slog.Default())
	logger.Record(auditEntry{Name: "a", Source: "http", OK: true})
	logger.Record(auditEntry{Name: "b", Source: "relay", OK: false, ExitCode: 1})
	if len(logger.Recent(10)) != 2 {
		t.Fatalf("expected 2 entries before clear")
	}
	if err := logger.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := logger.Recent(10); len(got) != 0 {
		t.Fatalf("entries after clear = %v", got)
	}
	// Clearing an already-empty logger is fine, and the log keeps working.
	if err := logger.Clear(); err != nil {
		t.Fatal(err)
	}
	logger.Record(auditEntry{Name: "c", Source: "stdio", OK: true})
	if got := logger.Recent(10); len(got) != 1 || got[0].Name != "c" {
		t.Fatalf("after clear+record = %v", got)
	}
	// A disabled logger is a no-op.
	var nilLogger *AuditLogger
	if err := nilLogger.Clear(); err != nil {
		t.Fatalf("nil clear: %v", err)
	}
}

func TestExecuteRecordsAuditEntries(t *testing.T) {
	script := executable(t, "#!/bin/sh\nprintf 'ok'\n")
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	cfg := config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:        "deploy",
			DisplayName: "Deploy the app",
			Command:     script,
			Enabled:     true,
		}},
	}
	executor := NewWebhookExecutor(cfg)
	executor.SetAuditLogger(NewAuditLogger(path, slog.Default()))
	result, requestErr := executor.RunAction(t.Context(), "deploy", []byte(`{}`), "stdio", "stdio")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	entries := executor.audit.Recent(10)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Name != "deploy" || entry.DisplayName != "Deploy the app" ||
		entry.Source != "stdio" || !entry.OK || entry.ExitCode != 0 {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
	if entry.Timestamp.IsZero() {
		t.Fatal("audit entry missing timestamp")
	}
}
