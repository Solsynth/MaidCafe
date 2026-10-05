package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeRuntimeInvocations reads what a fake runtime recorded and drops the
// `info` probe the daemon runs before it elevates a runtime command, because
// the tests that use this assert what an *operation* ran, not what the daemon
// asked the runtime about itself on the way there. The probe is covered by
// TestRuntimeElevationSkipsRootlessRuntime.
//
// Both recording styles are handled: one argument per line (the usual fake),
// and `cwd=`/`args=` pairs (the compose fake, which also records its working
// directory).
func fakeRuntimeInvocations(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	paired := false
	for _, line := range lines {
		if strings.HasPrefix(line, "args=") {
			paired = true
			break
		}
	}
	out := make([]string, 0, len(lines))
	if !paired {
		// `info --format <template>` is three lines of the probe.
		for i := 0; i < len(lines); i++ {
			if lines[i] == "info" && i+2 < len(lines) && lines[i+1] == "--format" {
				i += 2
				continue
			}
			out = append(out, lines[i])
		}
		return out
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "args=info --format ") {
			if len(out) > 0 && strings.HasPrefix(out[len(out)-1], "cwd=") {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// recordedRuntimeArgs joins fakeRuntimeInvocations back into the single string
// the one-argument-per-line tests compare against.
func recordedRuntimeArgs(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(fakeRuntimeInvocations(t, path), "\n")
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
	if got := recordedRuntimeArgs(t, out); got != "restart\nweb" {
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
	if got := recordedRuntimeArgs(t, out); got != "rm\n-f\nweb" {
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

// fakeSudoScript is a sudo that hands the command to the fake runtime with
// FAKE_STORE=root set, which is how the fake runtime knows it is root's store
// answering. The `--version` probe the daemon runs to ask whether it may run a
// tool at all is answered here: the standalone compose tools get
// [grantsComposeTool]'s verdict, everything else is permitted.
func fakeSudoScript(grantsComposeTool bool) string {
	verdict := "exit 1"
	if grantsComposeTool {
		verdict = "exit 0"
	}
	return "#!/bin/sh\n" +
		"if [ \"$3\" = \"--version\" ]; then\n" +
		"  case \"$2\" in\n" +
		"    *podman-compose|*docker-compose) " + verdict + " ;;\n" +
		"  esac\n" +
		"  exit 0\n" +
		"fi\n" +
		"shift\n" +
		"exec env FAKE_STORE=root \"$@\"\n"
}

// fakeStoreRuntimeScript is a podman with two stores: what it lists, inspects
// and creates depends on which user runs it — its own, or root's when the fake
// sudo set FAKE_STORE=root. Every call is recorded with the store it ran in and
// its argv, which is what the store-pinning tests assert on. A store whose
// project is empty holds nothing, so a container or project lookup there fails
// the way a real one does.
func fakeStoreRuntimeScript(own, root, dir string) string {
	quote := func(value string) string {
		return "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
	}
	listing := func(project string) string {
		if project == "" {
			return ":\n"
		}
		name := project + "_drasl_1"
		labels := "com.docker.compose.project=" + project
		return "printf '%s\\n' " + quote(fmt.Sprintf(
			`{"ID":"%s0123456789","Names":["%s"],"State":"running","Labels":"%s"}`,
			project, name, labels)) + "\n"
	}
	inspect := func(project, id string) string {
		if project == "" {
			return "printf 'Error: no such container\\n' >&2\nexit 125\n"
		}
		return "printf '%s\\n' " + quote(fmt.Sprintf(
			`{"Id":"%s","ImageName":"docker.io/unmojang/drasl","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"drasl","com.docker.compose.project.working_dir":"%s","com.docker.compose.project.config_files":"%s"}}}`,
			id, project, dir, filepath.Join(dir, "compose.yml"))) + "\n"
	}
	return "#!/bin/sh\n" +
		"store=\"${FAKE_STORE:-own}\"\n" +
		"if [ -n \"$FAKE_STORE_CALLS\" ]; then printf 'store=%s argv=%s\\n' \"$store\" \"$*\" >> \"$FAKE_STORE_CALLS\"; fi\n" +
		"case \"$1\" in\n" +
		"ps)\n" +
		"  if [ \"$store\" = root ]; then " + strings.TrimSpace(listing(root)) + "; else " + strings.TrimSpace(listing(own)) + "; fi\n" +
		"  ;;\n" +
		"inspect)\n" +
		"  if [ \"$store\" = root ]; then\n" + inspect(root, "root0123456789") +
		"  else\n" + inspect(own, "own0123456789") + "  fi\n" +
		"  ;;\n" +
		"info) printf 'true cgroupfs\\n' ;;\n" +
		"*) printf 'ran\\n' ;;\n" +
		"esac\n"
}

// readCallLog returns the calls a fake runtime recorded.
func readCallLog(t *testing.T, path string) []string {
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

// TestContainerUpdateRecreatesInTheStoreThatOwnsTheProject is the regression
// test for a second stack appearing next to a running one.
//
// The daemon user's podman and root's podman are two stores, and a project that
// lives in root's — the shape of a host whose stacks an operator started with
// sudo — used to be recreated by the unprivileged attempt first: that attempt
// created the whole project again in the daemon user's store, under the same
// container names, where it could not bind the ports the real one holds, and it
// left a container behind for every update. The step must run in the store that
// holds the project, and it must run the tool the operator's stacks are made
// with — the standalone `podman-compose` where the host has it — rather than
// substituting the runtime's own compose wrapper for it.
func TestContainerUpdateRecreatesInTheStoreThatOwnsTheProject(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a daemon running as root has one store")
	}
	// A standalone tool that records its own argv with the store it ran in.
	const standaloneBody = "#!/bin/sh\nprintf 'store=%s standalone argv=%s\\n' \"${FAKE_STORE:-own}\" \"$*\" >> \"$FAKE_STORE_CALLS\"\n"
	for _, tc := range []struct {
		name       string
		standalone bool
		grantsTool bool
		wantErr    []string
		wantSteps  []string
	}{
		{
			// The operator's own tool, through sudo, in root's store.
			name: "the granted standalone tool runs", standalone: true, grantsTool: true,
			wantSteps: []string{
				"store=root standalone argv=-p drasl -f FILE pull drasl",
				"store=root standalone argv=-p drasl -f FILE up -d --force-recreate drasl",
			},
		},
		{
			// The shipped sudoers rule grants the runtime and not the tool. The
			// step is refused with the line that would allow it: falling through
			// to the wrapper would hand the project to a different compose
			// implementation than the one that owns it.
			name: "a granted runtime does not stand in for the tool", standalone: true, grantsTool: false,
			wantErr: []string{"STANDALONE", "NOPASSWD", "root's store"},
		},
		{
			// No standalone tool on the host: the runtime's own subcommand is
			// the only compose tool there is, so using it mixes nothing.
			name: "the wrapper is the only tool", standalone: false, grantsTool: true,
			wantSteps: []string{
				"store=root argv=compose -p drasl -f FILE pull drasl",
				"store=root argv=compose -p drasl -f FILE up -d --force-recreate drasl",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			t.Setenv("FAKE_STORE_CALLS", calls)
			dir := t.TempDir()
			standalonePath := ""
			if tc.standalone {
				standalonePath = fakeCommand(t, "podman-compose", standaloneBody)
			}
			fakeCommand(t, "sudo", fakeSudoScript(tc.grantsTool))
			path := fakeCommand(t, "podman", fakeStoreRuntimeScript("", "drasl", dir))
			runner := newTestOpsRunner(t, map[string]string{"podman": path})

			response, status, requestErr := runner.dispatch(
				context.Background(), "container.update", opParams{target: "drasl_drasl_1"}, "test", "tester",
			)
			file := filepath.Join(dir, "compose.yml")
			if len(tc.wantErr) > 0 {
				if requestErr == nil || requestErr.status != http.StatusBadRequest {
					t.Fatalf("status=%d err=%+v, want 400", status, requestErr)
				}
				for _, want := range tc.wantErr {
					if want == "STANDALONE" {
						want = standalonePath
					}
					if !strings.Contains(requestErr.message, want) {
						t.Fatalf("message = %q, want it to mention %q", requestErr.message, want)
					}
				}
			} else if requestErr != nil || status != http.StatusOK || !response.OK {
				t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
			}
			// The steps, exactly: which store they ran in, which tool ran them
			// and with which argv. Anything else recorded is a store probe.
			var got []string
			for _, call := range readCallLog(t, calls) {
				if !strings.Contains(call, "standalone argv=") && !strings.Contains(call, "argv=compose") {
					continue
				}
				got = append(got, strings.ReplaceAll(call, file, "FILE"))
			}
			if strings.Join(got, "|") != strings.Join(tc.wantSteps, "|") {
				t.Fatalf("steps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.wantSteps, "\n"))
			}
		})
	}
}

// TestContainerUpdateRefusesAProjectInTwoStores pins the answer to a project
// that exists twice — the state a phantom copy leaves behind. Choosing one
// silently is how the confusion started, so the daemon names both and stops.
func TestContainerUpdateRefusesAProjectInTwoStores(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a daemon running as root has one store")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_STORE_CALLS", calls)
	fakeCommand(t, "sudo", fakeSudoScript(false))
	path := fakeCommand(t, "podman", fakeStoreRuntimeScript("drasl", "drasl", t.TempDir()))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	_, _, requestErr := runner.dispatch(
		context.Background(), "container.update", opParams{target: "drasl_drasl_1"}, "test", "tester",
	)
	if requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("ambiguous project update = %+v, want 400", requestErr)
	}
	for _, want := range []string{"more than one store", "daemon user's own store", "root's store", "drasl_drasl_1"} {
		if !strings.Contains(requestErr.message, want) {
			t.Fatalf("message = %q, want it to mention %q", requestErr.message, want)
		}
	}
	for _, call := range readCallLog(t, calls) {
		if strings.Contains(call, "argv=compose") {
			t.Fatalf("an ambiguous project was written to anyway: %q", call)
		}
	}
}

// TestComposeStepStaysInTheOwnStoreWhenTheProjectIsThere pins the other half:
// a project the daemon user runs is not touched through sudo, which would be
// the same mistake in the other direction — recreating it in root's store.
func TestComposeStepStaysInTheOwnStoreWhenTheProjectIsThere(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a daemon running as root has one store")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_STORE_CALLS", calls)
	dir := t.TempDir()
	fakeCommand(t, "sudo", fakeSudoScript(true))
	path := fakeCommand(t, "podman", fakeStoreRuntimeScript("myapp", "", dir))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	response, status, requestErr := runner.dispatch(
		context.Background(), "compose.up", opParams{target: "myapp", directory: dir}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !response.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
	}
	var steps []string
	for _, call := range readCallLog(t, calls) {
		if strings.Contains(call, "argv=compose") {
			if !strings.HasPrefix(call, "store=own") {
				t.Fatalf("an own-store project was written through sudo: %q", call)
			}
			steps = append(steps, strings.SplitN(call, "argv=", 2)[1])
		}
	}
	// The directory was given rather than a stack the daemon scanned, so no
	// file is named: compose reads the default one in the project directory.
	want := "compose -p myapp up -d"
	if got := strings.Join(steps, "|"); got != want {
		t.Fatalf("compose steps = %q, want %q", got, want)
	}
}

// TestComposeAttemptsRefusesWhenNoToolReachesTheStore pins the guard on the
// filtered tool list: when every compose tool is one `sudo -n` may not run, the
// step is refused with the grant that would allow it instead of being built in
// the wrong store, or built empty and reported as a success.
func TestComposeAttemptsRefusesWhenNoToolReachesTheStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a daemon running as root has one store")
	}
	fakeCommand(t, "sudo", "#!/bin/sh\nexit 1\n")
	path := fakeCommand(t, "podman", "#!/bin/sh\nexit 0\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	_, err := runner.composeAttempts(
		context.Background(),
		composeStore{Runtime: "podman", Path: path, Elevated: true},
		composeUpdateTarget{Project: "drasl", Directory: t.TempDir()},
		"up", "-d",
	)
	if err == nil {
		t.Fatal("a step with no way to reach its store was built anyway")
	}
	for _, want := range []string{"root's store", "sudo -n", "NOPASSWD"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

func TestNativeComposeOpUsesDirectoryAndArgs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	dir := t.TempDir()
	// A sudo that refuses everything: this test is about the argv the daemon
	// builds in the store the project lives in, and the machine's own sudo has
	// no business being asked. The store probe is answered empty and unrecorded,
	// so only the compose step itself is in the log.
	fakeCommand(t, "sudo", "#!/bin/sh\nexit 1\n")
	podman := fakeCommand(t, "podman", "#!/bin/sh\n"+
		"[ \"$1\" = ps ] && exit 0\n"+
		"printf 'cwd=%s\\n' \"$PWD\" >> "+out+"\n"+
		"printf 'args=%s\\n' \"$*\" >> "+out+"\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})
	resp, status, requestErr := runner.dispatch(
		context.Background(), "compose.up", opParams{target: "myapp", directory: dir}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !resp.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, resp)
	}
	steps := fakeRuntimeInvocations(t, out)
	if len(steps) != 2 || steps[0] != "cwd="+dir {
		t.Fatalf("cwd not applied; recorded %#v", steps)
	}
	if steps[1] != "args=compose -p myapp up -d" {
		t.Fatalf("recorded args %q", steps[1])
	}
}

// fakeProjectContainer is one container a fake runtime reports as part of a
// project: what the store says it is running, and where its image reference
// points now. Equal IDs mean the container is current.
type fakeProjectContainer struct {
	id       string
	name     string
	service  string
	image    string
	running  string
	resolves string
	state    string
}

// fakeProjectRuntime returns a runtime that lists [containers] as the
// containers of [project], answers the two reads a stack update compares with —
// the image a container runs, and the image a reference points at — and records
// every compose step it is asked to run under FAKE_STORE_CALLS.
func fakeProjectRuntime(project string, containers []fakeProjectContainer) string {
	lines := make([]string, 0, len(containers))
	byID := make([]string, 0, len(containers))
	byRef := make([]string, 0, len(containers))
	seen := make(map[string]bool, len(containers))
	for _, container := range containers {
		state := container.state
		if state == "" {
			state = "running"
		}
		lines = append(lines, fmt.Sprintf(
			`{"Id":%q,"Names":[%q],"Image":%q,"State":%q,"Status":"Up","Labels":{"com.docker.compose.project":%q,"com.docker.compose.service":%q}}`,
			container.id, container.name, container.image, state, project, container.service))
		// `inspect --format {{.Image}} <id>`: the image that container runs.
		byID = append(byID, fmt.Sprintf("%s) printf '%%s\\n' %q ;;", container.id, container.running))
		if !seen[container.image] {
			seen[container.image] = true
			// `image inspect --format {{.Id}} <ref>`: where that reference points.
			byRef = append(byRef, fmt.Sprintf("%s) printf '%%s\\n' %q ;;", container.image, container.resolves))
		}
	}
	return "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"ps)\n" +
		"  cat <<'EOF'\n" + strings.Join(lines, "\n") + "\nEOF\n" +
		"  ;;\n" +
		"inspect)\n" +
		"  case \"$4\" in\n" + strings.Join(byID, "\n") + "\n  esac\n" +
		"  ;;\n" +
		"image)\n" +
		"  case \"$5\" in\n" + strings.Join(byRef, "\n") + "\n  esac\n" +
		"  ;;\n" +
		"compose)\n" +
		"  printf 'cwd=%s\\n' \"$PWD\" >> \"$FAKE_STORE_CALLS\"\n" +
		"  printf 'args=%s\\n' \"$*\" >> \"$FAKE_STORE_CALLS\"\n" +
		"  ;;\n" +
		"esac\n"
}

// composeSteps returns the compose invocations a fake runtime recorded, in
// order.
func composeSteps(t *testing.T, path string) []string {
	t.Helper()
	steps := make([]string, 0, 2)
	for _, call := range readCallLog(t, path) {
		if args, ok := strings.CutPrefix(call, "args="); ok {
			steps = append(steps, args)
		}
	}
	return steps
}

// updateStackOn runs one stack update against a fake runtime installed with
// [containers] as the project's containers.
func updateStackOn(t *testing.T, calls string, containers []fakeProjectContainer) executionResponse {
	t.Helper()
	t.Setenv("FAKE_STORE_CALLS", calls)
	fakeCommand(t, "sudo", "#!/bin/sh\nexit 1\n")
	podman := fakeCommand(t, "podman", fakeProjectRuntime("myapp", containers))
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})
	response, status, requestErr := runner.dispatch(
		context.Background(), "compose.update",
		opParams{target: "myapp", directory: t.TempDir()}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !response.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
	}
	return response
}

