package daemon

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/privfs"
)

// privFixture starts a daemon with one privileged root whose writes go to a
// stand-in helper, and returns the app, the base URL, the helper's argv log
// and the root directory.
//
// The stand-in reproduces the real helper's contract (argv, stdin, exit codes)
// but runs unprivileged in a temporary directory, so the routing can be tested
// without a sudoers rule. `app.priv` is replaced with a runner that has no sudo
// prefix for the same reason: the sudo invocation itself is the one part of
// this path that needs a real installation.
func privFixture(t *testing.T, mutate func(*config.FilesConfig)) (*App, string, string, string) {
	t.Helper()
	root := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	helper := filepath.Join(t.TempDir(), "maidkit-priv-stand-in")
	script := `#!/bin/sh
# Stand-in for maidkit-priv: log argv, then apply the operation.
set -eu
verb="$2"
profile="$3"
rel="$4"
mode="${5:-}"
printf '%s\n' "$*" >> ` + argvLog + `
case "$verb" in
  write)  mkdir -p "` + root + `/$(dirname "$rel")"; cat > "` + root + `/$rel"; chmod "$mode" "` + root + `/$rel" ;;
  mkdir)  mkdir -p "` + root + `/$rel"; chmod "$mode" "` + root + `/$rel" ;;
  remove) rm -f "` + root + `/$rel" ;;
  *) echo "maidkit-priv: unknown fs operation" >&2; exit 2 ;;
esac
echo "{\"event\":\"privfs\",\"verb\":\"$verb\",\"profile\":\"$profile\",\"ok\":true}" >&2
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	files := config.FilesConfig{
		Enabled:    true,
		Secret:     "files-secret",
		Roots:      []config.FilesRootConfig{{Path: root, Privileged: true, Profile: "testprof"}},
		AllowWrite: true,
	}
	priv := config.PrivConfig{Helper: helper}
	if mutate != nil {
		mutate(&files)
	}
	cfg := config.DaemonConfig{
		Priv:              priv,
		ID:                "host-priv",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java"},
		ProcessesLimit:    50,
		RequestTimeout:    10 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      65536,
		MaxConcurrentRuns: 1,
		AuditPath:         filepath.Join(t.TempDir(), "audit.jsonl"),
		Files:             files,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.priv = &privRunner{}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app, "http://" + app.ListenAddr(), argvLog, root
}

func recordedArgv(t *testing.T, argvLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestFileAPIPrivilegedWriteRoutesThroughHelper(t *testing.T) {
	app, base, argvLog, root := privFixture(t, nil)

	// A file the daemon account can read but not write stands in for an nginx
	// configuration: the mode is what the helper is asked to preserve.
	target := filepath.Join(root, "app.conf")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"path":` + strconv.Quote(target) + `,"content":"` +
		base64.StdEncoding.EncodeToString([]byte("new contents")) + `"}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, nil)
	if status != http.StatusOK {
		t.Fatalf("privileged write status = %d (%v)", status, response)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "new contents" {
		t.Fatalf("content = %q (%v)", content, err)
	}
	// The helper is named by profile and relative path, never by directory.
	argv := recordedArgv(t, argvLog)
	if len(argv) != 1 {
		t.Fatalf("helper invocations = %v", argv)
	}
	if argv[0] != "fs write testprof app.conf 0640" {
		t.Fatalf("helper argv = %q", argv[0])
	}
	// An existing file's mode is preserved, so a refused mode is a refusal
	// rather than a rewrite to 0644.
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v (%v)", info.Mode().Perm(), err)
	}

	// The audit entry says the operation needed root, and names the path.
	entries := app.audit.Recent(5)
	if len(entries) == 0 || !entries[0].Privileged || entries[0].Name != fileActionWrite {
		t.Fatalf("audit entry = %+v", entries[0])
	}
	if entries[0].Target != target {
		t.Fatalf("audit target = %q", entries[0].Target)
	}
}

// TestFileAPIPrivilegedRawContentRoute pins the raw PUT route to the same
// write path as the JSON one. It runs through a different handler than
// /write does, and an earlier version of this code sent its bytes straight to
// the unprivileged writer — which failed with an internal error instead of
// elevating, and would have silently written as the daemon account had the
// guard been absent.
func TestFileAPIPrivilegedRawContentRoute(t *testing.T) {
	app, base, argvLog, root := privFixture(t, nil)

	target := filepath.Join(root, "site.conf")
	request, err := http.NewRequest(http.MethodPut,
		base+"/api/v1/files/content?path="+urlQuery(target), strings.NewReader("up=true"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer files-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("raw privileged write status = %d (%s)", response.StatusCode, body)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "up=true" {
		t.Fatalf("content = %q (%v)", content, err)
	}
	argv := recordedArgv(t, argvLog)
	if len(argv) != 1 || argv[0] != "fs write testprof site.conf 0644" {
		t.Fatalf("helper argv = %v", argv)
	}
	entries := app.audit.Recent(5)
	if len(entries) == 0 || !entries[0].Privileged || entries[0].Name != fileActionWrite {
		t.Fatalf("audit entry = %+v", entries[0])
	}
}

func TestFileAPIPrivilegedMkdirAndRemove(t *testing.T) {
	_, base, argvLog, root := privFixture(t, nil)

	mkdirBody := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "snippets")) + `}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/mkdir", mkdirBody, nil)
	if status != http.StatusOK {
		t.Fatalf("privileged mkdir status = %d (%v)", status, response)
	}
	if info, err := os.Stat(filepath.Join(root, "snippets")); err != nil || !info.IsDir() {
		t.Fatalf("mkdir result: %v", err)
	}

	target := filepath.Join(root, "snippets", "old.conf")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleteBody := []byte(`{"path":` + strconv.Quote(target) + `}`)
	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete", deleteBody, nil)
	if status != http.StatusOK {
		t.Fatalf("privileged delete status = %d (%v)", status, response)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("privileged delete left the file")
	}

	argv := recordedArgv(t, argvLog)
	if len(argv) != 2 {
		t.Fatalf("helper invocations = %v", argv)
	}
	if argv[0] != "fs mkdir testprof snippets 0755" {
		t.Fatalf("mkdir argv = %q", argv[0])
	}
	if argv[1] != "fs remove testprof snippets/old.conf" {
		t.Fatalf("remove argv = %q", argv[1])
	}
}

