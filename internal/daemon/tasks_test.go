package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// taskFrom parses the task a mutating operation answered with.
func taskFrom(t *testing.T, status int, body []byte) opTaskView {
	t.Helper()
	if status != http.StatusAccepted {
		t.Fatalf("start status = %d: %s", status, body)
	}
	var payload struct {
		OK   bool       `json:"ok"`
		Task opTaskView `json:"task"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("task body %s: %v", body, err)
	}
	if !payload.OK || payload.Task.ID == "" {
		t.Fatalf("task body = %s", body)
	}
	return payload.Task
}

// readTask reads one task, with the output written since [since] (-1 for the
// retained tail).
func readTask(t *testing.T, base, id string, since int64) opTaskView {
	t.Helper()
	path := "/api/v1/tasks/" + id
	if since >= 0 {
		path += fmt.Sprintf("?since=%d", since)
	}
	status, body := detailRequest(t, base, path)
	if status != http.StatusOK {
		t.Fatalf("task read = %d: %s", status, body)
	}
	var payload struct {
		Task opTaskView `json:"task"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("task read body %s: %v", body, err)
	}
	return payload.Task
}

// awaitTask polls a task until it leaves the running state.
func awaitTask(t *testing.T, base, id string) opTaskView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		task := readTask(t, base, id, -1)
		if task.Status != taskStatusRunning {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never finished: %+v", id, task)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// watchedComposeRuntime is a runtime whose compose steps take long enough to be
// watched: each one prints what it is doing, holds still, and exits cleanly —
// with a warning on stderr, the way podman and compose announce a host detail
// while they work. FAKE_COMPOSE_SLEEP sets how long each step holds still.
func watchedComposeRuntime() string {
	return `#!/bin/sh
case "$1" in
compose)
	echo "The cgroupv2 manager is set to systemd but there is no systemd user session available" >&2
	case "$*" in
	*pull*)
		echo "Pulling web"
		sleep "${FAKE_COMPOSE_SLEEP:-0}"
		;;
	*)
		echo "Recreating web"
		sleep "${FAKE_COMPOSE_SLEEP:-0}"
		;;
	esac
	;;
esac
`
}

// composeTaskFixture starts a daemon whose compose steps are watchable, and
// returns it with a project directory that has a compose file.
func composeTaskFixture(t *testing.T) (base, projectDir string) {
	t.Helper()
	fakeRuntimeBinary(t, watchedComposeRuntime())
	projectDir = filepath.Join(t.TempDir(), "myapp")
	writeComposeFile(t, filepath.Join(projectDir, "compose.yaml"),
		"services:\n  web:\n    image: nginx:1.25\n")
	app, err := NewApp(composeStacksTestConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = app.Shutdown(ctx) })
	return "http://" + app.ListenAddr(), projectDir
}

