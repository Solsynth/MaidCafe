package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// tmuxListFormat is the machine-readable listing every route and the control
// channel share: one tab-separated row per session.
const tmuxListFormat = "#{session_name}\t#{session_windows}\t#{session_created}\t#{session_attached}"

// tmuxSessionView is one row of the background-session listing.
type tmuxSessionView struct {
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Created  int64  `json:"created"`
	Attached int    `json:"attached"`
}

// terminalTmuxInstalled reports whether this host has tmux at all.
func terminalTmuxInstalled() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// tmuxCommand builds an argv for a tmux subcommand under the same account
// wrapper buildTerminalCommand uses, so a listing sees the sessions the
// terminal would.
func tmuxCommand(policy config.TerminalConfig, user string, args ...string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return nil, errTerminalTmuxUnavailable
	}
	argv := append([]string{bin}, args...)
	if user == "" {
		return exec.Command(argv[0], argv[1:]...), nil
	}
	wrapped := append([]string{"sudo", "-H", "-u", user}, argv...)
	return exec.Command(wrapped[0], wrapped[1:]...), nil
}

// listTmuxSessions lists the tmux sessions visible to [user] on this host.
//
// [installed] is false when tmux is absent, which the caller reports instead of
// an error: a host without tmux is a normal state, not a failure. A missing
// server (`no server running`) is an empty list for the same reason.
func listTmuxSessions(ctx context.Context, policy config.TerminalConfig, user string) (sessions []tmuxSessionView, installed bool, err error) {
	if !terminalTmuxInstalled() {
		return nil, false, nil
	}
	cmd, err := tmuxCommand(policy, user, "list-sessions", "-F", tmuxListFormat)
	if err != nil {
		return nil, false, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.Contains(strings.ToLower(stderr.String()), "no server running") {
			return nil, true, nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, true, fmt.Errorf("list tmux sessions: %s", message)
	}
	return parseTmuxSessions(stdout.String()), true, nil
}

// parseTmuxSessions reads rows produced with tmuxListFormat. A row that does not
// parse is skipped rather than failing the listing: one odd session name must
// not hide every other session from the user.
func parseTmuxSessions(output string) []tmuxSessionView {
	sessions := make([]tmuxSessionView, 0, 4)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		windows, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		created, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			continue
		}
		attached, err := strconv.Atoi(strings.TrimSpace(fields[3]))
		if err != nil {
			continue
		}
		sessions = append(sessions, tmuxSessionView{
			Name:     fields[0],
			Windows:  windows,
			Created:  created,
			Attached: attached,
		})
	}
	return sessions
}

// killTmuxSession ends one tmux session, every process in it.
func killTmuxSession(ctx context.Context, policy config.TerminalConfig, user, name string) error {
	cmd, err := tmuxCommand(policy, user, "kill-session", "-t", name)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("kill tmux session: %s", message)
	}
	return nil
}

// tmuxSessionExists reports whether a session named [name] exists. It lets the
// delete route answer 404 instead of passing tmux's own exit status through.
func tmuxSessionExists(ctx context.Context, policy config.TerminalConfig, user, name string) bool {
	cmd, err := tmuxCommand(policy, user, "has-session", "-t", "="+name)
	if err != nil {
		return false
	}
	return cmd.Run() == nil
}

// authorizeTerminalHTTP authenticates a plain HTTP terminal route with the same
// credential the terminal socket accepts. The routes are registered without
// authorizeMetrics because the terminal has its own secret, and they are not
// WebSocket upgrades, so the bearer header alone is enough.
func (a *App) authorizeTerminalHTTP(c *gin.Context, policy config.TerminalConfig) bool {
	secret, ok := bearerSecret(c.Request)
	expected := terminalExpectedSecret(policy, a.cfg.MetricsSecret)
	if !ok || expected == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return false
	}
	return true
}

// terminalSessionsGate applies the refusals both session routes share and
// returns the policy once they pass.
func (a *App) terminalSessionsGate(c *gin.Context) (config.TerminalConfig, bool) {
	policy := a.rt.Load().terminal
	if !a.authorizeTerminalHTTP(c, policy) {
		return policy, false
	}
	if !policy.Enabled {
		// The listing reveals whether anybody is using the host's shells, so it
		// is as protected as opening one.
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal disabled"})
		return policy, false
	}
	if !terminalSupported() {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"ok": false, "error": "terminal unsupported on this platform"})
		return policy, false
	}
	if user := strings.TrimSpace(c.Query("user")); user != "" && !terminalUserAllowed(policy, user) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal user not allowed"})
		return policy, false
	}
	return policy, true
}

// terminalUserAllowed reports whether [user] is in the policy's allowlist.
func terminalUserAllowed(policy config.TerminalConfig, user string) bool {
	for _, name := range policy.Users {
		if name == user {
			return true
		}
	}
	return false
}

// handleTerminalSessions lists the host's tmux sessions, so a client can offer
// the ones still running without a terminal attached.
func (a *App) handleTerminalSessions(c *gin.Context) {
	policy, ok := a.terminalSessionsGate(c)
	if !ok {
		return
	}
	user := strings.TrimSpace(c.Query("user"))
	sessions, installed, err := listTmuxSessions(c.Request.Context(), policy, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if !installed {
		c.JSON(http.StatusNotImplemented, gin.H{"ok": false, "error": errTerminalTmuxUnavailable.Error()})
		return
	}
	if sessions == nil {
		sessions = []tmuxSessionView{}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "tmux": true, "sessions": sessions})
}

// handleTerminalSessionKill ends one tmux session by name.
func (a *App) handleTerminalSessionKill(c *gin.Context) {
	policy, ok := a.terminalSessionsGate(c)
	if !ok {
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	if !terminalTmuxName.MatchString(name) {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": errTerminalTmuxInvalid.Error()})
		return
	}
	if !terminalTmuxInstalled() {
		c.JSON(http.StatusNotImplemented, gin.H{"ok": false, "error": errTerminalTmuxUnavailable.Error()})
		return
	}
	user := strings.TrimSpace(c.Query("user"))
	if !tmuxSessionExists(c.Request.Context(), policy, user, name) {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "no such tmux session"})
		return
	}
	if err := killTmuxSession(c.Request.Context(), policy, user, name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