func TestFileAPIPrivilegedReadsStayUnprivileged(t *testing.T) {
	_, base, argvLog, root := privFixture(t, nil)
	if err := os.WriteFile(filepath.Join(root, "readable.conf"), []byte("visible"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, response := fileJSON(t, http.MethodGet,
		base+"/api/v1/files/read?path="+urlQuery(filepath.Join(root, "readable.conf")), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read status = %d (%v)", status, response)
	}
	content, err := base64.StdEncoding.DecodeString(response["content"].(string))
	if err != nil || string(content) != "visible" {
		t.Fatalf("content = %q (%v)", content, err)
	}
	// Reading needs no root, so the helper must not have been involved.
	if argv := recordedArgv(t, argvLog); len(argv) != 0 {
		t.Fatalf("a read invoked the helper: %v", argv)
	}
}

func TestFileAPIPrivilegedRootAdvertisesItself(t *testing.T) {
	_, base, _, root := privFixture(t, nil)
	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("roots status = %d", status)
	}
	entry := response["roots"].([]any)[0].(map[string]any)
	if entry["privileged"] != true || entry["profile"] != "testprof" || entry["path"] != root {
		t.Fatalf("root info = %v", entry)
	}
}

func TestFileAPIPrivilegedUnsupportedOperationsRefused(t *testing.T) {
	_, base, argvLog, root := privFixture(t, nil)
	if err := os.WriteFile(filepath.Join(root, "a.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// move and copy need root inside a privileged root, and the helper does
	// not implement them. Reporting that is better than a partial copy or a
	// permission error that looks like a bug.
	moveBody := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "a.conf")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "b.conf")) + `}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/move", moveBody, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("privileged move status = %d (%v)", status, response)
	}
	copyBody := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "a.conf")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "c.conf")) + `}`)
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/copy", copyBody, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("privileged copy status = %d", status)
	}
	// A recursive delete is not something a profile authorizes. The root
	// itself is refused earlier, by the rule that no configured root is
	// modifiable; a directory inside it reaches the recursive refusal.
	if err := os.Mkdir(filepath.Join(root, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete",
		[]byte(`{"path":`+strconv.Quote(filepath.Join(root, "tree"))+`,"recursive":true}`), nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("privileged recursive delete status = %d", status)
	}
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete",
		[]byte(`{"path":`+strconv.Quote(root)+`,"recursive":true}`), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("privileged root delete status = %d", status)
	}
	if argv := recordedArgv(t, argvLog); len(argv) != 0 {
		t.Fatalf("a refused operation invoked the helper: %v", argv)
	}
}

func TestFileAPIPrivilegedWriteHonorsWriteGate(t *testing.T) {
	_, base, argvLog, root := privFixture(t, func(f *config.FilesConfig) { f.AllowWrite = false })
	body := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "blocked.conf")) + `,"content":"eA=="}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only privileged write status = %d (%v)", status, response)
	}
	// The read-only switch is enforced before the helper, so a privileged root
	// cannot be a way around it.
	if argv := recordedArgv(t, argvLog); len(argv) != 0 {
		t.Fatalf("the write gate let the helper run: %v", argv)
	}
	// The root itself stays untouchable.
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/write",
		[]byte(`{"path":`+strconv.Quote(root)+`,"content":"eA=="}`), nil)
	if status != http.StatusForbidden {
		t.Fatalf("root write status = %d", status)
	}
}

