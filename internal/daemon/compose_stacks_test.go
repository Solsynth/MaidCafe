package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// writeComposeFile writes one compose declaration into a test tree.
func writeComposeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestComposeScanFindsProjectsFromTheirOwnFiles asserts what a scan reads out
// of a directory tree: which projects exist, where they live, which files make
// them up (in the order compose merges them) and which services they declare.
// The project name comes from the file when it declares one and from the
// directory otherwise — the same fallback compose applies.
func TestComposeScanFindsProjectsFromTheirOwnFiles(t *testing.T) {
	root := t.TempDir()
	writeComposeFile(t, filepath.Join(root, "stacks", "web", "compose.yaml"),
		"name: storefront\nservices:\n  web:\n    image: nginx:1.25\n")
	writeComposeFile(t, filepath.Join(root, "stacks", "web", "compose.override.yaml"),
		"services:\n  worker:\n    image: busybox\n")
	writeComposeFile(t, filepath.Join(root, "stacks", "blog", "stack.yml"),
		"services:\n  db:\n    image: postgres:16\n")
	// A YAML file that is not a compose declaration, and a project deeper than
	// the scan is allowed to look.
	writeComposeFile(t, filepath.Join(root, "stacks", "web", "notes.yaml"), "hello: world\n")
	writeComposeFile(t, filepath.Join(root, "stacks", "deep", "a", "b", "compose.yaml"),
		"services:\n  buried:\n    image: busybox\n")

	stacks := scanComposeStacks(
		context.Background(),
		[]string{root},
		composeScanLimits{depth: 3, maxFiles: 100},
	)

	var names []string
	byProject := map[string]composeStack{}
	for _, stack := range stacks {
		names = append(names, stack.Project)
		byProject[stack.Project] = stack
	}
	if strings.Join(names, ",") != "blog,storefront" {
		t.Fatalf("projects = %v, want blog and storefront", names)
	}

	storefront := byProject["storefront"]
	if storefront.Directory != filepath.Join(root, "stacks", "web") {
		t.Fatalf("storefront directory = %q", storefront.Directory)
	}
	// The base file precedes the override: passing them in this order reproduces
	// what compose loads by itself in that directory.
	wantFiles := []string{
		filepath.Join(root, "stacks", "web", "compose.yaml"),
		filepath.Join(root, "stacks", "web", "compose.override.yaml"),
	}
	if !equalStrings(storefront.Files, wantFiles) {
		t.Fatalf("storefront files = %v, want %v", storefront.Files, wantFiles)
	}
	if !equalStrings(storefront.Services, []string{"web", "worker"}) {
		t.Fatalf("storefront services = %v, want web and worker", storefront.Services)
	}

	blog := byProject["blog"]
	if blog.Directory != filepath.Join(root, "stacks", "blog") {
		t.Fatalf("blog directory = %q", blog.Directory)
	}
	if !equalStrings(blog.Services, []string{"db"}) {
		t.Fatalf("blog services = %v, want db", blog.Services)
	}
}

// TestComposeScanStopsAtItsBounds pins the two caps a scan runs under: a tree
// that is deeper than the configured depth is not walked, and the file budget
// stops the walk rather than reading a whole filesystem.
func TestComposeScanStopsAtItsBounds(t *testing.T) {
	root := t.TempDir()
	writeComposeFile(t, filepath.Join(root, "one", "compose.yaml"), "services:\n  a:\n    image: busybox\n")
	writeComposeFile(t, filepath.Join(root, "two", "three", "four", "compose.yaml"), "services:\n  b:\n    image: busybox\n")

	shallow := scanComposeStacks(context.Background(), []string{root}, composeScanLimits{depth: 2, maxFiles: 100})
	if len(shallow) != 1 || shallow[0].Project != "one" {
		t.Fatalf("depth 2 scan = %+v, want only the project at the root's depth", shallow)
	}

	deep := scanComposeStacks(context.Background(), []string{root}, composeScanLimits{depth: 4, maxFiles: 100})
	if len(deep) != 2 {
		t.Fatalf("depth 4 scan found %d projects, want 2", len(deep))
	}

	// One file is one read: a budget of one cannot reach the second project,
	// whatever order the walk visits them in.
	capped := scanComposeStacks(context.Background(), []string{root}, composeScanLimits{depth: 4, maxFiles: 1})
	if len(capped) > 1 {
		t.Fatalf("a one-file budget read %d projects", len(capped))
	}
}

