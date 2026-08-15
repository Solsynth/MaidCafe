package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// templateVarPattern matches {{ name }} placeholders inside action scripts.
// Names are deliberately free-form (no case convention): the requester picks
// the vocabulary, e.g. {{ SERVICE_NAME }} or {{ serviceName }}.
var templateVarPattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// substituteScriptTemplate replaces {{ name }} placeholders in the action
// script at [scriptPath] with values from the JSON request [body]. The
// runner is an SSH- or HMAC-authenticated trusted source, so values are
// inserted verbatim (no shell escaping); template control is the feature,
// not a boundary against untrusted input. A placeholder whose name is absent
// from the body is an error so a missing value never silently becomes a
// broken command line.
func substituteScriptTemplate(scriptPath string, body []byte) ([]byte, error) {
	source, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read action script: %w", err)
	}
	var values map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		// The body is also piped to the script on stdin, so it may be
		// arbitrary bytes; only JSON bodies can carry template values.
		_ = json.Unmarshal(body, &values)
	}
	var missing []string
	substituted := templateVarPattern.ReplaceAllStringFunc(
		string(source),
		func(match string) string {
			name := templateVarPattern.FindStringSubmatch(match)[1]
			value, ok := values[name]
			if !ok {
				missing = append(missing, name)
				return match
			}
			if value == nil {
				return ""
			}
			return fmt.Sprint(value)
		},
	)
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"action requires %s",
			strings.Join(missing, ", "),
		)
	}
	return []byte(substituted), nil
}

type counters struct {
	successes atomic.Uint64
	failures  atomic.Uint64
}

func (c *counters) values() (uint64, uint64) { return c.successes.Load(), c.failures.Load() }

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.buf.Len() < b.limit {
		remaining := b.limit - b.buf.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return len(p), nil
}
func (b *limitedBuffer) String() string { return b.buf.String() }

type WebhookExecutor struct {
	hooks         map[string]config.WebhookConfig
	actions       map[string]config.WebhookConfig
	scriptTimeout time.Duration
	maxBodyBytes  int64
	slots         chan struct{}
	counts        counters
	onComplete    func(config.WebhookConfig, bool, int, string, time.Duration)
}

func NewWebhookExecutor(cfg config.DaemonConfig) *WebhookExecutor {
	hooks := make(map[string]config.WebhookConfig, len(cfg.Webhooks)+len(cfg.Actions))
	actions := make(map[string]config.WebhookConfig, len(cfg.Actions))
	for _, h := range cfg.Webhooks {
		hooks[h.Name] = h
	}
	for _, action := range cfg.Actions {
		hooks[action.Name] = action
		actions[action.Name] = action
	}
	return &WebhookExecutor{
		hooks:         hooks,
		actions:       actions,
		scriptTimeout: cfg.ScriptTimeout,
		maxBodyBytes:  cfg.MaxBodyBytes,
		slots:         make(chan struct{}, cfg.MaxConcurrentRuns),
	}
}
func (e *WebhookExecutor) SetCompletionHandler(handler func(config.WebhookConfig, bool, int, string, time.Duration)) {
	e.onComplete = handler
}
func (e *WebhookExecutor) Counts() (uint64, uint64) { return e.counts.values() }

