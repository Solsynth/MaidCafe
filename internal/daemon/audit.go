package daemon

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// auditMaxBytes caps the active audit file before it rotates to <path>.1,
// which keeps one generation of history across the boundary.
const auditMaxBytes = 1 << 20 // 1 MiB

// auditEntry is one hook execution record, appended to the audit file as a
// single JSON line per run. Name is the API slug; DisplayName is the optional
// human-readable label configured for the hook.
type auditEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name,omitempty"`
	Source      string    `json:"source"`
	// InvokedBy names the caller: a Solarpass handle, a labeled cloud
	// credential, or the transport ("stdio") when no identity is attached.
	InvokedBy  string `json:"invoked_by,omitempty"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	// Stdout and Stderr carry the captured run output (bounded by the
	// executor's per-run buffer), so a run's full log is inspectable later.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AuditLogger durably records every hook execution as JSONL. Logging is
// best-effort: a logger that cannot open its file is disabled with a warning
// and never affects execution. One rotation generation is kept (path.1).
type AuditLogger struct {
	path   string
	logger *slog.Logger
	mu     sync.Mutex
}

// NewAuditLogger returns nil (disabled) when [path] is empty or the state
// directory cannot be created; a disabled logger is a no-op everywhere.
func NewAuditLogger(path string, logger *slog.Logger) *AuditLogger {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(trimmed), 0o750); err != nil {
		logger.Warn("audit logging disabled: cannot create directory",
			"path", trimmed, "error", err)
		return nil
	}
	return &AuditLogger{path: trimmed, logger: logger}
}

// Record appends one entry, rotating the active file first when it reached
// auditMaxBytes.
func (a *AuditLogger) Record(entry auditEntry) {
	if a == nil {
		return
	}
	line, err := json.Marshal(entry)
	if err != nil {
		a.logger.Warn("audit record failed", "error", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		a.logger.Warn("audit record failed", "path", a.path, "error", err)
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() >= auditMaxBytes {
		_ = file.Close()
		_ = os.Rename(a.path, a.path+".1")
		file, err = os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			a.logger.Warn("audit record failed", "path", a.path, "error", err)
			return
		}
		defer file.Close()
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		a.logger.Warn("audit record failed", "path", a.path, "error", err)
	}
}

// Clear wipes every audit record, active and rotated file alike. A disabled
// logger is a no-op.
func (a *AuditLogger) Clear() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, path := range []string{a.path, a.path + ".1"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Recent returns up to [limit] (bounded to 500) of the newest entries,
// newest first. The rotated file is read before the active one so entries
// stay in order across the rotation boundary; unreadable or malformed lines
// are skipped. A disabled logger yields an empty (non-nil) slice so the API
// always responds with `entries: []`.
func (a *AuditLogger) Recent(limit int) []auditEntry {
	entries := []auditEntry{}
	if a == nil {
		return entries
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	appendFile := func(path string) {
		file, err := os.Open(path)
		if err != nil {
			return
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var entry auditEntry
			if json.Unmarshal(scanner.Bytes(), &entry) == nil {
				entries = append(entries, entry)
			}
		}
	}
	appendFile(a.path + ".1")
	appendFile(a.path)
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	// Newest first, for "recent runs" style consumers.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}