// TestComposeStackStorePersistsAndPrunes asserts the registry's two
// responsibilities: what a scan assigns survives a restart, and a stack whose
// directory has gone is dropped by the next scan while one outside the scan's
// reach is left alone.
func TestComposeStackStorePersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose-stacks.json")
	root := t.TempDir()
	kept := filepath.Join(root, "kept")
	gone := filepath.Join(root, "gone")
	// `elsewhere` is managed but outside any scan this test runs, and its
	// directory stays on disk: scanning one starting point says nothing about
	// the others.
	elsewhere := filepath.Join(root, "elsewhere")
	writeComposeFile(t, filepath.Join(kept, "compose.yaml"), "services:\n  web:\n    image: nginx\n")
	writeComposeFile(t, filepath.Join(gone, "compose.yaml"), "services:\n  old:\n    image: nginx\n")
	writeComposeFile(t, filepath.Join(elsewhere, "compose.yaml"), "services:\n  far:\n    image: nginx\n")

	store := newComposeStackStore(path, newTestLogger())
	added, updated, removed := store.Apply([]composeStack{
		{Project: "kept", Directory: kept, Files: []string{filepath.Join(kept, "compose.yaml")}, Services: []string{"web"}},
		{Project: "gone", Directory: gone, Files: []string{filepath.Join(gone, "compose.yaml")}, Services: []string{"old"}},
		{Project: "elsewhere", Directory: elsewhere, Files: []string{filepath.Join(elsewhere, "compose.yaml")}, Services: []string{"far"}},
	})
	if len(added) != 3 || len(updated) != 0 || len(removed) != 0 {
		t.Fatalf("first apply = %d added, %d updated, %d removed", len(added), len(updated), len(removed))
	}

	reloaded := newComposeStackStore(path, newTestLogger())
	if stack, ok := reloaded.Get("kept"); !ok || stack.Directory != kept {
		t.Fatalf("registry did not survive a reload: %+v", stack)
	}
	// Lookup is case-insensitive: compose treats project names that way, and two
	// answers for one project would be worse than none.
	if _, ok := reloaded.Get("KEPT"); !ok {
		t.Fatal("project lookup should ignore case")
	}

	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	_, updated, removed = reloaded.Apply([]composeStack{
		{Project: "kept", Directory: kept, Files: []string{filepath.Join(kept, "compose.yaml")}, Services: []string{"web"}},
	})
	if len(updated) != 0 {
		t.Fatalf("a rescan of an unchanged stack reported %d updates", len(updated))
	}
	if len(removed) != 1 || removed[0].Project != "gone" {
		t.Fatalf("removed = %+v, want the stack whose directory is gone", removed)
	}
	// `elsewhere` was not in this scan and still exists, so it stays assigned.
	if _, ok := reloaded.Get("elsewhere"); !ok {
		t.Fatal("a stack outside the scan's roots must stay managed")
	}

	if stack, ok := reloaded.Remove("kept"); !ok || stack.Project != "kept" {
		t.Fatalf("remove = %+v, %v", stack, ok)
	}
	if _, ok := newComposeStackStore(path, newTestLogger()).Get("kept"); ok {
		t.Fatal("a removed stack came back after a reload")
	}
}

