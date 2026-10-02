package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/privfs"
)

// privRunner runs maidkit-priv, the root-capable helper, for one file
// operation. The daemon itself stays unprivileged: it reaches root through a
// passwordless sudoers rule scoped to the helper, and the helper resolves the
// target directory from its own root-owned profile file. A compromised daemon
// can therefore name only directories an operator declared in both the daemon
// configuration and the helper's profiles — which is the point of the split.
//
// The daemon never passes a directory to the helper: only a profile name, a
// profile-relative path and a mode. The file API's caller reaches those three
// fields only through the policy's validation, and the helper validates them
// again against its own file.
//
// Only the sudo prefix is held here. The helper *path* is read from the live
// configuration on every call, so a hot reload moves it without restarting and
// without a pointer swap a request could race.
type privRunner struct {
	// sudo is the prefix used to reach root. It is empty when the daemon
	// already runs as root, and in tests that substitute a stand-in helper.
	sudo []string
}

// privRunnerTimeout bounds one helper invocation. A privileged write is a
// small file operation; a helper that has not finished by now is wedged, and
// holding the request open would only hide that.
const privRunnerTimeout = 30 * time.Second

// newPrivRunner returns nil when the daemon has no way to elevate. Running as
// root makes sudo unnecessary; otherwise sudo must exist, because without it
// every privileged write would fail with a bare permission error that says
// nothing about the missing installation.
func newPrivRunner() *privRunner {
	if os.Geteuid() == 0 {
		return &privRunner{}
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return nil
	}
	return &privRunner{sudo: []string{"sudo", "-n"}}
}

// helperPath resolves the helper binary from the live configuration.
func (a *App) helperPath() string {
	if path := strings.TrimSpace(a.rt.Load().priv.Helper); path != "" {
		return path
	}
	return config.FilesDefaultPrivilegedHelper
}

// privateSystemd reports whether native systemd actions are routed through the
// helper. Read per request, so a reload switches it without a restart.
func (a *App) privateSystemd() bool { return a.rt.Load().priv.Systemd }

// argv renders the full argument vector for one helper invocation, with the
// sudo prefix this host needs. It is the single place that knows how the daemon
// reaches root, so the file API and the native ops cannot spell it differently.
func (r *privRunner) argv(path string, args ...string) []string {
	argv := append([]string{}, r.sudo...)
	argv = append(argv, path)
	return append(argv, args...)
}

// run executes one helper verb with [payload] on stdin. It returns the
// helper's stdout and stderr, and a request-level failure when the invocation
// failed.
//
// The failure text decides how it is reported: a refusal (the helper rejected
// the profile, path or mode), a missing sudoers rule and a missing binary are
// answers an operator must act on, so the helper's own message survives.
func (r *privRunner) run(ctx context.Context, path, verb, profile, rel, mode string, payload []byte) (string, string, *fileActionError) {
	args := r.argv(path, "fs", verb, profile, rel)
	if mode != "" {
		args = append(args, mode)
	}
	runCtx, cancel := context.WithTimeout(ctx, privRunnerTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, args[0], args[1:]...)
	if payload != nil {
		command.Stdin = bytes.NewReader(payload)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	out, errText := stdout.String(), strings.TrimSpace(stderr.String())
	if err == nil {
		return out, errText, nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return out, errText, fileError(http.StatusGatewayTimeout, "privileged helper timed out after %s", privRunnerTimeout)
	}
	return out, errText, privFailure(err, errText, path)
}

// privFailure turns the helper's exit into the response a caller sees. The
// messages operators actually hit each get their own advice, because "exit
// status 1" is not something anyone can act on.
func privFailure(err error, stderr, path string) *fileActionError {
	switch {
	case strings.Contains(stderr, "a password is required"),
		strings.Contains(stderr, "no tty present"),
		strings.Contains(stderr, "no askpass program"):
		return fileError(http.StatusForbidden,
			"the privileged helper needs a passwordless sudoers rule; install the one `%s sudoers` prints", path)
	case strings.Contains(stderr, "must run as root"):
		return fileError(http.StatusFailedDependency,
			"the privileged helper %s was not granted root; install its sudoers rule", path)
	case strings.Contains(stderr, "command not found"),
		strings.Contains(stderr, "No such file or directory"):
		return fileError(http.StatusFailedDependency, "the privileged helper %s is not installed", path)
	}
	message := helperMessage(stderr)
	if message == "" {
		message = err.Error()
	}
	// A refusal and an I/O failure both arrive here, and the helper's own
	// message says which — it is passed through rather than flattened.
	return fileError(http.StatusBadGateway, "the privileged helper failed: %s", message)
}

// helperMessage picks the operator-facing line out of the helper's stderr. The
// helper writes a human sentence and a JSON audit record to the same stream, so
// the sentence is preferred and the JSON is never pasted into an API response.
func helperMessage(stderr string) string {
	if stderr == "" {
		return ""
	}
	lines := strings.Split(stderr, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "maidkit-priv: ") {
			return strings.TrimSpace(line)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "{") {
			return line
		}
	}
	return ""
}

// privWrite writes [data] into a privileged root through the helper, which
// selects the profile and enforces its own path and mode rules.
func (a *App) privWrite(ctx context.Context, target fileTarget, data []byte, mode os.FileMode) *fileActionError {
	if a.priv == nil {
		return fileError(http.StatusServiceUnavailable, "no privileged helper is available on this host")
	}
	if len(data) > privfs.MaxContentBytes {
		return fileError(http.StatusRequestEntityTooLarge,
			"content is over the privileged write limit (%d bytes)", privfs.MaxContentBytes)
	}
	_, _, err := a.priv.run(ctx, a.helperPath(), "write", target.profile, target.rel, fmt.Sprintf("%04o", mode), data)
	return err
}

// privMkdir creates a directory in a privileged root through the helper.
func (a *App) privMkdir(ctx context.Context, target fileTarget, mode os.FileMode) *fileActionError {
	if a.priv == nil {
		return fileError(http.StatusServiceUnavailable, "no privileged helper is available on this host")
	}
	_, _, err := a.priv.run(ctx, a.helperPath(), "mkdir", target.profile, target.rel, fmt.Sprintf("%04o", mode), nil)
	return err
}

// privRemove deletes a file in a privileged root through the helper.
func (a *App) privRemove(ctx context.Context, target fileTarget) *fileActionError {
	if a.priv == nil {
		return fileError(http.StatusServiceUnavailable, "no privileged helper is available on this host")
	}
	_, _, err := a.priv.run(ctx, a.helperPath(), "remove", target.profile, target.rel, "", nil)
	return err
}

// privWriteModeFor picks the mode for a privileged write. An existing file's
// mode is preserved (the same truncate-in-place semantics every other write in
// this API has); a new file is created 0644. A mode the helper's profile does
// not grant is a refusal from the helper, never a silent downgrade, so an
// operator sees the profile they need to widen.
//
// The stat runs as the daemon account and is only used to choose a mode: a
// caller cannot turn it into an escalation, because the write itself is
// confined by the helper's own root.
func privWriteModeFor(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return info.Mode().Perm()
	}
	return 0o644
}

// privMkdirMode is the mode the API asks the helper for when it creates a
// directory in a privileged root.
const privMkdirMode = 0o755