// TestComposeUpdateRunsAsATask pins the contract a client lives with: an
// operation that pulls images answers with a task before the work is done, the
// task carries the plan it is following, and its output arrives while it runs —
// which is what a progress view is built on, and what a request held open for
// minutes could never give (the client's own read timeout fires first).
func TestComposeUpdateRunsAsATask(t *testing.T) {
	t.Setenv("FAKE_COMPOSE_SLEEP", "1")
	base, projectDir := composeTaskFixture(t)

	status, body := signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	task := taskFrom(t, status, body)
	if task.Name != "compose.update" || task.Target != "myapp" || task.Source != "http" {
		t.Fatalf("task = %+v", task)
	}
	if len(task.Stages) != 2 || task.Stages[0].Label != "pull" || task.Stages[1].Label != "recreate" {
		t.Fatalf("plan = %+v", task.Stages)
	}

	// Watched mid-run, the plan says where the run is: the pull is under way
	// and the recreate has not been reached.
	running := readTask(t, base, task.ID, -1)
	if running.Status != taskStatusRunning {
		t.Fatalf("the update finished before it could be watched: %+v", running)
	}
	if running.Stages[1].Status != stageStatusPending {
		t.Fatalf("recreate ran before the pull finished: %+v", running.Stages)
	}
	if running.Stages[0].Status == stageStatusPending {
		t.Fatalf("pull never started: %+v", running.Stages)
	}

	finished := awaitTask(t, base, task.ID)
	if finished.Status != taskStatusSucceeded || !finished.OK || finished.ExitCode != 0 {
		t.Fatalf("finished = %+v", finished)
	}
	for _, stage := range finished.Stages {
		if stage.Status != taskStatusSucceeded {
			t.Fatalf("stage %s = %s", stage.Label, stage.Status)
		}
	}
	// Both stages' output is retained, and the byte count says how much.
	if finished.OutputBytes == 0 {
		t.Fatalf("no output was retained: %+v", finished)
	}
	full := readTask(t, base, task.ID, 0)
	if !strings.Contains(full.Stdout, "Pulling web") || !strings.Contains(full.Stdout, "Recreating web") {
		t.Fatalf("stdout = %q", full.Stdout)
	}
	// Both streams reach the task, and in the retained output they arrive
	// together: what a runtime warns about while it works — podman's cgroup
	// notes, compose's per-service remarks — is part of watching a pull.
	raw := readTaskBody(t, base, "/api/v1/tasks/"+task.ID+"?since=0")
	if !strings.Contains(string(raw), "no systemd user session") {
		t.Fatalf("the command's stderr did not reach the task output: %s", raw)
	}

	// The list is how a client that lost the id finds the run again, and an id
	// the daemon never issued is a clean 404 rather than an empty task.
	status, body = detailRequest(t, base, "/api/v1/tasks?limit=5")
	if status != http.StatusOK || !strings.Contains(string(body), task.ID) {
		t.Fatalf("task list = %d %s", status, body)
	}
	if status, _ := detailRequest(t, base, "/api/v1/tasks/deadbeef"); status != http.StatusNotFound {
		t.Fatalf("unknown task status = %d, want 404", status)
	}
}

// TestTaskOutputIsReadAsADelta pins the output contract: a client asks for the
// bytes after the last one it read, so watching a long pull does not mean
// re-reading everything it has already shown.
func TestTaskOutputIsReadAsADelta(t *testing.T) {
	fakeRuntimeBinary(t, watchedComposeRuntime())
	projectDir := filepath.Join(t.TempDir(), "myapp")
	writeComposeFile(t, filepath.Join(projectDir, "compose.yaml"),
		"services:\n  web:\n    image: nginx:1.25\n")
	app, err := NewApp(composeStacksTestConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	base := "http://" + app.ListenAddr()

	status, body := signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	task := taskFrom(t, status, body)
	awaitTask(t, base, task.ID)

	var first struct {
		Output          string `json:"output"`
		OutputFrom      int64  `json:"output_from"`
		OutputTruncated bool   `json:"output_truncated"`
	}
	raw := readTaskBody(t, base, "/api/v1/tasks/"+task.ID)
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("task body %s: %v", raw, err)
	}
	if first.OutputFrom != 0 || first.OutputTruncated || first.Output == "" {
		t.Fatalf("first read = %+v", first)
	}
	// Reading from the end yields nothing new, and says so.
	var second struct {
		Output     string `json:"output"`
		OutputFrom int64  `json:"output_from"`
	}
	raw = readTaskBody(t, base, fmt.Sprintf("/api/v1/tasks/%s?since=%d", task.ID, len(first.Output)))
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatalf("task body %s: %v", raw, err)
	}
	if second.Output != "" || second.OutputFrom != int64(len(first.Output)) {
		t.Fatalf("second read = %+v", second)
	}
}

// readTaskBody performs one authorized GET and returns the raw body.
func readTaskBody(t *testing.T, base, path string) []byte {
	t.Helper()
	status, body := detailRequest(t, base, path)
	if status != http.StatusOK {
		t.Fatalf("task read = %d: %s", status, body)
	}
	return body
}