// TestComposeStackStoreStartsEmptyOnACorruptFile pins the recovery rule: the
// registry mirrors what is on disk and a scan rebuilds it, so an unreadable
// file is reported and skipped rather than repaired in place.
func TestComposeStackStoreStartsEmptyOnACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose-stacks.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newComposeStackStore(path, newTestLogger())
	if stacks := store.List(); len(stacks) != 0 {
		t.Fatalf("corrupt registry produced %d stacks", len(stacks))
	}
	// The next scan can still write: the registry is not wedged by the bad file.
	if added, _, _ := store.Apply([]composeStack{{Project: "web", Directory: "/srv/web"}}); len(added) != 1 {
		t.Fatalf("added = %+v", added)
	}
}

// TestComposeLabelFilesResolveAgainstTheProjectDirectory is the podman-compose
// case: the working directory is absolute while the config files are recorded
// the way the operator typed them. The files stay usable — resolved against the
// directory, in the order compose merges them — instead of failing the update.
func TestComposeLabelFilesResolveAgainstTheProjectDirectory(t *testing.T) {
	target, err := composeUpdateTargetFromLabels(map[string]string{
		"com.docker.compose.project":              "myapp",
		"com.docker.compose.service":              "web",
		"com.docker.compose.project.working_dir":  "/srv/myapp",
		"com.docker.compose.project.config_files": "compose.yml,compose.override.yml",
		"io.podman.compose.project":               "myapp",
	})
	if err != nil {
		t.Fatalf("labels refused: %v", err)
	}
	if target.Directory != "/srv/myapp" {
		t.Fatalf("directory = %q", target.Directory)
	}
	if !equalStrings(target.Files, []string{"compose.yml", "compose.override.yml"}) {
		t.Fatalf("files = %v, want them as recorded", target.Files)
	}
	resolved := make([]string, 0, len(target.Files))
	for _, file := range target.Files {
		path, ok := resolveComposeFile(target.Directory, file)
		if !ok {
			t.Fatalf("file %q did not resolve", file)
		}
		resolved = append(resolved, path)
	}
	if !equalStrings(resolved, []string{"/srv/myapp/compose.yml", "/srv/myapp/compose.override.yml"}) {
		t.Fatalf("resolved files = %v", resolved)
	}
}

// TestComposeLabelPathsStayInsideTheProject pins the guard that replaced the
// absolute-path requirement: a recorded path may be relative, but it may not
// walk out of the directory the labels named, and a directory this package
// cannot use leaves the project to discovery rather than failing the update.
func TestComposeLabelPathsStayInsideTheProject(t *testing.T) {
	target, err := composeUpdateTargetFromLabels(map[string]string{
		"com.docker.compose.project":              "myapp",
		"com.docker.compose.service":              "web",
		"com.docker.compose.project.working_dir":  "relative/dir",
		"com.docker.compose.project.config_files": "../../etc/evil.yml",
	})
	if err != nil {
		t.Fatalf("labels refused: %v", err)
	}
	if target.Directory != "" {
		t.Fatalf("a relative working directory must not be used: %q", target.Directory)
	}
	if len(target.Files) != 0 {
		t.Fatalf("a parent-escaping file must be dropped: %v", target.Files)
	}

	if _, err := composeUpdateTargetFromLabels(map[string]string{
		"com.docker.compose.service": "web",
		"com.docker.compose.project": "myapp",
	}); err != nil {
		t.Fatalf("project and service are enough for the target: %v", err)
	}
	if _, err := composeUpdateTargetFromLabels(map[string]string{
		"com.docker.compose.project": "myapp",
	}); err == nil {
		t.Fatal("a container without a service label is not compose-managed")
	}
}

