package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// The digests a container's local image was pulled at and its registry now
// publishes. They differ, which is how the tests below get an "outdated"
// answer to assert on.
const (
	containerLocalDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	containerRemoteDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	containerRunningID    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	containerLocalImageID = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	containerID           = "abcdef1234567890abcdef"
)

// composeLabels is the compose identity docker compose records on the
// containers it creates, with the project directory pointing at a path the
// test controls: an update actually runs `compose` in it.
func composeLabels(workingDir string) string {
	return fmt.Sprintf(
		`{"com.docker.compose.project":"app","com.docker.compose.service":"web","com.docker.compose.project.working_dir":%q,"com.docker.compose.project.config_files":%q}`,
		workingDir, filepath.Join(workingDir, "compose.yml"),
	)
}

// fakeRuntimeScript answers the container detail reads a test needs with fixed
// payloads, and records the mutating calls (a pull, a compose step) so a test
// can assert the argv the daemon built. [labels] is what the container's
// inspect reports, which is how the compose-managed case is selected.
func fakeRuntimeScript(labels string) string {
	return `#!/bin/sh
record() { if [ -n "$FAKE_RUNTIME_CALLS" ]; then printf '%s\n' "$@" >> "$FAKE_RUNTIME_CALLS"; fi }
case "$1" in
ps)
	printf '%s\n' '{"ID":"` + containerID + `","Names":["web"],"Image":"nginx:1.25","State":"running","Status":"Up 2 hours","Labels":` + labels + `}'
	;;
inspect)
	printf '%s\n' '{"Id":"` + containerID + `","Image":"` + containerRunningID + `","ImageName":"docker.io/library/nginx:1.25","Config":{"Image":"nginx:1.25","Labels":` + labels + `}}'
	;;
image)
	printf '%s\n' '{"Id":"` + containerLocalImageID + `","RepoDigests":["docker.io/library/nginx@` + containerLocalDigest + `"]}'
	;;
stats)
	printf '%s\n' '[{"id":"` + containerID + `","name":"web","cpu_percent":"1.50%","mem_usage":"3.092MB / 16.7GB","mem_percent":"0.02%","netio":"1.0kB / 2.0kB","blocki":"0B / 0B","pids":"7"}]'
	;;
logs)
	echo "2024-01-01T00:00:02.000000000Z on stdout"
	echo "2024-01-01T00:00:01.000000000Z on stderr" >&2
	;;
pull)
	record "$@"
	printf 'Pulled\n'
	;;
compose)
	printf 'cwd=%s\n' "$PWD" >> "$FAKE_RUNTIME_CALLS"
	record "$@"
	printf 'Composed\n'
	;;
esac
`
}

// fakeRuntimeBinary installs the fake runtime as the only one on PATH. PATH is
// narrowed to it plus the system directories: a detail test must resolve
// exactly the runtime it installed, never a real runtime on the machine
// running the tests.
//
// A `sudo` that refuses everything is installed beside it, so no test reaches
// the machine's real sudo by accident: a store that needs elevation is then
// simply out of reach, which is the deterministic answer. A test that wants
// elevation installs its own fake after this one, and PATH puts it first.
func fakeRuntimeBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return path
}