type executionResponse struct {
	OK       bool   `json:"ok"`
	Name     string `json:"name"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// Error carries a request-level failure (template missing a value, unread
	// script) that is not a script exit; status is 4xx/5xx alongside.
	Error string `json:"error,omitempty"`
}

func bearerSecret(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return "", false
	}
	secret := strings.TrimSpace(value[7:])
	return secret, secret != ""
}

func authorizedRequest(r *http.Request, expected string) bool {
	secret, ok := bearerSecret(r)
	return ok && strings.TrimSpace(expected) != "" &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) == 1
}

// signatureValid reports whether [provided] is the lowercase-hex
// HMAC-SHA256 of [body] keyed by [secret]. Webhook and action invocations are
// authenticated this way; the transport (SSH tunnel, Tailscale or the MaidKit
// cloud relay) already provides confidentiality.
func signatureValid(secret string, body []byte, provided string) bool {
	if secret == "" || provided == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	computed := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(strings.TrimSpace(provided))) == 1
}

type requestError struct {
	status  int
	message string
}

func (e *WebhookExecutor) run(name string, r *http.Request) (executionResponse, int, *requestError) {
	hook, exists := e.hooks[name]
	if !exists || !hook.Enabled {
		return executionResponse{}, 0, &requestError{status: http.StatusNotFound, message: "not found"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, e.maxBodyBytes+1))
	if err != nil {
		return executionResponse{}, 0, &requestError{status: http.StatusBadRequest, message: "read request body"}
	}
	if int64(len(body)) > e.maxBodyBytes {
		return executionResponse{}, 0, &requestError{status: http.StatusRequestEntityTooLarge, message: "request body too large"}
	}
	// The signature is the only credential a webhook needs: it proves the
	// caller holds the hook secret and that the body is untampered.
	if !signatureValid(hook.Secret, body, r.Header.Get("X-MaidCafe-Signature")) {
		return executionResponse{}, 0, &requestError{status: http.StatusUnauthorized, message: "unauthorized"}
	}
	response, status := e.execute(r.Context(), hook, body)
	if status == http.StatusTooManyRequests {
		return executionResponse{}, 0, &requestError{status: status, message: "too many concurrent runs"}
	}
	return response, status, nil
}

// ExecuteWebhook verifies the HMAC signature over [body] and runs the named
// webhook. Used by the MaidKit cloud relay, where the request is delivered by
// polling instead of an HTTP handler.
func (e *WebhookExecutor) ExecuteWebhook(name string, body []byte, signature string) (executionResponse, int) {
	hook, exists := e.hooks[name]
	if !exists || !hook.Enabled {
		return executionResponse{}, http.StatusNotFound
	}
	if !signatureValid(hook.Secret, body, signature) {
		return executionResponse{}, http.StatusUnauthorized
	}
	return e.execute(context.Background(), hook, body)
}

// buildRunCommand returns the exec.Cmd for a configured hook, honoring the
// hook's working directory, environment and run-as user.
//
// Without a run-as user the command executes directly: the working directory
// defaults to the daemon's own, and the hook's KEY=VALUE entries are appended
// to the daemon's environment.
//
// With a run-as user the command is delegated to sudo (the daemon itself is
// unprivileged; the sudoers rule granting the daemon the right to run
// MaidKit-deployed scripts as that user is installed by MaidKit). Environment
// entries are passed as command-line VAR=value assignments so sudo's
// env_reset still applies them, HOME is set to the target user's home with
// -H, and a non-empty working directory is applied by a fixed wrapper — no
// request-controlled input is ever interpolated into it.
func buildRunCommand(ctx context.Context, hook config.WebhookConfig, command string, args []string) *exec.Cmd {
	if hook.User == "" {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = hook.Cwd
		if cmd.Dir == "" {
			cmd.Dir = "/"
		}
		if len(hook.Env) > 0 {
			cmd.Env = append(os.Environ(), hook.Env...)
		}
		return cmd
	}
	argv := []string{"sudo", "-H", "-u", hook.User}
	argv = append(argv, hook.Env...)
	if hook.Cwd != "" {
		argv = append(argv,
			"sh", "-c", `cd -- "$1" && shift && exec "$@"`,
			"maidcafe", hook.Cwd, command,
		)
		argv = append(argv, args...)
	} else {
		argv = append(argv, command)
		argv = append(argv, args...)
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// renderScriptTemp writes the substituted [script] to a per-run file the
// executor then executes, so the deployed script on disk stays untouched.
//
// Without a run-as user the file lives in the system temp directory (private
// under systemd PrivateTmp) with 0700. With a run-as user the target account
// must read and execute it, so it is rendered next to the deployed script
// under a hidden .run directory with 0755 — the same directory the sudoers
// whitelist generated by MaidKit covers. The returned cleanup removes the
// file; the .run directory itself is left in place.
func renderScriptTemp(hook config.WebhookConfig, script []byte) (string, func(), error) {
	dir := ""
	mode := os.FileMode(0o700)
	if hook.User != "" {
		dir = filepath.Join(filepath.Dir(hook.Command), ".run")
		if err := os.MkdirAll(dir, 0o770); err != nil {
			return "", nil, fmt.Errorf(
				"create runtime script directory %s: %w (is it writable by the daemon user?)",
				dir, err,
			)
		}
		mode = 0o755
	}
	tmp, err := os.CreateTemp(dir, "maidcafe-action-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create runtime script: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(script); err != nil {
		tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("write runtime script: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("chmod runtime script: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close runtime script: %w", err)
	}
	return name, cleanup, nil
}

// execute runs a hook's command with [body] on stdin under the concurrency
// slot and script timeout, updating counters and completion notifications.
func (e *WebhookExecutor) execute(ctx context.Context, hook config.WebhookConfig, body []byte) (executionResponse, int) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return executionResponse{}, http.StatusTooManyRequests
	}
	runCtx, cancel := context.WithTimeout(ctx, e.scriptTimeout)
	defer cancel()
	command, args := hook.Command, hook.Args
	if hook.Script {
		// Substitute {{ name }} template variables from the request body and
		// run the rendered body from a per-run file so the deployed script
		// on disk stays untouched.
		script, err := substituteScriptTemplate(command, body)
		if err != nil {
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusBadRequest
		}
		rendered, cleanup, err := renderScriptTemp(hook, script)
		if err != nil {
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusInternalServerError
		}
		defer cleanup()
		command = rendered
	}
	cmd := buildRunCommand(runCtx, hook, command, args)
	cmd.Stdin = bytes.NewReader(body)
	stdout, stderr := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)
	exitCode, status := 0, http.StatusOK
	timedOut := runCtx.Err() == context.DeadlineExceeded
	if err != nil {
		e.counts.failures.Add(1)
		status = http.StatusBadGateway
		if timedOut {
			status = http.StatusGatewayTimeout
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	} else {
		e.counts.successes.Add(1)
	}
	if e.onComplete != nil {
		e.onComplete(hook, err == nil, exitCode, stderr.String(), duration)
	}
	return executionResponse{
		OK:       err == nil,
		Name:     hook.Name,
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, status
}

// RunAction executes a configured action through an authenticated SSH/stdin
// transport. SSH already provides the transport boundary, so action secrets
// are not required in this mode.
func (e *WebhookExecutor) RunAction(
	ctx context.Context,
	name string,
	body []byte,
) (executionResponse, *requestError) {
	if int64(len(body)) > e.maxBodyBytes {
		return executionResponse{}, &requestError{
			status:  http.StatusRequestEntityTooLarge,
			message: "request body too large",
		}
	}
	hook, exists := e.actions[name]
	if !exists || !hook.Enabled {
		return executionResponse{}, &requestError{
			status:  http.StatusNotFound,
			message: "not found",
		}
	}
	response, status := e.execute(ctx, hook, body)
	if status == http.StatusTooManyRequests {
		return executionResponse{}, &requestError{
			status:  http.StatusTooManyRequests,
			message: "too many concurrent runs",
		}
	}
	return response, nil
}

func (e *WebhookExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	response, status, requestErr := e.run(path.Base(r.URL.Path), r)
	if requestErr != nil {
		http.Error(w, requestErr.message, requestErr.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func (e *WebhookExecutor) GinHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		response, status, requestErr := e.run(c.Param("name"), c.Request)
		if requestErr != nil {
			c.AbortWithStatusJSON(requestErr.status, gin.H{"error": requestErr.message})
			return
		}
		c.JSON(status, response)
	}
}
