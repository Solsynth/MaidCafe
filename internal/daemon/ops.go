package daemon

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Native host operations: typed, validated mutations the daemon executes
// directly — container lifecycle, process kill, systemd unit actions and
// compose project actions. They mirror the operations MaidKit performs over
// SSH, so a MaidCafe-managed host can be operated through the daemon: locally
// over HTTP, over the SSH stdio pipe, or remotely through the cloud relay —
// without a workstation SSH session.
//
// Safety: unlike script actions, native ops never interpolate caller input
// into a shell. Targets are validated against the same patterns MaidKit's SSH
// layer enforces, and commands run directly with exec.CommandContext (no
// shell wrapper), so a relayed request cannot inject commands. Root-owned
// resources are reached with a `sudo -n` retry mirroring the collectors;
// under the shipped systemd unit's NoNewPrivileges that retry is inert, so
// such ops fail with a clear error and MaidKit falls back to SSH.

// composeOpTimeout bounds compose actions, which are slow by nature (pulls,
// recreates). The daemon-wide scriptTimeout stays the default for the other
// native ops.
const composeOpTimeout = 5 * time.Minute

// Native op validation patterns, kept identical to the MaidKit client so both
// channels accept exactly the same targets.
var (
	nativeContainerRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	nativeSystemdUnitPattern  = regexp.MustCompile(`^[A-Za-z0-9:._@\-]+\.service$`)
	nativeProjectPattern      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	nativeDirectoryPattern    = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
)

// nativeContainerVerbs maps a container lifecycle action to the runtime CLI
// verb. remove uses `rm`, with -f when forced — exactly like the MaidKit SSH
// layer.
var nativeContainerVerbs = map[string]string{
	"start":   "start",
	"stop":    "stop",
	"restart": "restart",
	"pause":   "pause",
	"unpause": "unpause",
	"kill":    "kill",
	"remove":  "rm",
}

// nativeSystemdVerbs maps a systemd action to the systemctl verb.
var nativeSystemdVerbs = map[string]string{
	"start":   "start",
	"stop":    "stop",
	"restart": "restart",
	"reload":  "reload",
	"enable":  "enable",
	"disable": "disable",
}

// nativeComposeVerbs maps a compose project action to the CLI arguments
// (never built from caller input).
var nativeComposeVerbs = map[string]string{
	"up":       "up -d",
	"stop":     "stop",
	"restart":  "restart",
	"pull":     "pull",
	"recreate": "up -d --force-recreate",
}

// nativeOpDisplayNames maps every native slug to its human-readable label for
// the cloud action report and the audit log.
var nativeOpDisplayNames = map[string]string{
	"container.start":   "Start container",
	"container.stop":    "Stop container",
	"container.restart": "Restart container",
	"container.pause":   "Pause container",
	"container.unpause": "Unpause container",
	"container.kill":    "Kill container",
	"container.remove":  "Remove container",
	"process.kill":      "Kill process",
	"systemd.start":     "Start systemd unit",
	"systemd.stop":      "Stop systemd unit",
	"systemd.restart":   "Restart systemd unit",
	"systemd.reload":    "Reload systemd unit",
	"systemd.enable":    "Enable systemd unit",
	"systemd.disable":   "Disable systemd unit",
	"compose.up":        "Compose up",
	"compose.stop":      "Compose stop",
	"compose.restart":   "Compose restart",
	"compose.pull":      "Compose pull",
	"compose.recreate":  "Compose recreate",
}

// isNativeOpSlug reports whether [name] is a built-in native operation. Used
// by the cloud relay to dispatch by name before the hook table.
func isNativeOpSlug(name string) bool {
	for _, slug := range config.NativeOpNames {
		if slug == name {
			return true
		}
	}
	return false
}

// nativeOpReport returns the built-in operations in the same shape as
// configured actions, so the cloud lists them as invocable for this daemon.
func nativeOpReport() []config.WebhookConfig {
	report := make([]config.WebhookConfig, 0, len(config.NativeOpNames))
	for _, slug := range config.NativeOpNames {
		report = append(report, config.WebhookConfig{
			Name:        slug,
			DisplayName: nativeOpDisplayNames[slug],
			Enabled:     true,
		})
	}
	return report
}