// privHelperFailureFixture starts a daemon whose stand-in helper always fails
// with [stderr], so the daemon's error mapping can be checked.
func privHelperFailureFixture(t *testing.T, stderr string, exit int) string {
	t.Helper()
	root := t.TempDir()
	helper := filepath.Join(t.TempDir(), "maidkit-priv-failing")
	script := "#!/bin/sh\necho '" + stderr + "' >&2\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	files := config.FilesConfig{
		Enabled:    true,
		Secret:     "files-secret",
		Roots:      []config.FilesRootConfig{{Path: root, Privileged: true, Profile: "testprof"}},
		AllowWrite: true,
	}
	priv := config.PrivConfig{Helper: helper}
	cfg := config.DaemonConfig{
		ID: "host-priv-fail", Transport: "http", Listen: "127.0.0.1:0",
		MetricsSecret: "metrics-secret", MetricsInterval: time.Hour, StreamInterval: time.Second,
		Runtimes: []string{"java"}, ProcessesLimit: 50, RequestTimeout: 10 * time.Second,
		ScriptTimeout: time.Second, MaxBodyBytes: 65536, MaxConcurrentRuns: 1,
		Files: files, Priv: priv,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.priv = &privRunner{}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return "http://" + app.ListenAddr() + "|" + root
}

func TestFileAPIPrivilegedHelperFailuresAreActionable(t *testing.T) {
	cases := []struct {
		name       string
		helperErr  string
		exit       int
		wantStatus int
		wantText   string
	}{
		{
			// The message operators hit when the sudoers rule is missing.
			name:       "missing sudoers rule",
			helperErr:  "sudo: a password is required",
			exit:       1,
			wantStatus: http.StatusForbidden,
			wantText:   "sudoers",
		},
		{
			name:       "helper not granted root",
			helperErr:  "maidkit-priv: must run as root (install the sudoers rule printed by `maidkit-priv sudoers`)",
			exit:       5,
			wantStatus: http.StatusFailedDependency,
			wantText:   "was not granted root",
		},
		{
			// The helper's own audit line shares stderr; it must not leak into
			// the API response.
			name:       "policy refusal keeps the helper message",
			helperErr:  "maidkit-priv: mode 0700 is not granted by profile \"testprof\"\n{\"event\":\"privfs\",\"ok\":false}",
			exit:       3,
			wantStatus: http.StatusBadGateway,
			wantText:   "not granted by profile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseAndRoot := privHelperFailureFixture(t, tc.helperErr, tc.exit)
			base, root, _ := strings.Cut(baseAndRoot, "|")
			body := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "x.conf")) + `,"content":"eA=="}`)
			status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, nil)
			if status != tc.wantStatus {
				t.Fatalf("status = %d (%v)", status, response)
			}
			message, _ := response["error"].(string)
			if !strings.Contains(message, tc.wantText) {
				t.Fatalf("error = %q, want it to contain %q", message, tc.wantText)
			}
			if strings.Contains(message, "event") {
				t.Fatalf("the helper's audit JSON leaked into the response: %q", message)
			}
		})
	}
}

func TestFileAPIPrivilegedWriteWithoutRunnerRefused(t *testing.T) {
	// A host with neither root nor sudo has no way to elevate; a privileged
	// write must say so rather than fail on a permission error that looks like
	// a bug in the caller.
	app, base, _, root := privFixture(t, nil)
	app.priv = nil
	body := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "x.conf")) + `,"content":"eA=="}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v)", status, response)
	}
}

func TestFileAPIPrivilegedWriteRespectsContentLimit(t *testing.T) {
	_, base, _, root := privFixture(t, nil)
	oversized := base64.StdEncoding.EncodeToString(make([]byte, privfs.MaxContentBytes+1))
	body := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "big.conf")) + `,"content":"` + oversized + `"}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d (%v)", status, response)
	}
}