// TestTaskOutputKeepsATail pins the bound on what a task retains: a run that
// writes more than the tail leaves behind cannot have all of it, and a client
// reads what is left as a replacement rather than an extension.
func TestTaskOutputKeepsATail(t *testing.T) {
	task := newOpTask("t1", "compose.update", "Update compose stack", "myapp", "http", "", nil, nil)
	line := strings.Repeat("x", 1024) + "\n"
	for range 40 {
		if _, err := task.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := task.snapshot().OutputBytes, int64(40*len(line)); got != want {
		t.Fatalf("output bytes = %d, want %d", got, want)
	}
	chunk, from, truncated := task.outputSince(0)
	if !truncated || len(chunk) != taskOutputLimit {
		t.Fatalf("reading from the start = %d bytes, truncated %v", len(chunk), truncated)
	}
	if from != int64(40*len(line))-int64(taskOutputLimit) {
		t.Fatalf("output starts at %d", from)
	}
	// A client at the end of what it read gets exactly the new bytes.
	if _, err := task.Write([]byte("tail\n")); err != nil {
		t.Fatal(err)
	}
	total := task.snapshot().OutputBytes
	chunk, from, truncated = task.outputSince(total - 5)
	if truncated || from != total-5 || string(chunk) != "tail\n" {
		t.Fatalf("tail delta = %q from %d (truncated %v)", chunk, from, truncated)
	}
}

// TestTaskSurvivesTheClientThatStartedIt is the regression the whole task
// exists for: MaidKit's HTTP client gives up after ten seconds of silence, and
// before tasks that gave up on a compose update *and* aborted it — the request
// context was the run's context. The run now belongs to the daemon, so a client
// that disconnects mid-update still gets a stack that finished updating.
func TestTaskSurvivesTheClientThatStartedIt(t *testing.T) {
	t.Setenv("FAKE_COMPOSE_SLEEP", "1")
	base, projectDir := composeTaskFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, base+"/api/v1/compose/myapp/update",
		strings.NewReader(`{"directory":"`+projectDir+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	request.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", []byte(`{"directory":"`+projectDir+`"}`)))
	status, body := doRequest(t, request)
	task := taskFrom(t, status, body)

	// The client goes away with the run still in its first stage — the pull is
	// holding still for a second.
	cancel()
	finished := awaitTask(t, base, task.ID)
	if finished.Status != taskStatusSucceeded {
		t.Fatalf("the run died with its client: %+v", finished)
	}
	if !strings.Contains(finished.Stdout, "Recreating web") {
		t.Fatalf("the run stopped before the recreate: %q", finished.Stdout)
	}
}

// TestTaskCancellationStopsTheRun pins the one thing that does stop a task: the
// operator asking. A run left behind must not keep a pull going after the
// person watching it decided against it.
func TestTaskCancellationStopsTheRun(t *testing.T) {
	t.Setenv("FAKE_COMPOSE_SLEEP", "30")
	base, projectDir := composeTaskFixture(t)

	status, body := signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	task := taskFrom(t, status, body)
	if status, body := signedPost(t, base, "/api/v1/tasks/"+task.ID+"/cancel", `{}`); status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	finished := awaitTask(t, base, task.ID)
	if finished.Status != taskStatusCanceled {
		t.Fatalf("canceled task = %+v", finished)
	}
	if finished.Error == "" {
		t.Fatalf("a cancelled run says nothing about why: %+v", finished)
	}
	// Cancelling again is not an error: the caller asked for it to be stopped,
	// and it is.
	if status, body := signedPost(t, base, "/api/v1/tasks/"+task.ID+"/cancel", `{}`); status != http.StatusOK {
		t.Fatalf("second cancel = %d: %s", status, body)
	}
}

// TestTaskStartReportsABusyDaemon pins what a client is told when the daemon is
// already running its limit of operations: the request is answered, not queued
// behind work that will not start.
func TestTaskStartReportsABusyDaemon(t *testing.T) {
	t.Setenv("FAKE_COMPOSE_SLEEP", "30")
	base, projectDir := composeTaskFixture(t)

	status, body := signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	first := taskFrom(t, status, body)

	status, body = signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "already running") {
		t.Fatalf("second start = %d: %s", status, body)
	}
	// The slot is the daemon's, not the request's: it comes back with the run.
	if status, body := signedPost(t, base, "/api/v1/tasks/"+first.ID+"/cancel", `{}`); status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	awaitTask(t, base, first.ID)
	status, body = signedPost(t, base, "/api/v1/compose/myapp/update", `{"directory":"`+projectDir+`"}`)
	taskFrom(t, status, body)
}