// TestComposeAttemptsCarryNoAnsiFlag is the guard for a pull that never
// started: the runtime's `compose` subcommand is a dispatcher, so on a podman
// host it hands the arguments to whichever provider is installed —
// `podman-compose` here. `--ansi never` is the *plugin's* spelling; the
// provider only knows `--no-ansi`, and its argument parser ends the command
// with a usage error before anything runs:
//
//	podman-compose: error: argument command: invalid choice: 'never'
//	Error: executing /usr/local/bin/podman-compose --ansi never -p drasl …
//
// Nothing needs the flag: the daemon never gives compose a terminal, and
// compose's own `ansi: auto` disables colors when it is writing to a pipe.
func TestComposeAttemptsCarryNoAnsiFlag(t *testing.T) {
	fakeCommand(t, "podman-compose", "#!/bin/sh\nexit 0\n")
	runtime := fakeCommand(t, "podman", "#!/bin/sh\nexit 0\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": runtime})
	target := composeUpdateTarget{Project: "myapp", Directory: "/srv/myapp"}

	attempts := runner.composeAttempts(t.Context(), runtime, target, "pull")
	if len(attempts) == 0 {
		t.Fatal("no compose attempts were built")
	}
	for _, attempt := range attempts {
		for _, arg := range attempt.args {
			if arg == "--ansi" || arg == "--no-ansi" {
				t.Fatalf("attempt %s carries %s: %v", attempt.command, arg, attempt.args)
			}
		}
	}
}

// TestComposeAttemptsPreferPodmanComposeDirectly pins the order on a podman
// host: the standalone tool is the one that runs, and `podman compose` — which
// only forwards to a provider — is the fallback, not the first attempt. Going
// through the wrapper is what let its argument handling reject a stack update
// before podman-compose ever saw it.
func TestComposeAttemptsPreferPodmanComposeDirectly(t *testing.T) {
	standalone := fakeCommand(t, "podman-compose", "#!/bin/sh\nexit 0\n")
	runtime := fakeCommand(t, "podman", "#!/bin/sh\nexit 0\n")
	runner := newTestOpsRunner(t, map[string]string{"podman": runtime})
	target := composeUpdateTarget{
		Project: "myapp", Directory: "/srv/myapp", Files: []string{"compose.yml"},
	}

	attempts := runner.composeAttempts(t.Context(), runtime, target, "up", "-d", "--force-recreate", "web")
	// The `sudo -n` variants are added when the host has sudo, which a test
	// machine usually does; the relation under test is which tool runs first.
	direct := make([]opAttempt, 0, 2)
	for _, attempt := range attempts {
		if attempt.command == runtime || attempt.command == standalone {
			direct = append(direct, attempt)
		}
	}
	if len(direct) != 2 {
		t.Fatalf("direct attempts = %+v, want the standalone tool and the runtime", direct)
	}
	first := direct[0]
	if first.command != standalone || !equalStrings(first.args, []string{
		"-p", "myapp", "-f", "/srv/myapp/compose.yml", "up", "-d", "--force-recreate", "web",
	}) {
		t.Fatalf("standalone attempt = %s %v", first.command, first.args)
	}
	second := direct[1]
	if second.command != runtime || !equalStrings(second.args, []string{
		"compose", "-p", "myapp", "-f", "/srv/myapp/compose.yml",
		"up", "-d", "--force-recreate", "web",
	}) {
		t.Fatalf("runtime attempt = %s %v", second.command, second.args)
	}
	if second.cwd != "/srv/myapp" {
		t.Fatalf("runtime cwd = %q, want the project directory", second.cwd)
	}
	if first.cwd != "/srv/myapp" {
		t.Fatalf("standalone cwd = %q, want the project directory", first.cwd)
	}
}