// opParams carries a native operation's validated targets. The slug encodes
// the operation family and verb; the params carry the identity (container id,
// unit name, compose project) and per-family options (force for container
// remove, working directory for compose).
type opParams struct {
	target    string
	pid       int
	force     bool
	directory string
}

// nativeParamsFromValues builds opParams from a decoded JSON body (the cloud
// relay and stdio transports carry everything in the body; HTTP carries the
// identity in the path).
func nativeParamsFromValues(slug string, values map[string]any) opParams {
	var p opParams
	switch {
	case strings.HasPrefix(slug, "container."):
		p.target, _ = values["id"].(string)
		p.force, _ = values["force"].(bool)
	case slug == "process.kill":
		switch v := values["pid"].(type) {
		case float64:
			p.pid = int(v)
		case int:
			p.pid = v
		case string:
			p.pid, _ = strconv.Atoi(v)
		}
	case strings.HasPrefix(slug, "systemd."):
		p.target, _ = values["unit"].(string)
	case strings.HasPrefix(slug, "compose."):
		p.target, _ = values["project"].(string)
		p.directory, _ = values["directory"].(string)
	}
	return p
}

// opAttempt is one command the runner may try, in order.
type opAttempt struct {
	command string
	args    []string
	cwd     string
}

// nativeOpRunner executes native operations through the executor's
// concurrency slot, timeout, counters and audit log. Container runtimes are
// resolved with the shared runtime probe (podman first), falling back to the
// other runtime when the target is not found there.
type nativeOpRunner struct {
	executor      *WebhookExecutor
	runtimes      func(ctx context.Context) map[string]string
	scriptTimeout time.Duration
}

// dispatch validates [slug] and [params], builds the command attempts and
// runs them. Request-level failures return a requestError; execution
// outcomes return an executionResponse with the usual status codes (502 for
// non-zero exit, 504 for timeout).
func (r *nativeOpRunner) dispatch(
	ctx context.Context,
	slug string,
	params opParams,
	source string,
	invokedBy string,
) (executionResponse, int, *requestError) {
	var attempts []opAttempt
	timeout := r.scriptTimeout
	bad := func(message string) (executionResponse, int, *requestError) {
		return executionResponse{}, 0, &requestError{status: http.StatusBadRequest, message: message}
	}
	switch {
	case strings.HasPrefix(slug, "container."):
		verb := strings.TrimPrefix(slug, "container.")
		cliVerb, known := nativeContainerVerbs[verb]
		if !known {
			return bad("unknown container action")
		}
		if !nativeContainerRefPattern.MatchString(params.target) {
			return bad("invalid container reference")
		}
		args := []string{cliVerb}
		if verb == "remove" && params.force {
			args = append(args, "-f")
		}
		args = append(args, params.target)
		for _, runtime := range []string{"podman", "docker"} {
			path, ok := r.runtimes(ctx)[runtime]
			if !ok {
				continue
			}
			attempts = append(attempts, opAttempt{command: path, args: args})
			if sudo := r.sudoAttempt(); sudo != nil {
				attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{path}, args...)...)})
			}
		}
	case slug == "process.kill":
		if params.pid <= 1 {
			return bad("invalid process id")
		}
		args := []string{"-s", "KILL", "--", strconv.Itoa(params.pid)}
		attempts = append(attempts, opAttempt{command: "kill", args: args})
		if sudo := r.sudoAttempt(); sudo != nil {
			attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{"kill"}, args...)...)})
		}
	case strings.HasPrefix(slug, "systemd."):
		verb := strings.TrimPrefix(slug, "systemd.")
		if _, known := nativeSystemdVerbs[verb]; !known {
			return bad("unknown systemd action")
		}
		unit := strings.TrimSpace(params.target)
		if unit != "" && !strings.Contains(unit, ".") {
			unit += ".service"
		}
		if !nativeSystemdUnitPattern.MatchString(unit) {
			return bad("invalid systemd unit")
		}
		args := []string{verb, unit}
		attempts = append(attempts, opAttempt{command: "systemctl", args: args})
		if sudo := r.sudoAttempt(); sudo != nil {
			attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{"systemctl"}, args...)...)})
		}
	case strings.HasPrefix(slug, "compose."):
		verb := strings.TrimPrefix(slug, "compose.")
		cliArgs, known := nativeComposeVerbs[verb]
		if !known {
			return bad("unknown compose action")
		}
		if !nativeProjectPattern.MatchString(params.target) || !validNativeDirectory(params.directory) {
			return bad("invalid compose project or directory")
		}
		if timeout < composeOpTimeout {
			timeout = composeOpTimeout
		}
		for _, runtime := range []string{"podman", "docker"} {
			path, ok := r.runtimes(ctx)[runtime]
			if !ok {
				continue
			}
			args := append([]string{"compose", "--ansi", "never", "-p", params.target}, strings.Fields(cliArgs)...)
			attempts = append(attempts, opAttempt{command: path, args: args, cwd: params.directory})
			if sudo := r.sudoAttempt(); sudo != nil {
				attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{path}, args...)...), cwd: params.directory})
			}
		}
	default:
		return bad("unknown operation")
	}
	if len(attempts) == 0 {
		return executionResponse{}, 0, &requestError{status: http.StatusBadGateway, message: "no container runtime available"}
	}
	response, status := r.executeNative(ctx, slug, nativeOpDisplayNames[slug], attempts, timeout, source, invokedBy)
	return response, status, nil
}