// TestComposeUpdateRecreatesOnlyTheContainersThatMoved is the point of the
// stack update's second half: the pull moves some services' images, and only
// those are recreated. The stack's cache and database are not bounced because
// an application image moved.
func TestComposeUpdateRecreatesOnlyTheContainersThatMoved(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	updateStackOn(t, calls, []fakeProjectContainer{
		{id: "c-web", name: "myapp_web_1", service: "web", image: "nginx:1.25", running: "sha256:old", resolves: "sha256:new"},
		{id: "c-db", name: "myapp_db_1", service: "db", image: "postgres:16", running: "sha256:db", resolves: "sha256:db"},
	})
	want := []string{"compose -p myapp pull", "compose -p myapp up -d --force-recreate web"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v", got, want)
	}
}

// TestComposeUpdateRecreatesAStoppedContainer pins the other half of "which
// containers an update touches": one that is down is brought up on its image
// whether or not that image moved, which is what a compose `up` means.
func TestComposeUpdateRecreatesAStoppedContainer(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	updateStackOn(t, calls, []fakeProjectContainer{
		{id: "c-web", name: "myapp_web_1", service: "web", image: "nginx:1.25", running: "sha256:same", resolves: "sha256:same", state: "exited"},
	})
	want := []string{"compose -p myapp pull", "compose -p myapp up -d --force-recreate web"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v", got, want)
	}
}