// TestComposeAttemptsPreferTheDockerPlugin is the other half of the order rule:
// docker implements compose, so its own subcommand stays the first attempt and
// the standalone binary is the fallback for a host that has only that.
func TestComposeAttemptsPreferTheDockerPlugin(t *testing.T) {
	standalone := fakeCommand(t, "docker-compose", "#!/bin/sh\nexit 0\n")
	runtime := fakeCommand(t, "docker", "#!/bin/sh\nexit 0\n")
	runner := newTestOpsRunner(t, map[string]string{"docker": runtime})
	target := composeUpdateTarget{Project: "myapp", Directory: "/srv/myapp"}

	attempts := runner.composeAttempts(t.Context(), runtime, target, "pull")
	direct := make([]opAttempt, 0, 2)
	for _, attempt := range attempts {
		if attempt.command == runtime || attempt.command == standalone {
			direct = append(direct, attempt)
		}
	}
	if len(direct) != 2 {
		t.Fatalf("direct attempts = %+v, want the plugin and the standalone tool", direct)
	}
	if direct[0].command != runtime || !equalStrings(direct[0].args, []string{
		"compose", "-p", "myapp", "pull",
	}) {
		t.Fatalf("plugin attempt = %s %v", direct[0].command, direct[0].args)
	}
	if direct[1].command != standalone || !equalStrings(direct[1].args, []string{
		"-p", "myapp", "pull",
	}) {
		t.Fatalf("standalone attempt = %s %v", direct[1].command, direct[1].args)
	}
}

// composeStacksTestConfig is the standard daemon config with a writable
// registry, so tests never touch the shipped path.
func composeStacksTestConfig(t *testing.T) config.DaemonConfig {
	t.Helper()
	cfg := detailTestConfig()
	cfg.Compose = config.ComposeConfig{
		StacksPath: filepath.Join(t.TempDir(), "compose-stacks.json"),
	}
	return cfg
}

// signedPost sends one signed mutating request and returns its status and body.
func signedPost(t *testing.T, base, path, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	request.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", []byte(body)))
	return doRequest(t, request)
}

