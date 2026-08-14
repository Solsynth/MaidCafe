package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

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
}

func bearerSecret(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return "", false
	}
	secret := strings.TrimSpace(value[7:])
	return secret, secret != ""
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
	secret, ok := bearerSecret(r)
	if !ok || subtle.ConstantTimeCompare([]byte(secret), []byte(hook.Secret)) != 1 {
		return executionResponse{}, 0, &requestError{status: http.StatusUnauthorized, message: "unauthorized"}
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return executionResponse{}, 0, &requestError{status: http.StatusTooManyRequests, message: "too many concurrent runs"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, e.maxBodyBytes+1))
	if err != nil {
		return executionResponse{}, 0, &requestError{status: http.StatusRequestEntityTooLarge, message: "request body too large"}
	}
	if int64(len(body)) > e.maxBodyBytes {
		return executionResponse{}, 0, &requestError{status: http.StatusRequestEntityTooLarge, message: "request body too large"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), e.scriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, hook.Command, hook.Args...)
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(body)
	stdout, stderr := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err = cmd.Run()
	duration := time.Since(start)
	exitCode, status := 0, http.StatusOK
	timedOut := ctx.Err() == context.DeadlineExceeded
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
	return executionResponse{OK: err == nil, Name: name, ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, status, nil
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
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return executionResponse{}, &requestError{
			status:  http.StatusTooManyRequests,
			message: "too many concurrent runs",
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, e.scriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, hook.Command, hook.Args...)
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(body)
	stdout, stderr := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)
	exitCode := 0
	if err != nil {
		e.counts.failures.Add(1)
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
		Name:     name,
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
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
