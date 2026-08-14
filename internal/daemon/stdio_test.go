package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestStdioActionProtocol(t *testing.T) {
	script := executable(t, "#!/bin/sh\ncat\n")
	cfg := config.DaemonConfig{
		ID:                "host-stdio",
		Transport:         "stdio",
		MetricsInterval:   time.Hour,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:    "backup",
			Command: script,
			Enabled: true,
		}},
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
	var ready map[string]any
	if !responses.Scan() {
		t.Fatalf("missing ready event: %v", responses.Err())
	}
	if err := json.Unmarshal(responses.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if ready["event"] != "ready" {
		t.Fatalf("unexpected ready event: %#v", ready)
	}

	if _, err := fmt.Fprintln(stdinWriter, `{"type":"request","id":"1","action":"action","name":"backup","body":"hello"}`); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if !responses.Scan() {
		t.Fatalf("missing action response: %v", responses.Err())
	}
	if err := json.Unmarshal(responses.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != true {
		t.Fatalf("action failed: %#v", response)
	}
	result, ok := response["result"].(map[string]any)
	if !ok || result["stdout"] != "hello" {
		t.Fatalf("unexpected action result: %#v", response)
	}

	if _, err := fmt.Fprintln(stdinWriter, `{"type":"request","id":"2","action":"shutdown"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErr:
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdio daemon did not stop")
	}

}
