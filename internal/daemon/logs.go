package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Container log tracking: every running container is tailed incrementally
// with `<runtime> logs --since <cursor> --timestamps <id>` on the logs
// cadence. New lines are appended to a per-container JSONL file (rotated at
// 512 KiB with one generation, pruned by metricsRetentionDays) and fanned
// out to SSE `logs` subscribers as the delta since the last tick. The
// in-memory ring is the snapshot window and the cursor source; the disk file
// is the durable record.

const (
	// containerLogRingLines caps the in-memory tail per container (and the
	// snapshot endpoint's answer).
	containerLogRingLines = 1000
	// containerLogFileBytes rotates a container's log file at this size,
	// keeping one generation like the audit log.
	containerLogFileBytes = 1 << 19 // 512 KiB
	// containerLogTailBytes caps one tail command's captured output; a 200
	// line backfill can exceed the executor's 8 KiB buffer.
	containerLogTailBytes = 256 * 1024
)

// containerLogLine is one captured log line with its (runtime-provided)
// timestamp.
type containerLogLine struct {
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

// logsFramePayload is one SSE `logs` frame: the new lines for one container
// captured since the last tick.
type logsFramePayload struct {
	Container string             `json:"container"`
	Lines     []containerLogLine `json:"lines"`
}

type logAlertRule struct {
	name      string
	pattern   *regexp.Regexp
	container string
	title     string
	cooldown  time.Duration
}

type logAlertEvaluator struct {
	mu    sync.Mutex
	rules []logAlertRule
	last  map[string]time.Time
}

func newLogAlertEvaluator() *logAlertEvaluator {
	return &logAlertEvaluator{last: map[string]time.Time{}}
}

func (e *logAlertEvaluator) SetAlerts(alerts []config.LogAlertConfig) {
	rules := make([]logAlertRule, 0, len(alerts))
	for _, alert := range alerts {
		if alert.Enabled != nil && !*alert.Enabled {
			continue
		}
		pattern, err := regexp.Compile(alert.Pattern)
		if err != nil {
			continue
		}
		cooldown := time.Duration(alert.CooldownSeconds) * time.Second
		if cooldown <= 0 {
			cooldown = 5 * time.Minute
		}
		rules = append(rules, logAlertRule{
			name: alert.Name, pattern: pattern, container: alert.Container,
			title: alert.Title, cooldown: cooldown,
		})
	}
	e.mu.Lock()
	e.rules = rules
	e.mu.Unlock()
}

type logAlertMatch struct {
	Rule      string
	Title     string
	Container string
	Line      containerLogLine
}

func (e *logAlertEvaluator) Match(container string, line containerLogLine) []logAlertMatch {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	matches := make([]logAlertMatch, 0)
	for _, rule := range e.rules {
		if rule.container != "" && rule.container != container {
			continue
		}
		if !rule.pattern.MatchString(line.Line) {
			continue
		}
		key := rule.name + "\x00" + container
		if previous, ok := e.last[key]; ok && now.Sub(previous) < rule.cooldown {
			continue
		}
		e.last[key] = now
		matches = append(matches, logAlertMatch{Rule: rule.name, Title: rule.title, Container: container, Line: line})
	}
	return matches
}

type logUploadBuffer struct {
	mu      sync.Mutex
	pending []LogUploadEntry
	lastTry time.Time
}

func newLogUploadBuffer() *logUploadBuffer {
	return &logUploadBuffer{}
}

func (b *logUploadBuffer) Add(container string, lines []containerLogLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range lines {
		b.pending = append(b.pending, LogUploadEntry{ContainerID: container, Timestamp: line.TS, Line: line.Line})
	}
	if len(b.pending) > 5000 {
		b.pending = b.pending[len(b.pending)-5000:]
	}
}

func (b *logUploadBuffer) Flush(ctx context.Context, publisher *CloudPublisher, enabled bool, interval time.Duration, maxLines int) {
	if !enabled || publisher == nil || maxLines <= 0 {
		return
	}
	b.mu.Lock()
	now := time.Now()
	if len(b.pending) == 0 || (!b.lastTry.IsZero() && now.Sub(b.lastTry) < interval) {
		b.mu.Unlock()
		return
	}
	b.lastTry = now
	count := maxLines
	if count > len(b.pending) {
		count = len(b.pending)
	}
	batch := append([]LogUploadEntry(nil), b.pending[:count]...)
	b.mu.Unlock()
	if err := publisher.PublishLogs(ctx, batch); err != nil {
		return
	}
	b.mu.Lock()
	if len(b.pending) >= count {
		b.pending = b.pending[count:]
	}
	b.mu.Unlock()
}

type containerLogTail struct {
	lines  []containerLogLine
	lastTS time.Time // cursor for the next --since tail
}

// containerLogStore persists per-container logs and keeps the bounded tail
// rings. A missing storage directory keeps the rings only (memory-only
// daemons, tests).
type containerLogStore struct {
	mu        sync.Mutex
	dir       string
	retention time.Duration
	tails     map[string]*containerLogTail
}

func newContainerLogStore(storageDir string, retentionDays int) *containerLogStore {
	retention := time.Duration(retentionDays) * 24 * time.Hour
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if retention > 30*24*time.Hour {
		retention = 30 * 24 * time.Hour
	}
	return &containerLogStore{
		dir:       storageDir,
		retention: retention,
		tails:     map[string]*containerLogTail{},
	}
}

// Append records new lines for one container, pruning expired log files
// when the store is disk-backed. Returns the lines kept (all of them, up to
// the ring cap).
func (s *containerLogStore) Append(id string, lines []containerLogLine) {
	if len(lines) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tail := s.tails[id]
	if tail == nil {
		tail = &containerLogTail{}
		s.tails[id] = tail
	}
	for _, line := range lines {
		tail.lines = append(tail.lines, line)
		if line.TS.After(tail.lastTS) {
			tail.lastTS = line.TS
		}
	}
	if len(tail.lines) > containerLogRingLines {
		tail.lines = tail.lines[len(tail.lines)-containerLogRingLines:]
	}
	if s.dir == "" {
		return
	}
	if err := s.appendDiskLocked(id, lines); err != nil {
		// Log capture is best-effort: a disk failure never drops the ring.
		_ = err
	}
	_ = s.pruneLocked(time.Now())
}

func (s *containerLogStore) appendDiskLocked(id string, lines []containerLogLine) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(s.dir, id+".jsonl")
	if info, err := os.Stat(path); err == nil && info.Size() >= containerLogFileBytes {
		_ = os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			return err
		}
	}
	return nil
}

