package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// terminalRelayPollBackoff is the pause after a failed pickup and while the
// relay is switched off. A cloud outage must not turn the relay into a hot
// loop of failing or useless requests.
const terminalRelayPollBackoff = 5 * time.Second

// terminalRelayPollGrace is added to the pickup's own wait for its HTTP
// deadline, so the client never gives up before the cloud answers. It is the
// only call that outlives the configured RequestTimeout.
const terminalRelayPollGrace = 15 * time.Second

// terminalRelayDialTimeout bounds the outbound agent WebSocket handshake. The
// session that follows is unbounded, like the direct endpoint's.
const terminalRelayDialTimeout = 15 * time.Second

// terminalRelaySession is one session the cloud handed over in a pickup. Its
// JSON names are the cloud's frozen pickup payload, so the two sides agree on
// the wire shape without either parsing PTY bytes.
type terminalRelaySession struct {
	ID        string `json:"id"`
	Shell     string `json:"shell"`
	User      string `json:"user"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	InvokedBy string `json:"invoked_by"`
	CreatedAt string `json:"created_at"`
}

type terminalRelayPending struct {
	Sessions []terminalRelaySession `json:"sessions"`
}

// terminalRelay serves sessions that arrive through the MaidCafe cloud instead
// of this host's own listener, so a browser can attach to a daemon behind NAT.
// The daemon long-polls the cloud for pickups and then dials one outbound
// WebSocket per accepted session; both legs of the browser connection ride that
// socket, and the daemon never listens for it.
type terminalRelay struct {
	publisher *atomic.Pointer[CloudPublisher]
	terminal  *terminalManager
	policy    func() config.TerminalConfig
	logger    *slog.Logger
}

// newTerminalRelay returns nil when the relay cannot run: there is no cloud
// publisher to poll or no session manager to serve from. The caller checks
// terminal.relay.enabled before asking for one.
func newTerminalRelay(publisher *atomic.Pointer[CloudPublisher], terminal *terminalManager, policy func() config.TerminalConfig, logger *slog.Logger) *terminalRelay {
	if publisher == nil || publisher.Load() == nil || terminal == nil || policy == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &terminalRelay{publisher: publisher, terminal: terminal, policy: policy, logger: logger}
}

// Run polls the cloud for relayed sessions until ctx is cancelled, starting
// with an immediate poll. A failed pickup backs off before retrying, so an
// unreachable cloud stays a warn line instead of a flood. The policy is read
// per iteration, so a reload can turn the relay off without a restart.
func (r *terminalRelay) Run(ctx context.Context) {
	for {
		policy := r.policy()
		if !policy.Relay.Enabled {
			if !terminalRelayWait(ctx, terminalRelayPollBackoff) {
				return
			}
			continue
		}
		sessions, err := r.poll(ctx, policy)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logger.Warn("terminal relay poll failed", "error", err)
			if !terminalRelayWait(ctx, terminalRelayPollBackoff) {
				return
			}
			continue
		}
		for _, session := range sessions {
			session := session
			go r.serve(ctx, session)
		}
	}
}

// poll holds one pickup long-poll: the cloud answers immediately when a
// session is waiting and holds the request for the full wait otherwise.
func (r *terminalRelay) poll(ctx context.Context, policy config.TerminalConfig) ([]terminalRelaySession, error) {
	publisher := r.publisher.Load()
	if publisher == nil {
		return nil, fmt.Errorf("cloud publisher is not configured")
	}
	wait := terminalRelayPollWait(policy)
	query := url.Values{"wait": {wait.String()}, "limit": {"1"}}
	var pending terminalRelayPending
	if err := publisher.requestWithTimeout(ctx, wait+terminalRelayPollGrace, "GET",
		"/terminal/requests/pending?"+query.Encode(), nil, &pending); err != nil {
		return nil, err
	}
	return pending.Sessions, nil
}

// serve runs one handed-over session on its own goroutine. It dials the cloud's
// agent socket first: every refusal after that travels in-band on the socket,
// because the cloud has already paired it with the browser that is waiting for
// an answer. Only a session that passes the daemon's own gates starts a shell.
func (r *terminalRelay) serve(ctx context.Context, handed terminalRelaySession) {
	policy := r.policy()
	publisher := r.publisher.Load()
	if publisher == nil {
		return
	}
	target, secret, err := publisher.terminalAgentTarget(handed.ID)
	if err != nil {
		r.logger.Warn("terminal relay agent socket unavailable", "session", handed.ID, "error", err)
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, terminalRelayDialTimeout)
	// The daemon is not a browser: it authenticates with the same daemon secret
	// as every other cloud call, and it sends no Origin header.
	conn, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}},
	})
	cancel()
	if err != nil {
		r.logger.Warn("terminal relay agent dial failed", "session", handed.ID, "error", err)
		return
	}
	reject := func(status websocket.StatusCode, message string) {
		// The error frame is the only way to tell the waiting browser why no
		// shell appeared; the cloud forwards it and closes the other leg.
		writeTerminalText(ctx, conn, terminalErrorFrame{Type: "error", Message: message})
		_ = conn.Close(status, terminalCloseReason(message))
	}
	if !policy.Relay.Enabled {
		reject(websocket.StatusPolicyViolation, "terminal relay disabled")
		return
	}
	if !terminalRelayIdentityAllowed(policy, handed.InvokedBy) {
		r.logger.Warn("terminal relay session rejected",
			"session", handed.ID, "identity", handed.InvokedBy, "reason", "identity not allowed")
		reject(websocket.StatusPolicyViolation, "terminal identity not allowed")
		return
	}
	request, err := r.terminal.Resolve(policy, terminalRequest{
		Shell: handed.Shell,
		User:  handed.User,
		Cols:  handed.Cols,
		Rows:  handed.Rows,
	})
	switch {
	case errors.Is(err, errTerminalShellNotAllowed):
		reject(websocket.StatusPolicyViolation, "terminal shell not allowed")
		return
	case errors.Is(err, errTerminalUserNotAllowed):
		reject(websocket.StatusPolicyViolation, "terminal user not allowed")
		return
	case err != nil:
		reject(websocket.StatusPolicyViolation, err.Error())
		return
	}
	if r.terminal.Count() >= terminalSessionCap(policy) {
		reject(websocket.StatusPolicyViolation, "too many terminal sessions")
		return
	}
	conn.SetReadLimit(terminalMaxInputFrame)
	remote := "cloud:" + handed.InvokedBy
	session, err := r.terminal.Open(policy, request, remote)
	if err != nil {
		r.terminal.recordStartFailure(remote, err)
		reject(websocket.StatusInternalError, err.Error())
		return
	}
	reason := runTerminalSession(ctx, session, conn, policy)
	// The manager's own log names the daemon's session; this line ties the end
	// back to the cloud's session id and the identity that asked for it.
	r.logger.Info("terminal relay session ended",
		"session", handed.ID, "identity", handed.InvokedBy, "reason", reason)
}

// terminalRelayIdentityAllowed reports whether the identity the cloud
// forwarded may open a relayed session. An empty allowlist accepts any
// identity, which still means any identity the cloud chose to authorize.
func terminalRelayIdentityAllowed(policy config.TerminalConfig, identity string) bool {
	if len(policy.Relay.Users) == 0 {
		return true
	}
	for _, allowed := range policy.Relay.Users {
		if allowed == identity {
			return true
		}
	}
	return false
}

// terminalRelayPollWait resolves the pickup hold; zero means the default.
func terminalRelayPollWait(policy config.TerminalConfig) time.Duration {
	if policy.Relay.PollWait <= 0 {
		return config.TerminalRelayDefaultPollWait
	}
	return policy.Relay.PollWait
}

// terminalRelayWait sleeps for d and reports false when ctx ended first.
func terminalRelayWait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
