package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLogLines(t *testing.T) {
	out := []byte(
		"2026-08-18T10:00:00.123456789Z hello world\n" +
			"2026-08-18T10:00:01.000000000Z second line\n" +
			"line without a timestamp\n",
	)
	lines := parseLogLines(out)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0].Line != "hello world" {
		t.Fatalf("line[0] = %q", lines[0].Line)
	}
	if lines[0].TS.IsZero() || lines[0].TS.Format(time.RFC3339Nano) != "2026-08-18T10:00:00.123456789Z" {
		t.Fatalf("line[0] ts = %v", lines[0].TS)
	}
	if lines[2].TS.IsZero() {
		t.Fatal("timestamp-less line should fall back to now()")
	}
}

func TestContainerLogStoreRingAndCursor(t *testing.T) {
	store := newContainerLogStore("", 7)
	for i := range containerLogRingLines + 50 {
		store.Append("web", []containerLogLine{{TS: time.Unix(int64(i), 0), Line: "line"}})
	}
	snapshot := store.Snapshot("web", containerLogRingLines)
	if len(snapshot) != containerLogRingLines {
		t.Fatalf("ring capped at %d, got %d", containerLogRingLines, len(snapshot))
	}
	if got := store.Cursor("web"); got.Unix() != int64(containerLogRingLines+49) {
		t.Fatalf("cursor = %v", got)
	}
	if store.Snapshot("missing", 10) != nil {
		t.Fatal("unknown container should return nil")
	}
}

func TestContainerLogStoreDiskAndPrune(t *testing.T) {
	dir := t.TempDir()
	store := newContainerLogStore(dir, 7)
	now := time.Now().UTC()
	store.Append("web", []containerLogLine{{TS: now, Line: "first"}})
	store.Append("web", []containerLogLine{{TS: now.Add(time.Second), Line: "second"}})
	path := filepath.Join(dir, "web.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []containerLogLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry containerLogLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, entry)
	}
	if len(decoded) != 2 || decoded[1].Line != "second" {
		t.Fatalf("disk log = %+v", decoded)
	}
	// An expired file is pruned on the next append.
	oldPath := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(oldPath, []byte("{\"ts\":\"2020-01-01T00:00:00Z\",\"line\":\"x\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	store.Append("web", []containerLogLine{{TS: now.Add(2 * time.Second), Line: "third"}})
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("expired log file was not pruned")
	}
}

func TestLogsCollectorTailsIncrementally(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	fake := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\nprintf '2026-08-18T10:00:00.000000000Z first\\n2026-08-18T10:00:01.000000000Z second\\n'\n")
	store := newContainerLogStore("", 7)
	collector := &LogsCollector{probe: &runtimeProbeState{}, store: store}
	// Prime the probe so it resolves the fake binary directly.
	collector.probe.mu.Lock()
	collector.probe.runtimePaths = map[string]string{"podman": fake}
	collector.probe.probed = true
	collector.probe.mu.Unlock()

	collected, err := collector.collect(context.Background(), map[string][]containerEntry{
		"podman": {{ID: "web", State: "running"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collected["web"]) != 2 {
		t.Fatalf("expected 2 lines, got %+v", collected["web"])
	}
	// The cursor advanced, so the next tail uses --since.
	collected, err = collector.collect(context.Background(), map[string][]containerEntry{
		"podman": {{ID: "web", State: "running"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(out)
	joined := string(args)
	tailIdx := strings.Index(joined, "--tail")
	sinceIdx := strings.Index(joined, "--since")
	if tailIdx == -1 || sinceIdx == -1 || tailIdx > sinceIdx {
		t.Fatalf("expected backfill then --since tail, got %q", joined)
	}
	// The fake repeats the same timestamps, so the second run captures
	// nothing (cursor did not regress).
	if len(collected) != 0 {
		t.Fatalf("duplicate lines should be dropped, got %+v", collected)
	}
}

func TestLogsCollectorSkipsStoppedContainers(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	fake := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	collector := &LogsCollector{probe: &runtimeProbeState{}, store: newContainerLogStore("", 7)}
	collector.probe.mu.Lock()
	collector.probe.runtimePaths = map[string]string{"podman": fake}
	collector.probe.probed = true
	collector.probe.mu.Unlock()
	collected, err := collector.collect(context.Background(), map[string][]containerEntry{
		"podman": {{ID: "web", State: "exited"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collected) != 0 {
		t.Fatalf("stopped container should not be tailed: %+v", collected)
	}
	if data, _ := os.ReadFile(out); len(data) != 0 {
		t.Fatalf("logs command should not run for stopped containers: %q", data)
	}
}