// sudoAttempt returns the sudo -n prefix when the daemon is not root and
// sudo is available, else nil. Mirrors the collectors' never-interactive
// elevation.
func (r *nativeOpRunner) sudoAttempt() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return nil
	}
	return []string{"sudo", "-n"}
}

// validNativeDirectory checks an absolute compose working directory with no
// traversal, mirroring MaidKit's SSH-layer check.
func validNativeDirectory(value string) bool {
	return value != "" && !strings.Contains(value, "..") && nativeDirectoryPattern.MatchString(value)
}

// executeNative runs [attempts] in order under one concurrency slot and one
// timeout budget, recording a single audit entry and updating the execution
// counters. A failed attempt retries with the next (e.g. the sudo or
// alternate-runtime variant); the first attempt's failure stays the primary
// signal when every attempt fails, mirroring the collectors.
func (r *nativeOpRunner) executeNative(
	ctx context.Context,
	slug string,
	displayName string,
	attempts []opAttempt,
	timeout time.Duration,
	source string,
	invokedBy string,
) (executionResponse, int) {
	select {
	case r.executor.slots <- struct{}{}:
		defer func() { <-r.executor.slots }()
	default:
		return executionResponse{Name: slug}, http.StatusTooManyRequests
	}
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var response executionResponse
	response.Name = slug
	var primaryErr error
	var primaryStdout, primaryStderr string
	for _, attempt := range attempts {
		stdout, stderr, exitCode, err := runOpOnce(runCtx, attempt)
		if err == nil {
			response.OK = true
			response.ExitCode = 0
			response.Stdout = stdout
			response.Stderr = stderr
			// A later attempt succeeded; the earlier failure is irrelevant.
			primaryErr = nil
			break
		}
		if primaryErr == nil {
			primaryErr = err
			primaryStdout = stdout
			primaryStderr = stderr
			response.ExitCode = exitCode
		}
		if runCtx.Err() == context.DeadlineExceeded {
			break
		}
	}
	if primaryErr != nil {
		response.OK = false
		response.Stdout = primaryStdout
		response.Stderr = primaryStderr
	}
	if r.executor.audit != nil {
		r.executor.audit.Record(auditEntry{
			Timestamp:   time.Now().UTC(),
			Name:        slug,
			DisplayName: displayName,
			Source:      source,
			InvokedBy:   invokedBy,
			OK:          response.OK,
			ExitCode:    response.ExitCode,
			DurationMS:  time.Since(started).Milliseconds(),
			Stdout:      response.Stdout,
			Stderr:      response.Stderr,
			Error:       auditErrorTail(response),
		})
	}
	status := http.StatusOK
	if primaryErr != nil {
		r.executor.counts.failures.Add(1)
		status = http.StatusBadGateway
		if runCtx.Err() == context.DeadlineExceeded {
			status = http.StatusGatewayTimeout
		}
	} else {
		r.executor.counts.successes.Add(1)
	}
	return response, status
}

// runOpOnce runs one attempt with bounded stdout/stderr capture and no shell.
func runOpOnce(ctx context.Context, attempt opAttempt) (stdout, stderr string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, attempt.command, attempt.args...)
	cmd.Dir = attempt.cwd
	outBuf, errBuf := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}
