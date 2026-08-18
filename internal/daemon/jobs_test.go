package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func enabledJob(name, schedule, action string) config.JobConfig {
	enabled := true
	return config.JobConfig{Name: name, Schedule: schedule, Action: action, Enabled: &enabled}
}

func TestJobRunnerRunsConfiguredAction(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	script := executable(t, "#!/bin/sh\nprintf 'job-ran' > "+out+"\n")
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      4096,
		MaxConcurrentRuns: 2,
		Actions: []config.WebhookConfig{{
			Name: "backup", Command: script, Enabled: true,
		}},
	})
	runner := newJobRunner(executor, &nativeOpRunner{executor: executor}, &atomic.Pointer[CloudPublisher]{}, nil)
	runner.SetJobs([]config.JobConfig{enabledJob("j1", "@every 1s", "backup")})
	runner.tick(context.Background(), time.Now().Add(2*time.Second))
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "job-ran" {
		t.Fatalf("job action did not run: %q err=%v", data, err)
	}
}

func TestJobRunnerDispatchesNativeOp(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	podman := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	fakeCommand(t, "kill", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	executor := NewWebhookExecutor(config.DaemonConfig{ScriptTimeout: time.Second, MaxBodyBytes: 4096, MaxConcurrentRuns: 2})
	ops := &nativeOpRunner{executor: executor, runtimes: stubRuntimes(map[string]string{"podman": podman})}
	ops.SetScriptTimeout(time.Second)
	runner := newJobRunner(executor, ops, &atomic.Pointer[CloudPublisher]{}, nil)
	job := enabledJob("kill", "@every 1s", "process.kill")
	job.Body = map[string]any{"pid": 4242}
	runner.SetJobs([]config.JobConfig{job})
	runner.tick(context.Background(), time.Now().Add(2*time.Second))
	got, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(got)) != "-s\nKILL\n--\n4242" {
		t.Fatalf("native op job did not run: %q err=%v", got, err)
	}
}

func TestJobRunnerSkipsOverlappingRuns(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	script := executable(t, "#!/bin/sh\nprintf x >> "+out+"\n")
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      4096,
		MaxConcurrentRuns: 2,
		Actions: []config.WebhookConfig{{
			Name: "slow", Command: script, Enabled: true,
		}},
	})
	runner := newJobRunner(executor, &nativeOpRunner{executor: executor}, &atomic.Pointer[CloudPublisher]{}, nil)
	runner.SetJobs([]config.JobConfig{enabledJob("j1", "@every 1s", "slow")})
	// Simulate two ticks while a run is in flight: only the first executes.
	ctx := context.Background()
	runner.tick(ctx, time.Now().Add(2*time.Second))
	// Force the second tick to overlap by stalling the state manually.
	runner.mu.Lock()
	for _, state := range runner.jobs {
		state.running = true
	}
	runner.mu.Unlock()
	before, _ := os.ReadFile(out)
	runner.tick(ctx, time.Now().Add(3*time.Second))
	after, _ := os.ReadFile(out)
	if string(after) != string(before) {
		t.Fatalf("overlapping run executed: %q -> %q", before, after)
	}
}

func TestJobRunnerPublishesFailureNotification(t *testing.T) {
	var got notificationPayload
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/notifications") {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected cloud request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cloud.Close()
	failing := executable(t, "#!/bin/sh\necho 'boom' >&2\nexit 1\n")
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      4096,
		MaxConcurrentRuns: 2,
		Actions: []config.WebhookConfig{{
			Name: "fail", Command: failing, Enabled: true,
		}},
	})
	publisher, err := NewCloudPublisher(config.DaemonConfig{
		ID: "host-1", CloudURL: cloud.URL, CloudSecret: "s", RequestTimeout: time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	box := &atomic.Pointer[CloudPublisher]{}
	box.Store(publisher)
	runner := newJobRunner(executor, &nativeOpRunner{executor: executor}, box, nil)
	job := enabledJob("failjob", "@every 1s", "fail")
	job.NotifyOnFailure = true
	runner.SetJobs([]config.JobConfig{job})
	runner.tick(context.Background(), time.Now().Add(2*time.Second))
	if got.Kind != "job.failure" {
		t.Fatalf("expected job.failure notification, got %+v", got)
	}
	if got.Metadata["job"] != "failjob" {
		t.Fatalf("notification metadata missing job name: %+v", got)
	}
}
