package daemon

import (
	"context"
	"encoding/base64"
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

// fakeCommand writes an executable [name] into a temp dir on PATH and returns
// its path, so exec.LookPath and the runtime probe resolve it.
func fakeCommand(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func stubRuntimes(paths map[string]string) func(context.Context) map[string]string {
	return func(context.Context) map[string]string { return paths }
}

func newTestOpsRunner(t *testing.T, runtimes map[string]string) *nativeOpRunner {
	t.Helper()
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxConcurrentRuns: 2,
	})
	runner := &nativeOpRunner{
		executor: executor,
		runtimes: stubRuntimes(runtimes),
	}
	runner.SetScriptTimeout(time.Second)
	return runner
}

func TestNativeOpValidation(t *testing.T) {
	runner := newTestOpsRunner(t, nil)
	ctx := context.Background()
	cases := []struct {
		slug   string
		params opParams
	}{
		{"container.restart", opParams{target: "bad;id"}},
		{"container.restart", opParams{target: "id with space"}},
		{"container.remove", opParams{target: ""}},
		{"container.frobnicate", opParams{target: "web"}},
		{"process.kill", opParams{pid: 1}},
		{"process.kill", opParams{pid: 0}},
		{"process.kill", opParams{pid: -3}},
		{"systemd.restart", opParams{target: "bad;unit"}},
		{"systemd.stop", opParams{target: ""}},
		{"systemd.enable", opParams{target: "../etc/passwd.service"}},
		{"compose.up", opParams{target: "bad/project", directory: "/srv/app"}},
		{"compose.up", opParams{target: "good", directory: "relative"}},
		{"compose.up", opParams{target: "good", directory: "/srv/../app"}},
		{"compose.up", opParams{target: "good", directory: ""}},
		{"unknown.op", opParams{}},
	}
	for _, tc := range cases {
		_, _, requestErr := runner.dispatch(ctx, tc.slug, tc.params, "test", "tester")
		if requestErr == nil || requestErr.status != http.StatusBadRequest {
			t.Errorf("%s %+v: want 400, got %+v", tc.slug, tc.params, requestErr)
		}
	}
	// A valid container target on a host with no runtime is an execution
	// failure (502), not a bad request.
	_, status, requestErr := runner.dispatch(ctx, "container.restart", opParams{target: "web"}, "test", "tester")
	if requestErr == nil || requestErr.status != http.StatusBadGateway {
		t.Fatalf("no-runtime case: want 502, got status=%d err=%+v", status, requestErr)
	}
}

func TestNativeContainerOpExecutes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	podman := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})
	resp, status, requestErr := runner.dispatch(
		context.Background(), "container.restart", opParams{target: "web"}, "test", "tester",
	)
	if requestErr != nil {
		t.Fatalf("dispatch: %+v", requestErr)
	}
	if status != http.StatusOK || !resp.OK || resp.ExitCode != 0 {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "restart\nweb" {
		t.Fatalf("recorded args %q, want %q", got, "restart\nweb")
	}
}

func TestNativeContainerOpForcedRemove(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	podman := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})
	_, status, requestErr := runner.dispatch(
		context.Background(), "container.remove", opParams{target: "web", force: true}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%+v", status, requestErr)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "rm\n-f\nweb" {
		t.Fatalf("recorded args %q, want %q", got, "rm\n-f\nweb")
	}
}

func TestNativeContainerOpFallsBackToDocker(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	podman := fakeCommand(t, "podman", "#!/bin/sh\necho 'Error: no such container: web' >&2\nexit 125\n")
	docker := fakeCommand(t, "docker", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman, "docker": docker})
	ctx := context.Background()
	resp, status, requestErr := runner.dispatch(ctx, "container.restart", opParams{target: "web"}, "test", "tester")
	t.Logf("DBG runtimes: %v", runner.runtimes(ctx))
	if requestErr != nil || status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, resp)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "restart\nweb") {
		t.Fatalf("docker never ran; recorded %q", got)
	}
}

func TestNativeProcessKillExecutes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	fakeCommand(t, "kill", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	runner := newTestOpsRunner(t, nil)
	resp, status, requestErr := runner.dispatch(
		context.Background(), "process.kill", opParams{pid: 4242}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, resp)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "-s\nKILL\n--\n4242" {
		t.Fatalf("recorded args %q, want %q", got, "-s\nKILL\n--\n4242")
	}
}

