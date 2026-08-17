package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func executable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestWebhookExecutesOpaqueBodyAndRejectsBadSecret(t *testing.T) {
	output := filepath.Join(t.TempDir(), "body")
	script := executable(t, "#!/bin/sh\ncat > "+output+"\nprintf '%s' safe\n")
	cfg := config.DaemonConfig{ScriptTimeout: time.Second, MaxBodyBytes: 1024, MaxConcurrentRuns: 1, Webhooks: []config.WebhookConfig{{Name: "hook", Secret: "secret", Command: script, Enabled: true}}}
	executor := NewWebhookExecutor(cfg)
	server := httptest.NewServer(executor)
	defer server.Close()
	body := `{"x":"$(touch ` + filepath.Join(t.TempDir(), "sentinel") + `)"}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/hook", strings.NewReader(body))
	req.Header.Set("X-MaidCafe-Signature", signedHeader("secret", []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("stdin mismatch: %q", got)
	}
	bad, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/hook", strings.NewReader(body))
	bad.Header.Set("X-MaidCafe-Signature", signedHeader("wrong", []byte(body)))
	resp, err = http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature status %d", resp.StatusCode)
	}
}
func TestWebhookFailureTimeoutAndBodyLimit(t *testing.T) {
	sleep := executable(t, "#!/bin/sh\nsleep 1\n")
	cfg := config.DaemonConfig{ScriptTimeout: 30 * time.Millisecond, MaxBodyBytes: 4, MaxConcurrentRuns: 1, Webhooks: []config.WebhookConfig{{Name: "slow", Secret: "s", Command: sleep, Enabled: true}}}
	executor := NewWebhookExecutor(cfg)
	server := httptest.NewServer(executor)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/slow", strings.NewReader("x"))
	req.Header.Set("X-MaidCafe-Signature", signedHeader("s", []byte("x")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("timeout status %d", resp.StatusCode)
	}
	large, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/slow", strings.NewReader("large"))
	large.Header.Set("X-MaidCafe-Signature", signedHeader("s", []byte("large")))
	resp, err = http.DefaultClient.Do(large)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body status %d", resp.StatusCode)
	}
	var payload executionResponse
	_ = json.NewDecoder(resp.Body).Decode(&payload)
}
func TestHealthDoesNotRevealConfiguration(t *testing.T) {
	cfg := config.DaemonConfig{
		ID:                "host-1",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		ScriptTimeout:     time.Second,
		RequestTimeout:    time.Second,
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		MaxBodyBytes:      100,
		MaxConcurrentRuns: 1,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	baseURL := "http://" + app.ListenAddr()
	unauthorized, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized health status = %d", unauthorized.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer metrics-secret")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "command") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("health leaked config: %s", encoded)
	}
}

func TestSubstituteScriptTemplate(t *testing.T) {
	script := executable(t, "#!/bin/sh\necho '{{ SERVICE_NAME }} → {{ serviceName }}'\n")
	substituted, err := substituteScriptTemplate(script, []byte(`{"SERVICE_NAME":"nginx","serviceName":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(substituted), "echo 'nginx → web'") {
		t.Fatalf("unexpected substitution: %q", substituted)
	}

	// Values are inserted verbatim: the caller is trusted, no escaping.
	script = executable(t, "#!/bin/sh\ncat {{ PATH }}\n")
	substituted, err = substituteScriptTemplate(script, []byte(`{"PATH":"/etc/passwd; id"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(substituted) != "#!/bin/sh\ncat /etc/passwd; id\n" {
		t.Fatalf("verbatim substitution expected, got %q", substituted)
	}

	// Numbers and JSON null render sensibly.
	script = executable(t, "#!/bin/sh\necho {{ N }} {{ NIL }}\n")
	substituted, err = substituteScriptTemplate(script, []byte(`{"N":42,"NIL":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(substituted) != "#!/bin/sh\necho 42 \n" {
		t.Fatalf("scalar substitution expected, got %q", substituted)
	}
}

func TestSubstituteScriptTemplateRequiresValues(t *testing.T) {
	script := executable(t, "#!/bin/sh\necho {{ SERVICE_NAME }}\n")
	_, err := substituteScriptTemplate(script, []byte(`{"OTHER":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "SERVICE_NAME") {
		t.Fatalf("missing variable should fail with its name, got %v", err)
	}

	// A non-JSON body carries no values; the script must not reference any.
	_, err = substituteScriptTemplate(script, []byte("not json"))
	if err == nil || !strings.Contains(err.Error(), "SERVICE_NAME") {
		t.Fatalf("non-JSON body should surface missing variables, got %v", err)
	}
	plain := executable(t, "#!/bin/sh\ncat\n")
	substituted, err := substituteScriptTemplate(plain, []byte("not json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(substituted) != "#!/bin/sh\ncat\n" {
		t.Fatalf("template-free script must pass through, got %q", substituted)
	}
}

func TestBuildRunCommandAppliesCwdAndEnv(t *testing.T) {
	hook := config.WebhookConfig{Command: "/bin/echo", Cwd: "/srv/app", Env: []string{"FOO=bar", "EMPTY="}}
	cmd := buildRunCommand(context.Background(), hook, hook.Command, []string{"-n", "hi"})
	if cmd.Dir != "/srv/app" {
		t.Fatalf("cwd = %q", cmd.Dir)
	}
	if cmd.Args[0] != "/bin/echo" || len(cmd.Args) != 3 {
		t.Fatalf("args = %v", cmd.Args)
	}
	env := map[string]string{}
	for _, kv := range cmd.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	if env["FOO"] != "bar" || env["EMPTY"] != "" {
		t.Fatalf("env = %v", env)
	}
	if cmd.Env == nil {
		t.Fatal("direct mode must inherit the daemon environment")
	}

	// Default working directory stays the daemon's own.
	plain := buildRunCommand(context.Background(), config.WebhookConfig{Command: "/bin/true"}, "/bin/true", nil)
	if plain.Dir != "/" {
		t.Fatalf("default cwd = %q", plain.Dir)
	}
}

func TestBuildRunCommandDelegatesUserRunsToSudo(t *testing.T) {
	hook := config.WebhookConfig{
		Command: "/etc/maidcafe/actions/deploy.sh",
		User:    "deploy",
		Cwd:     "/srv/myapp",
		Env:     []string{"CI_BUILD=42", "SPACED=two words"},
	}
	cmd := buildRunCommand(context.Background(), hook, hook.Command, []string{"--force"})
	// The command stays the absolute script path (never a shell wrapper) so
	// the sudoers rule `/etc/maidcafe/actions/run/*` matches it. No -D:
	// default sudoers rules reject it. A non-script command inherits the
	// daemon-set cwd through sudo instead.
	want := []string{
		"sudo", "-H", "-u", "deploy",
		"CI_BUILD=42", "SPACED=two words",
		"/etc/maidcafe/actions/deploy.sh", "--force",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv = %v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
	if cmd.Dir != "/srv/myapp" {
		t.Fatalf("cwd = %q", cmd.Dir)
	}

	// Script actions apply cwd through their injected cd line, so the sudo
	// process keeps the daemon's own cwd (the daemon user may not be able to
	// traverse the target directory).
	scriptHook := hook
	scriptHook.Script = true
	scriptCmd := buildRunCommand(context.Background(), scriptHook, scriptHook.Command, nil)
	if scriptCmd.Dir != "" {
		t.Fatalf("script action cwd = %q, want unset", scriptCmd.Dir)
	}
	for _, arg := range scriptCmd.Args {
		if arg == "-D" {
			t.Fatalf("script action must not pass -D: %v", scriptCmd.Args)
		}
	}

	// Without a cwd no directory is forced and the command still comes last.
	noCwd := buildRunCommand(context.Background(), config.WebhookConfig{
		Command: "/bin/true", User: "deploy",
	}, "/bin/true", nil)
	if len(noCwd.Args) != 5 || noCwd.Args[0] != "sudo" || noCwd.Args[3] != "deploy" || noCwd.Args[4] != "/bin/true" {
		t.Fatalf("no-cwd argv = %v", noCwd.Args)
	}
}

func TestExecuteHonorsCwdAndEnv(t *testing.T) {
	output := filepath.Join(t.TempDir(), "result")
	script := executable(t, "#!/bin/sh\nprintf '%s' \"$(pwd)\" > "+output+"\nprintf '%s' \"$GREETING\" >> "+output+"\n")
	cfg := config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:    "where",
			Command: script,
			Enabled: true,
			Cwd:     t.TempDir(),
			Env:     []string{"GREETING=hello"},
		}},
	}
	executor := NewWebhookExecutor(cfg)
	result, requestErr := executor.RunAction(context.Background(), "where", nil, "test", "test")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves /var → /private/var in pwd; compare physical paths.
	realCwd, err := filepath.EvalSymlinks(cfg.Actions[0].Cwd)
	if err != nil {
		t.Fatal(err)
	}
	want := realCwd + "hello"
	if string(got) != want {
		t.Fatalf("cwd/env result = %q, want %q", got, want)
	}
}

func TestRenderScriptTempLocations(t *testing.T) {
	deployed := filepath.Join(t.TempDir(), "actions", "deploy.sh")
	// Without a run-as user the script lands in the system temp dir, 0700.
	plain, cleanup, err := renderScriptTemp(config.WebhookConfig{Command: deployed}, []byte("#!/bin/sh\necho hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	plainDir, err := filepath.EvalSymlinks(filepath.Dir(plain))
	if err != nil {
		t.Fatal(err)
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plainDir != tempDir {
		t.Fatalf("plain temp = %q (dir %q)", plain, plainDir)
	}
	info, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("plain mode = %v", info.Mode().Perm())
	}

	// With a run-as user the script must be readable by the target account,
	// so it renders next to the deployed script with 0755.
	userRun, cleanup, err := renderScriptTemp(config.WebhookConfig{
		Command: deployed, User: "deploy",
	}, []byte("#!/bin/sh\necho hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if filepath.Dir(userRun) != filepath.Join(filepath.Dir(deployed), "run") {
		t.Fatalf("user temp = %q", userRun)
	}
	info, err = os.Stat(userRun)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("user mode = %v", info.Mode().Perm())
	}
}

func TestExecuteUserActionThroughSudo(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to switch users through sudo")
	}
	// The whole chain must be traversable by the target user: the rendered
	// script lives in <script dir>/run (0755) and the cwd needs world
	// traverse, since the injected `cd` runs as the target user.
	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(workDir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "who")
	script := executable(t, "#!/bin/sh\nid -un > "+output+"\npwd >> "+output+"\n")
	cfg := config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:    "as-user",
			Command: script,
			Script:  true,
			Enabled: true,
			User:    "nobody",
			Cwd:     cwd,
		}},
	}
	executor := NewWebhookExecutor(cfg)
	result, requestErr := executor.RunAction(context.Background(), "as-user", nil, "test", "test")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !result.OK {
		t.Fatalf("result = %+v (stderr %q)", result, result.Stderr)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || lines[0] != "nobody" {
		t.Fatalf("ran as %q, want nobody", strings.TrimSpace(string(got)))
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if lines[1] != realCwd {
		t.Fatalf("pwd = %q, want %q", lines[1], realCwd)
	}
}

func TestPrependWorkingDirectory(t *testing.T) {
	// Inserted after the shebang, which must stay on line one.
	got := prependWorkingDirectory("/srv/myapp", []byte("#!/bin/sh\necho hi\n"))
	want := "#!/bin/sh\ncd -- '/srv/myapp'\necho hi\n"
	if string(got) != want {
		t.Fatalf("prepend = %q, want %q", got, want)
	}

	// Single quotes in the cwd are escaped so the line stays one argument.
	got = prependWorkingDirectory("/srv/it's", []byte("#!/bin/sh\npwd\n"))
	want = "#!/bin/sh\ncd -- '/srv/it'\\''s'\npwd\n"
	if string(got) != want {
		t.Fatalf("quoting = %q, want %q", got, want)
	}

	// A body without a shebang gets the line at the top.
	got = prependWorkingDirectory("/srv/app", []byte("echo hi\n"))
	if string(got) != "cd -- '/srv/app'\necho hi\n" {
		t.Fatalf("no-shebang prepend = %q", got)
	}

	// A bare shebang without a newline still yields a valid script.
	got = prependWorkingDirectory("/srv/app", []byte("#!/bin/sh"))
	if string(got) != "#!/bin/sh\ncd -- '/srv/app'\n" {
		t.Fatalf("bare shebang = %q", got)
	}
}

func TestExecuteUsesPerHookTimeout(t *testing.T) {
	sleep := executable(t, "#!/bin/sh\nsleep 1\n")
	cfg := config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:    "slow-but-allowed",
			Command: sleep,
			Enabled: true,
			Timeout: 2 * time.Second,
		}},
	}
	executor := NewWebhookExecutor(cfg)
	result, requestErr := executor.RunAction(context.Background(), "slow-but-allowed", nil, "test", "test")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !result.OK {
		t.Fatalf("per-hook timeout should have allowed the run: %+v", result)
	}

	// Without the override the daemon-wide timeout still applies.
	cfg.Actions[0].Timeout = 0
	executor = NewWebhookExecutor(cfg)
	result, requestErr = executor.RunAction(context.Background(), "slow-but-allowed", nil, "test", "test")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if result.OK {
		t.Fatalf("daemon-wide timeout should have killed the run: %+v", result)
	}
}

func TestScriptActionSubstitutesTemplate(t *testing.T) {
	script := executable(t, "#!/bin/sh\necho \"hello {{ NAME }}\"\n")
	cfg := config.DaemonConfig{
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name:    "greet",
			Command: script,
			Script:  true,
			Enabled: true,
		}},
	}
	executor := NewWebhookExecutor(cfg)

	result, requestErr := executor.RunAction(
		context.Background(),
		"greet",
		[]byte(`{"NAME":"world"}`),
		"test",
		"test",
	)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !result.OK || !strings.Contains(result.Stdout, "hello world") {
		t.Fatalf("unexpected result: %+v", result)
	}

	// Missing values fail with a clear message and no script exit code.
	result, requestErr = executor.RunAction(context.Background(), "greet", []byte(`{}`), "test", "test")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if result.OK || !strings.Contains(result.Error, "NAME") {
		t.Fatalf("error should name the missing variable: %+v", result)
	}
}

func TestRelayExecutesSecretlessActions(t *testing.T) {
	output := filepath.Join(t.TempDir(), "body")
	script := executable(t, "#!/bin/sh\nprintf '%s' ok > "+output+"\n")
	cfg := config.DaemonConfig{
		ScriptTimeout: time.Second, MaxBodyBytes: 1024, MaxConcurrentRuns: 1,
		Actions:  []config.WebhookConfig{{Name: "cleanup", Command: script, Enabled: true}},
		Webhooks: []config.WebhookConfig{{Name: "hook", Secret: "secret", Command: script, Enabled: true}},
	}
	executor := NewWebhookExecutor(cfg)

	// Actions carry no secret by design: the relay runs them without a
	// signature because the request came through the daemon's own
	// cloud-authenticated poll.
	resp, status := executor.ExecuteWebhook("cleanup", []byte("{}"), "", "relay", "ci-bot")
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("secretless action rejected: status %d ok %v err %q", status, resp.OK, resp.Stderr)
	}
	// Webhooks still require their signature on the relay path.
	resp, status = executor.ExecuteWebhook("hook", []byte("{}"), "", "relay", "@alice")
	if status != http.StatusUnauthorized {
		t.Fatalf("webhook without signature expected unauthorized, got %d", status)
	}
	resp, status = executor.ExecuteWebhook("hook", []byte("{}"), signedHeader("secret", []byte("{}")), "relay", "@alice")
	if status != http.StatusOK {
		t.Fatalf("signed webhook relay rejected: %d", status)
	}
	// Unknown names stay 404.
	resp, status = executor.ExecuteWebhook("missing", []byte("{}"), "", "relay", "")
	if status != http.StatusNotFound {
		t.Fatalf("unknown action expected 404, got %d", status)
	}
}
func TestActionCompletionInvokesNotificationHandler(t *testing.T) {
	script := executable(t, "#!/bin/sh\nprintf '%s' done\n")
	cfg := config.DaemonConfig{
		ScriptTimeout: time.Second, MaxBodyBytes: 1024, MaxConcurrentRuns: 1,
		Actions: []config.WebhookConfig{{
			Name: "cleanup", Command: script, Enabled: true, NotifyOnSuccess: true,
		}},
	}
	executor := NewWebhookExecutor(cfg)
	var completed config.WebhookConfig
	var ok bool
	executor.SetCompletionHandler(func(hook config.WebhookConfig, success bool, _ int, _ string, _ time.Duration) {
		completed = hook
		ok = success
	})
	result, requestErr := executor.RunAction(context.Background(), "cleanup", []byte("{}"), "relay", "ci-bot")
	if requestErr != nil || !result.OK {
		t.Fatalf("action execution failed: result=%+v error=%v", result, requestErr)
	}
	if !ok || completed.Name != "cleanup" || !completed.NotifyOnSuccess {
		t.Fatalf("action completion callback missing: ok=%v hook=%+v", ok, completed)
	}
}
