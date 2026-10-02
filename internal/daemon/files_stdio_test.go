package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// TestStdioFileActions runs the file API over the SSH stdio transport, which
// is how a desktop MaidKit build reaches a daemon without its own listener.
func TestStdioFileActions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DaemonConfig{
		ID:                "host-stdio-files",
		Transport:         "stdio",
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java"},
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      65536,
		MaxConcurrentRuns: 1,
		AuditPath:         filepath.Join(t.TempDir(), "audit.jsonl"),
		Files: config.FilesConfig{
			Enabled:    true,
			Roots:      []config.FilesRootConfig{{Path: root}},
			AllowWrite: true,
		},
	}

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinReader, stdoutWriter
	defer func() {
		os.Stdin, os.Stdout = originalStdin, originalStdout
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	responses := bufio.NewScanner(stdoutReader)
	if !responses.Scan() {
		t.Fatalf("missing ready event: %v", responses.Err())
	}

	request := func(id, action string, body any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(stdinWriter, `{"type":"request","id":%q,"action":%q,"body":%s}`+"\n", id, action, encoded); err != nil {
			t.Fatal(err)
		}
		for responses.Scan() {
			var response map[string]any
			if err := json.Unmarshal(responses.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["type"] != "response" || response["id"] != id {
				continue
			}
			return response
		}
		t.Fatalf("no response for %s: %v", id, responses.Err())
		return nil
	}

	listed := request("1", "files.list", map[string]any{"path": root})
	if listed["ok"] != true {
		t.Fatalf("list response = %v", listed)
	}
	result := listed["result"].(map[string]any)
	if entries := result["entries"].([]any); len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}

	written := request("2", "files.write", map[string]any{
		"path":    filepath.Join(root, "existing.txt"),
		"content": base64.StdEncoding.EncodeToString([]byte("new")),
	})
	if written["ok"] != true {
		t.Fatalf("write response = %v", written)
	}
	content, err := os.ReadFile(filepath.Join(root, "existing.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Fatalf("written content = %q", content)
	}

	// A path outside the roots is refused over the pipe exactly as it is over
	// HTTP: the transport does not widen the policy.
	refused := request("3", "files.list", map[string]any{"path": "/etc"})
	if refused["ok"] != false {
		t.Fatalf("outside-root response = %v", refused)
	}
	// An unknown file action is not dispatched to the hook table or ops.
	unknown := request("4", "files.explode", map[string]any{"path": root})
	if unknown["ok"] != false {
		t.Fatalf("unknown action response = %v", unknown)
	}

	if _, err := fmt.Fprintln(stdinWriter, `{"type":"request","id":"5","action":"shutdown"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio run did not stop")
	}
}