func TestHelperMessagePrefersTheSentence(t *testing.T) {
	stderr := "{\"event\":\"privfs\",\"ok\":false}\nmaidkit-priv: refusing to remove the directory x\n{\"event\":\"privfs\",\"ok\":false}\n"
	if got := helperMessage(stderr); got != "maidkit-priv: refusing to remove the directory x" {
		t.Fatalf("helperMessage = %q", got)
	}
	if got := helperMessage("{\"event\":\"privfs\"}"); got != "" {
		t.Fatalf("helperMessage = %q", got)
	}
}

// TestNativeSystemdOpRoutesThroughTheHelper pins the systemd path: with the
// helper configured for it, the action must go there and nowhere else. The
// blanket `sudo -n systemctl` attempt must not remain behind it, because a
// helper refusal would then silently fall through to whatever broader sudo
// grant the host happens to have.
func TestNativeSystemdOpRoutesThroughTheHelper(t *testing.T) {
	root := t.TempDir()
	argvLog := filepath.Join(root, "argv.log")
	helper := filepath.Join(root, "maidkit-priv-stand-in")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> ` + argvLog + `
case "$1 $2" in
  "systemd restart") echo "restarted $3"; exit 0 ;;
esac
echo "maidkit-priv: refused" >&2
exit 3
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	newRunner := func(systemd bool) *nativeOpRunner {
		cfg := config.DaemonConfig{
			ID: "host-ops", Transport: "http", Listen: "127.0.0.1:0",
			MetricsSecret: "metrics-secret", MetricsInterval: time.Hour,
			StreamInterval: time.Second, Runtimes: []string{"java"},
			ProcessesLimit: 50, RequestTimeout: 5 * time.Second,
			ScriptTimeout: 5 * time.Second, MaxBodyBytes: 65536, MaxConcurrentRuns: 1,
			AuditPath: filepath.Join(root, "audit.jsonl"),
			Priv:      config.PrivConfig{Helper: helper, Systemd: systemd},
		}
		executor := NewWebhookExecutor(cfg)
		executor.SetAuditLogger(NewAuditLogger(cfg.AuditPath, nil))
		runner := &nativeOpRunner{executor: executor, runtimes: func(context.Context) map[string]string { return nil }}
		runner.SetScriptTimeout(cfg.ScriptTimeout)
		runner.SetPrivilegedPolicy(config.PrivConfig{Helper: helper, Systemd: systemd}, &privRunner{})
		return runner
	}

	// Routed: the helper runs it, and the response carries its stdout.
	routed := newRunner(true)
	response, status, requestErr := routed.dispatch(
		context.Background(), "systemd.restart",
		opParams{target: "nginx.service"}, "http", "tester",
	)
	if requestErr != nil {
		t.Fatalf("routed request error: %v", requestErr.message)
	}
	if status != http.StatusOK || !response.OK {
		t.Fatalf("routed systemd status = %d, response = %+v", status, response)
	}
	if !strings.Contains(response.Stdout, "restarted nginx.service") {
		t.Fatalf("stdout = %q", response.Stdout)
	}
	lines := recordedArgv(t, argvLog)
	if len(lines) != 1 || lines[0] != "systemd restart nginx.service" {
		t.Fatalf("helper argv = %v", lines)
	}

	// Not routed: the helper is untouched, and the host's own systemctl is
	// tried instead — the behavior that predates the helper.
	if err := os.Remove(argvLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	direct := newRunner(false)
	_, _, requestErr = direct.dispatch(
		context.Background(), "systemd.restart",
		opParams{target: "nginx.service"}, "http", "tester",
	)
	if requestErr != nil {
		t.Fatalf("direct request error: %v", requestErr.message)
	}
	if lines := recordedArgv(t, argvLog); len(lines) != 0 {
		t.Fatalf("an unrouted action invoked the helper: %v", lines)
	}
}

// TestNativeSystemdOpRejectsAnOptionShapedUnit guards the unit pattern: a
// leading dash would let a unit name reach systemctl's option parser.
func TestNativeSystemdOpRejectsAnOptionShapedUnit(t *testing.T) {
	runner := &nativeOpRunner{
		executor: NewWebhookExecutor(config.DaemonConfig{
			ScriptTimeout: time.Second, MaxBodyBytes: 1024, MaxConcurrentRuns: 1,
		}),
		runtimes: func(context.Context) map[string]string { return nil },
	}
	runner.SetScriptTimeout(time.Second)
	for _, unit := range []string{"-H.service", "--now.service", "--.service"} {
		_, _, requestErr := runner.dispatch(
			context.Background(), "systemd.restart", opParams{target: unit}, "http", "tester",
		)
		if requestErr == nil {
			t.Fatalf("unit %q was accepted", unit)
		}
		if requestErr.status != http.StatusBadRequest {
			t.Fatalf("unit %q status = %d", unit, requestErr.status)
		}
	}
}