// recordedRuntimeCalls returns the calls the fake runtime recorded.
func recordedRuntimeCalls(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// detailTestConfig is the minimum daemon configuration the detail and update
// endpoints need, with the background cadences pushed out so a test drives only
// the endpoint or check it is testing.
func detailTestConfig() config.DaemonConfig {
	return config.DaemonConfig{
		ID:                  "detail-host",
		Version:             "v9.9.9",
		Transport:           "http",
		Listen:              "127.0.0.1:0",
		MetricsSecret:       "metrics-secret",
		MetricsInterval:     time.Hour,
		StreamInterval:      time.Second,
		ContainersInterval:  time.Hour,
		LogsInterval:        0,
		UpdateCheckInterval: 6 * time.Hour,
		Runtimes:            []string{"java"},
		ProcessesLimit:      50,
		RequestTimeout:      5 * time.Second,
		ScriptTimeout:       time.Second,
		MaxBodyBytes:        4096,
		MaxConcurrentRuns:   1,
	}
}

// detailRequest performs one authorized GET against a running test daemon.
func detailRequest(t *testing.T, base, path string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestContainerDetailEndpoints(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))

	app, err := NewApp(detailTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	base := "http://" + app.ListenAddr()

	if status, body := detailRequest(t, base, "/api/v1/containers/web/inspect"); status != http.StatusOK {
		t.Fatalf("inspect status = %d: %s", status, body)
	} else {
		var payload struct {
			Container string `json:"container"`
			Runtime   string `json:"runtime"`
			Inspect   struct {
				Config struct {
					Image string `json:"Image"`
				} `json:"Config"`
			} `json:"inspect"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("inspect body %s: %v", body, err)
		}
		if payload.Container != containerID || payload.Runtime != "podman" {
			t.Fatalf("inspect identity = %+v", payload)
		}
		if payload.Inspect.Config.Image != "nginx:1.25" {
			t.Fatalf("inspect image = %q, want nginx:1.25", payload.Inspect.Config.Image)
		}
	}

	// An id prefix resolves the same container as its name.
	if status, body := detailRequest(t, base, "/api/v1/containers/abcdef/inspect"); status != http.StatusOK {
		t.Fatalf("inspect by prefix status = %d: %s", status, body)
	}
	if status, _ := detailRequest(t, base, "/api/v1/containers/does-not-exist/inspect"); status != http.StatusNotFound {
		t.Fatalf("unknown container status = %d, want 404", status)
	}

	status, body := detailRequest(t, base, "/api/v1/containers/web/stats")
	if status != http.StatusOK {
		t.Fatalf("stats status = %d: %s", status, body)
	}
	var stats containerStats
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("stats body %s: %v", body, err)
	}
	if stats.CPUPercent == nil || *stats.CPUPercent != 1.5 {
		t.Fatalf("stats cpu = %v", stats.CPUPercent)
	}
	if stats.MemoryUsageBytes == nil || *stats.MemoryUsageBytes != 3092000 {
		t.Fatalf("stats memory usage = %v", stats.MemoryUsageBytes)
	}
	if stats.MemoryLimitBytes == nil || *stats.MemoryLimitBytes != 16700000000 {
		t.Fatalf("stats memory limit = %v", stats.MemoryLimitBytes)
	}
	if stats.NetworkInputBytes == nil || *stats.NetworkInputBytes != 1000 {
		t.Fatalf("stats network input = %v", stats.NetworkInputBytes)
	}
	if stats.PIDs == nil || *stats.PIDs != 7 {
		t.Fatalf("stats pids = %v", stats.PIDs)
	}

	// The captured store has nothing for this container; the runtime does, and
	// what an application wrote to stderr has to come through too.
	status, body = detailRequest(t, base, "/api/v1/containers/web/logs")
	if status != http.StatusOK {
		t.Fatalf("captured logs status = %d: %s", status, body)
	}
	var captured struct {
		Source string             `json:"source"`
		Lines  []containerLogLine `json:"lines"`
	}
	if err := json.Unmarshal(body, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.Source != "captured" || len(captured.Lines) != 0 {
		t.Fatalf("captured logs = %+v", captured)
	}

	status, body = detailRequest(t, base, "/api/v1/containers/web/logs?source=runtime")
	if status != http.StatusOK {
		t.Fatalf("runtime logs status = %d: %s", status, body)
	}
	var live struct {
		Source string             `json:"source"`
		Lines  []containerLogLine `json:"lines"`
	}
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatal(err)
	}
	if live.Source != "runtime" || len(live.Lines) != 2 {
		t.Fatalf("runtime logs = %+v", live)
	}
	lines := make([]string, 0, 2)
	for _, line := range live.Lines {
		lines = append(lines, line.Line)
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "on stdout") || !strings.Contains(joined, "on stderr") {
		t.Fatalf("runtime log lines = %q", joined)
	}

	if status, _ := detailRequest(t, base, "/api/v1/containers/web/logs?source=elsewhere"); status != http.StatusBadRequest {
		t.Fatalf("unknown log source status = %d, want 400", status)
	}
	if status, _ := detailRequest(t, base, "/api/v1/containers/web/logs?source=runtime&lines=0"); status != http.StatusBadRequest {
		t.Fatalf("zero lines status = %d, want 400", status)
	}
	if status, _ := detailRequest(t, base, "/api/v1/containers/nope/logs?source=runtime"); status != http.StatusNotFound {
		t.Fatalf("runtime logs for unknown container status = %d, want 404", status)
	}

	if recorded := recordedRuntimeCalls(t, calls); len(recorded) != 0 {
		t.Fatalf("detail reads must not mutate anything: %v", recorded)
	}
}

func TestParseContainerStatsNormalizesBothRuntimes(t *testing.T) {
	dockerPayload := []byte(`{"Container":"abc","Name":"web","CPUPerc":"0.25%","MemUsage":"1.5MiB / 1.9GiB","MemPerc":"0.08%","NetIO":"1.2kB / 3.4kB","BlockIO":"0B / 0B","PIDs":"3"}`)
	stats, err := parseContainerStats(dockerPayload, containerRef{Runtime: "docker", ID: "abc", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Name != "web" || stats.CPUPercent == nil || *stats.CPUPercent != 0.25 {
		t.Fatalf("docker stats = %+v", stats)
	}
	if stats.MemoryUsageBytes == nil || *stats.MemoryUsageBytes != 1572864 {
		t.Fatalf("docker memory usage = %v, want 1.5MiB", stats.MemoryUsageBytes)
	}
	if stats.MemoryLimitBytes == nil || *stats.MemoryLimitBytes != 2040109465 {
		t.Fatalf("docker memory limit = %v", stats.MemoryLimitBytes)
	}
	if stats.PIDs == nil || *stats.PIDs != 3 {
		t.Fatalf("docker pids = %v", stats.PIDs)
	}

	// podman answers with an array and reports "--" for what a rootless
	// runtime cannot see, which must stay null rather than become zero.
	podmanPayload := []byte(`[{"id":"abc","name":"web","cpu_percent":"--","mem_usage":"3.092MB / 16.7GB","mem_percent":"0.02%","netio":"-- / --","blocki":"-- / --","pids":"2"}]`)
	stats, err = parseContainerStats(podmanPayload, containerRef{Runtime: "podman", ID: "abc", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.CPUPercent != nil || stats.NetworkInputBytes != nil || stats.BlockInputBytes != nil {
		t.Fatalf("unreported measurements must stay null: %+v", stats)
	}
	if stats.MemoryUsageBytes == nil || *stats.MemoryUsageBytes != 3092000 {
		t.Fatalf("podman memory usage = %v", stats.MemoryUsageBytes)
	}
	if stats.MemoryPercent == nil || *stats.MemoryPercent != 0.02 {
		t.Fatalf("podman memory percent = %v", stats.MemoryPercent)
	}
}

func TestParseContainerInspectHandlesBothShapes(t *testing.T) {
	// docker renders one object through its template engine.
	single, err := parseContainerInspect([]byte(`{"Id":"abc","Image":"sha256:run","Config":{"Image":"nginx:1.25","Labels":{"a":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if single.ImageRef != "nginx:1.25" || single.ImageID != "sha256:run" || single.Labels["a"] != "1" {
		t.Fatalf("single inspect = %+v", single)
	}

	// podman answers with an array and names the image at the top level.
	list, err := parseContainerInspect([]byte(`[{"Id":"abc","Image":"sha256:run","ImageName":"docker.io/library/redis:7","Config":{"Image":"sha256:run","Labels":{"b":"2"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if list.ImageRef != "docker.io/library/redis:7" || list.Labels["b"] != "2" {
		t.Fatalf("list inspect = %+v", list)
	}

	// A container created from an image ID has no reference to pull.
	byID, err := parseContainerInspect([]byte(`{"Id":"abc","Image":"sha256:run","Config":{"Image":"sha256:run"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if byID.ImageRef != "" {
		t.Fatalf("image-ID container must report no reference: %+v", byID)
	}
}