// Cursor returns the last-seen timestamp for one container (the --since
// argument), or the zero time when nothing was captured yet.
func (s *containerLogStore) Cursor(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tail := s.tails[id]; tail != nil {
		return tail.lastTS
	}
	return time.Time{}
}

// Snapshot returns the last [limit] lines for one container (1..1000;
// default 200), oldest first.
func (s *containerLogStore) Snapshot(id string, limit int) []containerLogLine {
	if limit <= 0 || limit > containerLogRingLines {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tail := s.tails[id]
	if tail == nil {
		return nil
	}
	lines := tail.lines
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	out := make([]containerLogLine, len(lines))
	copy(out, lines)
	return out
}

// pruneLocked removes log files whose mtime is older than the retention
// window. Callers hold mu.
func (s *containerLogStore) pruneLocked(now time.Time) error {
	if s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(s.dir, 0o750)
		}
		return err
	}
	cutoff := now.Add(-s.retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
			_ = os.Remove(filepath.Join(s.dir, entry.Name()) + ".1")
		}
	}
	return nil
}

// LogsCollector tails running containers with the shared runtime probe and
// sudo -n retry, appending new lines to the store.
type LogsCollector struct {
	probe  *runtimeProbeState
	store  *containerLogStore
	logger *slog.Logger
}

// collect tails the given runtime -> containers mapping (probe order) and
// returns the new lines captured this tick, keyed by container id.
func (c *LogsCollector) collect(ctx context.Context, runtimes map[string][]containerEntry) (map[string][]containerLogLine, error) {
	paths := c.probe.probePathSnapshot(ctx)
	collected := map[string][]containerLogLine{}
	for _, runtime := range []string{"podman", "docker"} {
		containers := runtimes[runtime]
		if len(containers) == 0 {
			continue
		}
		path, ok := paths[runtime]
		if !ok {
			continue
		}
		for _, container := range containers {
			if container.State != "running" {
				continue
			}
			lines, err := c.tail(ctx, path, container.ID)
			if err != nil {
				c.logger.Debug("container log tail failed", "container", container.ID, "error", err)
				continue
			}
			if len(lines) > 0 {
				collected[container.ID] = lines
				c.store.Append(container.ID, lines)
			}
		}
	}
	return collected, nil
}

// tail runs one incremental `logs` for the container, retrying through
// `sudo -n` when the direct invocation fails (root-owned containers).
func (c *LogsCollector) tail(ctx context.Context, runtimePath, id string) ([]containerLogLine, error) {
	cursor := c.store.Cursor(id)
	args := []string{"logs"}
	if cursor.IsZero() {
		args = append(args, "--tail", "200")
	} else {
		args = append(args, "--since", cursor.UTC().Format(time.RFC3339Nano))
	}
	args = append(args, "--timestamps", id)
	out, err := runCommandBounded(ctx, runtimePath, args...)
	if err != nil {
		if os.Geteuid() != 0 {
			if sudoOut, sudoErr := runCommandBounded(ctx, "sudo", append([]string{"-n", runtimePath}, args...)...); sudoErr == nil {
				out = sudoOut
				err = nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	lines := parseLogLines(out)
	// Lines older than the cursor (container restarts, clock skew) are
	// dropped so the cursor never regresses.
	cursor = c.store.Cursor(id)
	kept := lines[:0]
	for _, line := range lines {
		if !line.TS.After(cursor) {
			continue
		}
		kept = append(kept, line)
	}
	return kept, nil
}

// parseLogLines splits `logs --timestamps` output into ts/line pairs. Lines
// without a parseable RFC3339Nano prefix fall back to now().
func parseLogLines(out []byte) []containerLogLine {
	lines := make([]containerLogLine, 0, 32)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	now := time.Now().UTC()
	for scanner.Scan() {
		raw := scanner.Text()
		if raw == "" {
			continue
		}
		line := containerLogLine{TS: now, Line: raw}
		if space := strings.IndexByte(raw, ' '); space > 0 {
			if parsed, err := time.Parse(time.RFC3339Nano, raw[:space]); err == nil {
				line.TS = parsed
				line.Line = raw[space+1:]
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// runCommandBounded runs one command with bounded output capture, mirroring
// runCommand but sized for log tails.
func runCommandBounded(ctx context.Context, name string, args ...string) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, collectorExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, name, args...)
	outBuf, errBuf := &limitedBuffer{limit: containerLogTailBytes}, &limitedBuffer{limit: containerLogTailBytes}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return []byte(outBuf.String()), nil
}