// TestComposeUpdateRecreatesNothingWhenNothingMoved pins what an up-to-date
// stack costs: nothing. Pulling is what the caller asked for, and a project
// whose containers all run the image their references point at has no recreate
// to run — the stage says so instead of restarting services for no reason.
func TestComposeUpdateRecreatesNothingWhenNothingMoved(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	response := updateStackOn(t, calls, []fakeProjectContainer{
		{id: "c-web", name: "myapp_web_1", service: "web", image: "nginx:1.25", running: "sha256:same", resolves: "sha256:same"},
		{id: "c-db", name: "myapp_db_1", service: "db", image: "postgres:16", running: "sha256:db", resolves: "sha256:db"},
	})
	want := []string{"compose -p myapp pull"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v", got, want)
	}
	if !strings.Contains(response.Stdout, "nothing to recreate") {
		t.Fatalf("the run did not say why it recreated nothing: %q", response.Stdout)
	}
}

// TestComposeUpdateCreatesAProjectWithNoContainers pins the exception: a
// project this store holds no container of is created, not compared, so the
// recreate stays the whole-project one.
func TestComposeUpdateCreatesAProjectWithNoContainers(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	updateStackOn(t, calls, nil)
	want := []string{"compose -p myapp pull", "compose -p myapp up -d --force-recreate"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v", got, want)
	}
}

