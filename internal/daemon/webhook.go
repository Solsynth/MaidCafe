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
		// run the rendered body from a per-run temp file so the deployed
		// script on disk stays untouched.
		script, err := substituteScriptTemplate(command, body)
		if err != nil {
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusBadRequest
		}
		tmp, err := os.CreateTemp("", "maidcafe-action-*.sh")
		if err != nil {
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusInternalServerError
		}
		if _, err := tmp.Write(script); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusInternalServerError
		}
		if err := tmp.Chmod(0o700); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusInternalServerError
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return executionResponse{
				OK:    false,
				Name:  hook.Name,
				Error: err.Error(),
			}, http.StatusInternalServerError
		}
		command = tmp.Name()
		defer func() { os.Remove(command) }()
	}
	cmd := exec.CommandContext(runCtx, command, args...)
	cmd.Dir = "/"
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