func doRequest(t *testing.T, request *http.Request) (int, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// TestComposeScanAssignsAProjectAndTheContainerUpdateUsesIt is the feature end
// to end: a container whose own labels name its project but not its directory
// becomes updatable once the operator assigns that project with a scan.
func TestComposeScanAssignsAProjectAndTheContainerUpdateUsesIt(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	// The labels name the project and service, and nothing about the directory:
	// the shape the standalone podman-compose leaves behind on some hosts.
	fakeRuntimeBinary(t, fakeRuntimeScript(`{"com.docker.compose.project":"myapp","com.docker.compose.service":"web"}`))

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

	// The update is refused while no stack is assigned: the daemon will not
	// guess which directory a project lives in.
	status, body := signedPost(t, base, "/api/v1/containers/web/update", `{}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "not a stack this daemon manages") {
		t.Fatalf("unassigned update = %d %s", status, body)
	}

	// A scan without a signature is refused: it decides where compose runs.
	unsigned, err := http.NewRequest(http.MethodPost, base+"/api/v1/compose/stacks/scan",
		strings.NewReader(`{"path":"`+projectDir+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	unsigned.Header.Set("Authorization", "Bearer metrics-secret")
	if status, _ := doRequest(t, unsigned); status != http.StatusUnauthorized {
		t.Fatalf("unsigned scan status = %d, want 401", status)
	}

	status, body = signedPost(t, base, "/api/v1/compose/stacks/scan", `{"path":"`+projectDir+`"}`)
	if status != http.StatusOK {
		t.Fatalf("scan status = %d: %s", status, body)
	}
	var scanned struct {
		Found  int      `json:"found"`
		Added  []string `json:"added"`
		Stacks []struct {
			Project   string   `json:"project"`
			Directory string   `json:"directory"`
			Files     []string `json:"files"`
			Services  []string `json:"services"`
			Running   int      `json:"running"`
			Total     int      `json:"total"`
		} `json:"stacks"`
	}
	if err := json.Unmarshal(body, &scanned); err != nil {
		t.Fatalf("scan body %s: %v", body, err)
	}
	if scanned.Found != 1 || len(scanned.Added) != 1 || scanned.Added[0] != "myapp" {
		t.Fatalf("scan assigned %+v", scanned.Added)
	}
	if len(scanned.Stacks) != 1 || scanned.Stacks[0].Directory != projectDir {
		t.Fatalf("scan stacks = %+v", scanned.Stacks)
	}
	// The stack's health comes from the daemon's own container snapshot: the
	// fake runtime reports one running container carrying that project label.
	if scanned.Stacks[0].Total != 1 || scanned.Stacks[0].Running != 1 {
		t.Fatalf("stack health = %+v", scanned.Stacks[0])
	}

	// Now the same update runs, in the directory the scan recorded. A container
	// update pulls and recreates, so it answers with a task; the test waits for
	// it the way a client does.
	status, body = signedPost(t, base, "/api/v1/containers/web/update", `{}`)
	_ = awaitTask(t, base, taskFrom(t, status, body).ID)
	// The fake runtime records one argv token per line, so the invocation is
	// read as a whole.
	recorded := strings.Join(recordedRuntimeCalls(t, calls), " ")
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if !strings.Contains(recorded, "cwd="+projectDir+" compose -p myapp -f "+composeFile+" up -d --force-recreate web") {
		t.Fatalf("the recreate did not run in the scanned project directory: %s", recorded)
	}
	if !strings.Contains(recorded, "compose -p myapp -f "+composeFile+" pull web") {
		t.Fatalf("the update did not pull first: %s", recorded)
	}

	// Unassigning forgets the stack, and a second removal is a clean 404.
	request, err := http.NewRequest(http.MethodDelete, base+"/api/v1/compose/stacks/myapp", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	if status, body := doRequest(t, request); status != http.StatusOK {
		t.Fatalf("unassign status = %d: %s", status, body)
	}
	request, err = http.NewRequest(http.MethodDelete, base+"/api/v1/compose/stacks/myapp", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	if status, _ := doRequest(t, request); status != http.StatusNotFound {
		t.Fatalf("second unassign status = %d, want 404", status)
	}
}

// TestComposeUpdateActionRunsBothStages asserts the stack-level upgrade: one
// call pulls every service's image and then recreates on it, in the stack's own
// directory, with no directory from the caller.
func TestComposeUpdateActionRunsBothStages(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	fakeRuntimeBinary(t, fakeRuntimeScript(`{"com.docker.compose.project":"myapp"}`))

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

	if status, body := signedPost(t, base, "/api/v1/compose/stacks/scan", `{"path":"`+projectDir+`"}`); status != http.StatusOK {
		t.Fatalf("scan status = %d: %s", status, body)
	}
	status, body := signedPost(t, base, "/api/v1/compose/myapp/update", `{}`)
	_ = awaitTask(t, base, taskFrom(t, status, body).ID)
	recorded := strings.Join(recordedRuntimeCalls(t, calls), " ")
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if !strings.Contains(recorded, "compose -p myapp -f "+composeFile+" pull") {
		t.Fatalf("no pull stage: %s", recorded)
	}
	if !strings.Contains(recorded, "compose -p myapp -f "+composeFile+" up -d --force-recreate") {
		t.Fatalf("no recreate stage: %s", recorded)
	}
	if !strings.Contains(recorded, "cwd="+projectDir+" compose ") {
		t.Fatalf("the upgrade did not run in the stack's directory: %s", recorded)
	}
}

// TestComposeScanRejectsUnusableStartingPoints pins what a request may name:
// one absolute directory, not a list of both, and never a relative path.
func TestComposeScanRejectsUnusableStartingPoints(t *testing.T) {
	app, err := NewApp(composeStacksTestConfig(t), nil)
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
	dir := t.TempDir()

	cases := []struct {
		body   string
		status int
	}{
		{`{"path":"relative/dir"}`, http.StatusBadRequest},
		{`{"path":"` + dir + `","roots":["` + dir + `"]}`, http.StatusBadRequest},
		{`{"path":"/opt/../etc"}`, http.StatusBadRequest},
		{`{"roots":["` + dir + `"]}`, http.StatusOK},
		{`{"path":"` + dir + `"}`, http.StatusOK},
	}
	for _, tc := range cases {
		status, body := signedPost(t, base, "/api/v1/compose/stacks/scan", tc.body)
		if status != tc.status {
			t.Fatalf("scan %s = %d (%s), want %d", tc.body, status, body, tc.status)
		}
	}
}