// TestComposeUpdateFallsBackToTheWholeProjectWhenAContainerCannotBeNamed pins
// the other exception: a container that is behind but whose labels name no
// service cannot be singled out in an argv, and it is not left behind either —
// compose is handed the whole project instead of a partial list it would
// silently skip that container from.
func TestComposeUpdateFallsBackToTheWholeProjectWhenAContainerCannotBeNamed(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	updateStackOn(t, calls, []fakeProjectContainer{
		{id: "c-web", name: "myapp_web_1", service: "", image: "nginx:1.25", running: "sha256:old", resolves: "sha256:new"},
	})
	want := []string{"compose -p myapp pull", "compose -p myapp up -d --force-recreate"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v", got, want)
	}
}

// TestComposeUpdateFailsWhenTheStoreCannotBeListed pins the one comparison that
// is not made on a guess: a store that will not say what the project holds
// stops the update after its pull, rather than handing compose a recreate the
// daemon cannot justify.
func TestComposeUpdateFailsWhenTheStoreCannotBeListed(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_STORE_CALLS", calls)
	fakeCommand(t, "sudo", "#!/bin/sh\nexit 1\n")
	podman := fakeCommand(t, "podman", "#!/bin/sh\n"+
		"case \"$1\" in\n"+
		"ps) exit 1 ;;\n"+
		"compose) printf 'args=%s\\n' \"$*\" >> \"$FAKE_STORE_CALLS\" ;;\n"+
		"esac\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": podman})

	response, status, requestErr := runner.dispatch(
		context.Background(), "compose.update",
		opParams{target: "myapp", directory: t.TempDir()}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusBadGateway || response.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
	}
	want := []string{"compose -p myapp pull"}
	if got := composeSteps(t, calls); !equalStrings(got, want) {
		t.Fatalf("compose steps = %#v, want %#v (the pull ran, the recreate did not)", got, want)
	}
}