func TestNativeSystemdOpNormalizesAndExecutes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	fakeCommand(t, "systemctl", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	runner := newTestOpsRunner(t, nil)
	resp, status, requestErr := runner.dispatch(
		context.Background(), "systemd.restart", opParams{target: "nginx"}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, resp)
	}
	got, _ := os.ReadFile(out)
	t.Logf("DBG got=%q trim=%q eq=%v", got, strings.TrimSpace(string(got)), strings.TrimSpace(string(got)) == "restart\nnginx.service")
	if strings.TrimSpace(string(got)) != "restart\nnginx.service" {
		t.Fatalf("recorded args %q, want %q", got, "restart\nnginx.service")
	}
}

func TestNativeComposeOpUsesDirectoryAndArgs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	dir := t.TempDir()
	podman := fakeCommand(t, "podman", "#!/bin/sh\nprintf 'cwd=%s\\n' \"$PWD\" >> "+out+"\nprintf 'args=%s\\n' \"$*\" >> "+out+"\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})
	resp, status, requestErr := runner.dispatch(
		context.Background(), "compose.up", opParams{target: "myapp", directory: dir}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, resp)
	}
	got, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || lines[0] != "cwd="+dir {
		t.Fatalf("cwd not applied; recorded %q", got)
	}
	if lines[1] != "args=compose --ansi never -p myapp up -d" {
		t.Fatalf("recorded args %q", lines[1])
	}
}

func TestNativeOpReport(t *testing.T) {
	report := nativeOpReport()
	if len(report) != len(config.NativeOpNames) {
		t.Fatalf("report has %d ops, want %d", len(report), len(config.NativeOpNames))
	}
	seen := map[string]bool{}
	for _, op := range report {
		if !op.Enabled || op.Name == "" {
			t.Fatalf("op %+v must be enabled with a name", op)
		}
		seen[op.Name] = true
	}
	for _, slug := range config.NativeOpNames {
		if !seen[slug] {
			t.Fatalf("report missing %q", slug)
		}
	}
}

func TestNativeParamsFromValues(t *testing.T) {
	parse := func(slug, body string) opParams {
		var values map[string]any
		if err := json.Unmarshal([]byte(body), &values); err != nil {
			t.Fatal(err)
		}
		return nativeParamsFromValues(slug, values)
	}
	if p := parse("container.remove", `{"id":"web","force":true}`); p.target != "web" || !p.force {
		t.Fatalf("container params %+v", p)
	}
	if p := parse("process.kill", `{"pid":42}`); p.pid != 42 {
		t.Fatalf("process params %+v", p)
	}
	if p := parse("systemd.restart", `{"unit":"nginx"}`); p.target != "nginx" {
		t.Fatalf("systemd params %+v", p)
	}
	if p := parse("compose.up", `{"project":"app","directory":"/srv/app"}`); p.target != "app" || p.directory != "/srv/app" {
		t.Fatalf("compose params %+v", p)
	}
}

func TestRelayDispatchesNativeOp(t *testing.T) {
	var resultBody []byte
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/webhook-requests/r1/result") {
			resultBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected cloud request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cloud.Close()

	out := filepath.Join(t.TempDir(), "out")
	podman := fakeCommand(t, "podman", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+out+"\n")
	executor := NewWebhookExecutor(config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxConcurrentRuns: 1,
	})
	runner := &nativeOpRunner{
		executor: executor,
		runtimes: stubRuntimes(map[string]string{"podman": podman}),
	}
	runner.SetScriptTimeout(time.Second)
	publisher, err := NewCloudPublisher(config.DaemonConfig{
		ID:             "host-1",
		CloudURL:       cloud.URL,
		CloudSecret:    "cloud-secret",
		RequestTimeout: time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	publisherBox := &atomic.Pointer[CloudPublisher]{}
	publisherBox.Store(publisher)
	relay := NewWebhookRelay(publisherBox, executor, runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay.process(context.Background(), relayWebhookRequest{
		ID:        "r1",
		Name:      "container.restart",
		Body:      base64.StdEncoding.EncodeToString([]byte(`{"id":"web"}`)),
		InvokedBy: "@alice",
	})

	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "restart\nweb" {
		t.Fatalf("native op did not run; recorded %q", got)
	}
	if resultBody == nil {
		t.Fatal("relay never reported a result")
	}
	var result relayResultPayload
	if err := json.Unmarshal(resultBody, &result); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if result.Code != http.StatusOK {
		t.Fatalf("result code %d", result.Code)
	}
}
