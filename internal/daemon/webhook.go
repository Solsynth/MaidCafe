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
	audit         *AuditLogger
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

// SetAuditLogger wires the durable execution log; nil disables auditing.
func (e *WebhookExecutor) SetAuditLogger(audit *AuditLogger) {
	e.audit = audit
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
	ctx := r.Context()
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
	response, status := e.execute(ctx, hook, body, "http")
	if status == http.StatusTooManyRequests {
		return executionResponse{}, 0, &requestError{status: status, message: "too many concurrent runs"}
	}
	return response, status, nil
}

// ExecuteWebhook verifies the HMAC signature over [body] and runs the named
// webhook. Used by the MaidKit cloud relay, where the request is delivered by
// polling instead of an HTTP handler. [source] is recorded in the audit log.
//
// Hooks without a secret (actions, which carry none by design) skip the
// signature check: the relay only delivers requests the daemon itself pulled
// with its cloud secret, so the cloud's workspace-member authorization is the
// credential. The local HTTP endpoint keeps requiring a signature for every
// hook.
func (e *WebhookExecutor) ExecuteWebhook(name string, body []byte, signature string, source string) (executionResponse, int) {
	hook, exists := e.hooks[name]
	if !exists || !hook.Enabled {
		return executionResponse{}, http.StatusNotFound
	}
	if hook.Secret != "" && !signatureValid(hook.Secret, body, signature) {
		return executionResponse{}, http.StatusUnauthorized
	}
	return e.execute(context.Background(), hook, body, source)
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
// MaidKit-deployed scripts as that user is installed by MaidKit). The command
// stays the configured absolute path — never a shell wrapper — so the sudoers
// rule matches it. Environment entries are passed as command-line VAR=value
// assignments so sudo's env_reset still applies them; HOME is set to the
// target user's home with -H.
//
// The working directory is applied without sudo -D (which default sudoers
// rules reject): script actions already carry an injected `cd` line in their
// body, and plain commands inherit the daemon-set cwd, since sudo does not
// reset the working directory of the command it runs.
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
	argv = append(argv, command)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if !hook.Script && hook.Cwd != "" {
		// The chdir happens as the daemon user before sudo execs; the target
		// account inherits it. Script actions skip this — their body cds as
		// the target user instead.
		cmd.Dir = hook.Cwd
	}
	return cmd
}

// prependWorkingDirectory prefixes a `cd -- '<cwd>'` line to a script body so
// the rendered script runs in [cwd] without any shell wrapper around sudo.
// The line is inserted after a leading shebang (the kernel requires it on
// line one) and [cwd] is escaped for single quotes.
func prependWorkingDirectory(cwd string, script []byte) []byte {
	line := "cd -- '" + strings.ReplaceAll(cwd, "'", "'\\''") + "'\n"
	if len(script) >= 2 && script[0] == '#' && script[1] == '!' {
		if nl := bytes.IndexByte(script, '\n'); nl >= 0 {
			head := append([]byte{}, script[:nl+1]...)
			tail := append([]byte{}, script[nl+1:]...)
			return append(append(head, line...), tail...)
		}
		// A bare shebang with no newline: append the line after it.
		script = append(script, '\n')
		return append(append([]byte{}, script...), line...)
	}
	return append([]byte(line), script...)
}

// renderScriptTemp writes the substituted [script] to a per-run file the
// executor then executes, so the deployed script on disk stays untouched.
//
// Without a run-as user the file lives in the system temp directory (private
// under systemd PrivateTmp) with 0700. With a run-as user the target account
// must read and execute it, so it is rendered next to the deployed script
// under a run directory with 0755 — the sudoers whitelist generated
// by MaidKit covers `run/*` (plus the actions dir; sudoers wildcards do not
// cross "/"). The returned cleanup removes the file; the run directory
// itself is left in place.
func renderScriptTemp(hook config.WebhookConfig, script []byte) (string, func(), error) {
	dir := ""
	mode := os.FileMode(0o700)
	if hook.User != "" {
		dir = filepath.Join(filepath.Dir(hook.Command), "run")
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

// auditErrorTail captures a short human-readable failure reason for the
// audit log: the request-level error when there is one, else a truncated
// stderr tail. Never more than 512 bytes so one bad run cannot bloat the log.
func auditErrorTail(response executionResponse) string {
	body := response.Error
	if body == "" {
		body = response.Stderr
	}
	if body == "" {
		return ""
	}
	if len(body) > 512 {
		body = body[:512]
	}
	return body
}

// execute runs a hook's command with [body] on stdin under the concurrency
// slot and script timeout, updating counters, completion notifications and
// the audit log. [source] records how the run was triggered (http, stdio or
// relay); concurrency rejections are not logged — they are not executions.
func (e *WebhookExecutor) execute(
	ctx context.Context,
	hook config.WebhookConfig,
	body []byte,
	source string,
) (response executionResponse, status int) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return executionResponse{}, http.StatusTooManyRequests
	}
	started := time.Now()
	defer func() {
		if e.audit == nil {
			return
		}
		e.audit.Record(auditEntry{
			Timestamp:   time.Now().UTC(),
			Name:        hook.Name,
			DisplayName: strings.TrimSpace(hook.DisplayName),
			Source:      source,
			OK:          response.OK,
			ExitCode:    response.ExitCode,
			DurationMS:  time.Since(started).Milliseconds(),
			Error:       auditErrorTail(response),
		})
	}()
	timeout := e.scriptTimeout
	if hook.Timeout > 0 {
		// Per-hook override; the daemon-wide scriptTimeout stays the default.
		timeout = hook.Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command, args := hook.Command, hook.Args
	if hook.Script {
		// Substitute {{ name }} template variables from the request body and
		// run the rendered body from a per-run file so the deployed script
		// on disk stays untouched.
		script, err := substituteScriptTemplate(command, body)
		if err != nil {
			response = executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}
			return response, http.StatusBadRequest
		}
		if hook.Cwd != "" && hook.User != "" {
			// Apply the working directory inside the script itself: a shell
			// wrapper around sudo would break the sudoers match (the rule
			// names the script path, not `sh`). The line is trusted config
			// with single-quote escaping, inserted after the shebang so the
			// kernel still picks the interpreter.
			script = prependWorkingDirectory(hook.Cwd, script)
		}
		rendered, cleanup, err := renderScriptTemp(hook, script)
		if err != nil {
			response = executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}
			return response, http.StatusInternalServerError
		}
		defer cleanup()
		command = rendered
	}
	cmd := buildRunCommand(runCtx, hook, command, args)
	cmd.Stdin = bytes.NewReader(body)
	stdout, stderr := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	duration := time.Since(started)
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
	response = executionResponse{
		OK:       err == nil,
		Name:     hook.Name,
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
	return response, status
}

// RunAction executes a configured action through an authenticated SSH/stdin
// transport. SSH already provides the transport boundary, so action secrets
// are not required in this mode. [source] is recorded in the audit log.
func (e *WebhookExecutor) RunAction(
	ctx context.Context,
	name string,
	body []byte,
	source string,
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
	response, status := e.execute(ctx, hook, body, source)
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