// TestNativeOpReport pins the report the cloud lists invocable operations from.
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

	if got := recordedRuntimeArgs(t, out); got != "restart\nweb" {
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
func TestNativeOpFailurePublishesNativeChannel(t *testing.T) {
	var mu sync.Mutex
	var notifications []notificationPayload
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/notifications") {
			var got notificationPayload
			_ = json.NewDecoder(r.Body).Decode(&got)
			mu.Lock()
			notifications = append(notifications, got)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cloud.Close()
	publisher, err := NewCloudPublisher(config.DaemonConfig{
		ID: "host-1", CloudURL: cloud.URL, CloudSecret: "s", RequestTimeout: time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	box := &atomic.Pointer[CloudPublisher]{}
	box.Store(publisher)
	failing := fakeCommand(t, "podman", "#!/bin/sh\necho 'no such container' >&2\nexit 1\n")
	executor := NewWebhookExecutor(config.DaemonConfig{ScriptTimeout: time.Second, MaxConcurrentRuns: 2})
	runner := &nativeOpRunner{
		executor:  executor,
		runtimes:  stubRuntimes(map[string]string{"podman": failing}),
		publisher: box,
	}
	runner.SetScriptTimeout(time.Second)
	ctx := context.Background()

	// A failed manual removal reports on the native channel with its target.
	response, status, reqErr := runner.dispatch(ctx, "container.remove", opParams{target: "web", force: true}, "http", "")
	if reqErr != nil {
		t.Fatalf("dispatch returned request error: %+v", reqErr)
	}
	if status != http.StatusBadGateway || response.OK {
		t.Fatalf("expected failed execution, got status=%d ok=%v", status, response.OK)
	}
	mu.Lock()
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	got := notifications[0]
	mu.Unlock()
	if got.Kind != "nativeop.failure" {
		t.Fatalf("expected nativeop.failure, got %q", got.Kind)
	}
	if got.Title != "Remove container failed" {
		t.Fatalf("unexpected title %q", got.Title)
	}
	if got.Metadata["name"] != "container.remove" || got.Metadata["target"] != "web" {
		t.Fatalf("notification metadata missing slug/target: %+v", got.Metadata)
	}

	// Scheduled jobs keep their own job.failure channel: no double report.
	if _, _, reqErr := runner.dispatch(ctx, "container.remove", opParams{target: "web"}, "job", ""); reqErr != nil {
		t.Fatalf("job dispatch returned request error: %+v", reqErr)
	}
	// Successes stay silent: routine operations notify nobody.
	succeeding := fakeCommand(t, "podman", "#!/bin/sh\nexit 0\n")
	runner.runtimes = stubRuntimes(map[string]string{"podman": succeeding})
	if _, status, reqErr := runner.dispatch(ctx, "container.stop", opParams{target: "web"}, "http", ""); reqErr != nil || status != http.StatusOK {
		t.Fatalf("expected success, got status=%d err=%+v", status, reqErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 1 {
		t.Fatalf("expected still 1 notification, got %d", len(notifications))
	}
}

func TestContainerPullOpResolvesImageAndPulls(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	path := fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	response, status, requestErr := runner.dispatch(
		context.Background(), "container.pull", opParams{target: "web"}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !response.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
	}
	got := strings.Join(recordedRuntimeCalls(t, calls), "|")
	// The container's own reference is used as-is: the runtime resolves a short
	// name through the host's registry configuration, and so must a pull.
	if got != "pull|docker.io/library/nginx:1.25" {
		t.Fatalf("recorded calls = %q, want the container's own image reference", got)
	}
}

func TestContainerUpdateOpRecreatesComposeContainer(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	workingDir := t.TempDir()
	path := fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(workingDir)))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	response, status, requestErr := runner.dispatch(
		context.Background(), "container.update", opParams{target: "web"}, "test", "tester",
	)
	if requestErr != nil || status != http.StatusOK || !response.OK {
		t.Fatalf("status=%d err=%+v resp=%+v", status, requestErr, response)
	}
	file := filepath.Join(workingDir, "compose.yml")
	got := strings.Join(recordedRuntimeCalls(t, calls), "|")
	want := strings.Join([]string{
		"cwd=" + workingDir,
		strings.Join([]string{"compose", "-p", "app", "-f", file, "pull", "web"}, "|"),
		"cwd=" + workingDir,
		strings.Join([]string{"compose", "-p", "app", "-f", file, "up", "-d", "--force-recreate", "web"}, "|"),
	}, "|")
	if got != want {
		t.Fatalf("recorded calls =\n%s\nwant\n%s", got, want)
	}
	// Both stages report their output: the pull and the recreate each ran.
	if strings.Count(response.Stdout, "Composed") != 2 {
		t.Fatalf("update output = %q, want both stages", response.Stdout)
	}
}

func TestContainerUpdateOpRefusesUnmanagedContainer(t *testing.T) {
	path := fakeRuntimeBinary(t, fakeRuntimeScript(`{}`))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	_, _, requestErr := runner.dispatch(
		context.Background(), "container.update", opParams{target: "web"}, "test", "tester",
	)
	if requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("unmanaged update = %+v, want 400", requestErr)
	}
	if !strings.Contains(requestErr.message, "not compose-managed") {
		t.Fatalf("message = %q", requestErr.message)
	}
}

func TestContainerUpdateOpRefusesWithoutWorkingDirectory(t *testing.T) {
	labels := `{"com.docker.compose.project":"app","com.docker.compose.service":"web"}`
	path := fakeRuntimeBinary(t, fakeRuntimeScript(labels))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	_, _, requestErr := runner.dispatch(
		context.Background(), "container.update", opParams{target: "web"}, "test", "tester",
	)
	if requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("update without a working directory = %+v, want 400", requestErr)
	}
	if !strings.Contains(requestErr.message, "working directory") {
		t.Fatalf("message = %q", requestErr.message)
	}
}

func TestContainerPullOpReportsUnresolvableContainer(t *testing.T) {
	// A runtime that refuses every inspect: the operation must fail instead of
	// pulling an image the daemon could not identify.
	path := fakeRuntimeBinary(t, "#!/bin/sh\necho 'Error: no such container' >&2\nexit 125\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	_, _, requestErr := runner.dispatch(
		context.Background(), "container.pull", opParams{target: "web"}, "test", "tester",
	)
	if requestErr == nil || requestErr.status != http.StatusBadGateway {
		t.Fatalf("unresolvable pull = %+v, want 502", requestErr)
	}
	if !strings.Contains(requestErr.message, "no such container") {
		t.Fatalf("message = %q, want the runtime's own words", requestErr.message)
	}
}

func TestContainerPullOpRejectsInvalidReference(t *testing.T) {
	path := fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))
	runner := newTestOpsRunner(t, map[string]string{"podman": path})

	for _, target := range []string{"-rf", "bad id", ""} {
		_, _, requestErr := runner.dispatch(
			context.Background(), "container.pull", opParams{target: target}, "test", "tester",
		)
		if requestErr == nil || requestErr.status != http.StatusBadRequest {
			t.Fatalf("target %q = %+v, want 400", target, requestErr)
		}
	}
}
