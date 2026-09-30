package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Terminal session bounds and timings. The idle check runs on its own ticker
// because it must fire without any traffic, so it cannot ride the read loop.
const (
	terminalMaxInputFrame = 32 << 10 // client frame cap (Conn.SetReadLimit)
	terminalReadBuffer    = 32 << 10 // per PTY read
	terminalOutputQueue   = 64       // client-bound chunks; the PTY reader blocks when full
	terminalWriteTimeout  = 30 * time.Second
	terminalPingInterval  = 20 * time.Second
	terminalPingTimeout   = 10 * time.Second
	terminalIdleCheck     = 15 * time.Second
	terminalKillGrace     = 5 * time.Second
	// terminalDrainGrace bounds how long the process reaper waits for the PTY
	// reader after the shell exits, so the last output chunk is queued before
	// the exit frame is emitted. PTY reads report EOF as soon as the child is
	// gone on Linux and macOS, so this is a safety net, not a delay.
	terminalDrainGrace  = 2 * time.Second
	terminalDefaultCols = 80
	terminalDefaultRows = 24
	terminalMinCols     = 1
	terminalMaxCols     = 1000
	terminalMinRows     = 1
	terminalMaxRows     = 500
)

var (
	errTerminalShellNotAllowed = errors.New("terminal shell not allowed")
	errTerminalUserNotAllowed  = errors.New("terminal user not allowed")
	errTerminalTooMany         = errors.New("too many terminal sessions")
)

// Fixed terminal close reasons. They land in the audit entry's error field and
// in the exit frame, so a client can tell a normal shell exit (empty reason)
// from a policy-driven close.
const (
	terminalReasonIdle        = "idle timeout"
	terminalReasonLifetime    = "lifetime exceeded"
	terminalReasonClient      = "client closed"
	terminalReasonUnreachable = "client unreachable"
	terminalReasonShutdown    = "daemon shutdown"
	terminalReasonStartFailed = "shell start failed"
	terminalReasonProtocol    = "protocol error"
)

// terminalRequest is the resolved session request (defaults applied).
type terminalRequest struct {
	Shell string
	User  string // empty = the daemon's own account
	Cols  int
	Rows  int
}

// terminalSession is one live shell attached to a PTY master. The PTY reader
// is the only writer to [out], the process reaper is the only closer of
// [done], and Close drives the shutdown of both.
type terminalSession struct {
	id     string
	remote string
	shell  string
	user   string

	cmd     *exec.Cmd
	pty     *os.File
	manager *terminalManager

	out  chan []byte
	done chan struct{}
	// readDone is closed when the PTY reader returns, i.e. every byte the
	// shell wrote is queued in [out].
	readDone chan struct{}
	// closing is closed by Close, so a reader blocked on a full [out] queue
	// never outlives its session.
	closing chan struct{}

	started  time.Time
	activity atomic.Int64 // unix nanos of last input or output

	mu       sync.Mutex
	cols     int
	rows     int
	finished bool
	closed   bool
	reason   string // close reason, set before done is closed
	exitCode int
}

// terminalManager owns every live session and caps how many may run at once.
// The map is guarded by [mu]; sessions take it only to enter and leave.
type terminalManager struct {
	logger   *slog.Logger
	audit    *AuditLogger
	mu       sync.Mutex
	sessions map[string]*terminalSession
}

func newTerminalManager(logger *slog.Logger, audit *AuditLogger) *terminalManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &terminalManager{logger: logger, audit: audit, sessions: make(map[string]*terminalSession)}
}

// terminalSessionCap resolves the effective concurrent-session cap; an unset
// (zero) maxSessions means the documented default.
func terminalSessionCap(policy config.TerminalConfig) int {
	if policy.MaxSessions <= 0 {
		return config.TerminalDefaultMaxSessions
	}
	return policy.MaxSessions
}

// Count reports how many sessions are live, for the pre-upgrade rejection.
func (m *terminalManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Resolve applies the policy defaults and allowlists to a client's request.
// It runs before the WebSocket upgrade so a rejection is a plain HTTP
// response.
func (m *terminalManager) Resolve(policy config.TerminalConfig, req terminalRequest) (terminalRequest, error) {
	if req.Shell == "" && len(policy.Shells) > 0 {
		req.Shell = policy.Shells[0]
	}
	allowed := false
	for _, shell := range policy.Shells {
		if shell == req.Shell {
			allowed = true
			break
		}
	}
	if !allowed {
		return req, errTerminalShellNotAllowed
	}
	if req.User != "" {
		allowed = false
		for _, name := range policy.Users {
			if name == req.User {
				allowed = true
				break
			}
		}
		if !allowed {
			return req, errTerminalUserNotAllowed
		}
	}
	if req.Cols == 0 {
		req.Cols = terminalDefaultCols
	}
	if req.Rows == 0 {
		req.Rows = terminalDefaultRows
	}
	return req, nil
}

// Open starts an allowlisted login shell on a PTY and registers the session.
// It rejects with errTerminalTooMany when the policy's cap is reached.
func (m *terminalManager) Open(policy config.TerminalConfig, req terminalRequest, remote string) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= terminalSessionCap(policy) {
		return nil, errTerminalTooMany
	}
	cmd := buildTerminalCommand(policy, req)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)})
	if err != nil {
		if errors.Is(err, pty.ErrUnsupported) {
			return nil, fmt.Errorf("terminal unsupported on this platform: %w", err)
		}
		return nil, fmt.Errorf("start shell: %w", err)
	}
	started := time.Now()
	session := &terminalSession{
		id:       uuid.NewString(),
		remote:   remote,
		shell:    req.Shell,
		user:     req.User,
		cmd:      cmd,
		pty:      master,
		manager:  m,
		out:      make(chan []byte, terminalOutputQueue),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
		closing:  make(chan struct{}),
		started:  started,
		cols:     req.Cols,
		rows:     req.Rows,
	}
	session.activity.Store(started.UnixNano())
	m.sessions[session.id] = session
	go session.readOutput()
	go session.reap()
	m.logger.Info("terminal session opened",
		"session", session.id, "shell", req.Shell, "user", req.User,
		"remote", remote, "cols", req.Cols, "rows", req.Rows)
	return session, nil
}

// recordStartFailure reports a session that never opened. There is no session
// to close, so the audit entry is written here instead.
func (m *terminalManager) recordStartFailure(remote string, err error) {
	m.audit.Record(auditEntry{
		Timestamp: time.Now().UTC(),
		Name:      "terminal",
		Source:    "terminal",
		InvokedBy: remote,
		ExitCode:  -1,
		Error:     fmt.Sprintf("%s: %v", terminalReasonStartFailed, err),
	})
	m.logger.Warn("terminal session failed to start", "remote", remote, "error", err)
}

// CloseAll ends every live session. Hijacked connections are invisible to
// http.Server.Shutdown, so the daemon has to close terminals itself.
func (m *terminalManager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, session := range sessions {
		wg.Add(1)
		go func(session *terminalSession) {
			defer wg.Done()
			session.Close(terminalReasonShutdown)
		}(session)
	}
	wg.Wait()
}

// buildTerminalCommand mirrors buildRunCommand: the shell runs directly for
// the daemon's own account and through `sudo -H -u <user>` otherwise, with
// KEY=VALUE entries passed as sudo assignments so env_reset still applies.
// No caller input reaches argv.
func buildTerminalCommand(policy config.TerminalConfig, req terminalRequest) *exec.Cmd {
	cwd := policy.Cwd
	if cwd == "" {
		if req.User == "" {
			cwd, _ = os.UserHomeDir() // "" resolves to "/" below
		} else if account, err := user.Lookup(req.User); err == nil {
			cwd = account.HomeDir
		}
	}
	if cwd == "" {
		cwd = "/"
	}
	env := append([]string{"TERM=xterm-256color"}, policy.Env...)
	if req.User == "" {
		cmd := exec.Command(req.Shell, "-l")
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), env...)
		return cmd
	}
	argv := append([]string{"sudo", "-H", "-u", req.User}, env...)
	argv = append(argv, req.Shell, "-l")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	return cmd
}

// currentTerminalUser names the account a session without a run-as user runs
// as; used for the hello frame, so it is best-effort.
func currentTerminalUser() string {
	if account, err := user.Current(); err == nil {
		return account.Username
	}
	return ""
}

func (s *terminalSession) ID() string    { return s.id }
func (s *terminalSession) Shell() string { return s.shell }
func (s *terminalSession) User() string  { return s.user }

func (s *terminalSession) Cols() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols
}

func (s *terminalSession) Rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// Output is the client-bound queue of raw PTY chunks.
func (s *terminalSession) Output() <-chan []byte { return s.out }

// Done is closed once the shell exited (or Close gave up on it).
func (s *terminalSession) Done() <-chan struct{} { return s.done }

// Touch records session activity so the idle check does not fire while the
// client and the shell are exchanging bytes.
func (s *terminalSession) Touch() { s.activity.Store(time.Now().UnixNano()) }

// Idle reports how long the session has been quiet in both directions.
func (s *terminalSession) Idle() time.Duration {
	return time.Since(time.Unix(0, s.activity.Load()))
}

// Started is the session's start time, for the absolute-lifetime cap.
func (s *terminalSession) Started() time.Time { return s.started }

// Write sends keystrokes to the PTY master.
func (s *terminalSession) Write(p []byte) (int, error) {
	n, err := s.pty.Write(p)
	if err != nil {
		return n, fmt.Errorf("write to terminal: %w", err)
	}
	s.Touch()
	return n, nil
}

// Resize applies a new PTY size.
func (s *terminalSession) Resize(cols, rows int) error {
	if err := pty.Setsize(s.pty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		return fmt.Errorf("resize terminal: %w", err)
	}
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	s.Touch()
	return nil
}

// Exit reports the shell's exit code and the close reason ("" for a normal
// shell exit).
func (s *terminalSession) Exit() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.reason
}

// Close ends the session and is idempotent: the first caller wins, later ones
// return immediately.
func (s *terminalSession) Close(reason string) { s.manager.closeSession(s, reason) }

func (m *terminalManager) closeSession(s *terminalSession, reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()

	close(s.closing)
	// A reaped child's pid may already belong to another process, so the
	// group is only signalled while the shell is still unreaped.
	select {
	case <-s.done:
	default:
		terminateTerminal(s.cmd)
		if !waitTerminal(s.done, terminalKillGrace) {
			killTerminal(s.cmd)
			waitTerminal(s.done, terminalKillGrace)
		}
	}
	// A platform that never reports the shell's exit still has to release the
	// readers waiting on Done, so the forced finish is unconditional.
	s.finish(s.currentExitCode())
	_ = s.pty.Close()

	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()

	exitCode, reasonText := s.Exit()
	m.audit.Record(auditEntry{
		Timestamp:   time.Now().UTC(),
		Name:        "terminal",
		DisplayName: s.id,
		Source:      "terminal",
		InvokedBy:   s.remote,
		OK:          exitCode == 0 && reasonText == "",
		ExitCode:    exitCode,
		DurationMS:  time.Since(s.started).Milliseconds(),
		Error:       reasonText,
	})
	m.logger.Info("terminal session closed",
		"session", s.id, "remote", s.remote, "exit_code", exitCode,
		"reason", reasonText, "duration_ms", time.Since(s.started).Milliseconds())
}

// readOutput copies PTY output into the client-bound queue. A blocked send is
// deliberate backpressure: the shell stops producing instead of losing bytes.
func (s *terminalSession) readOutput() {
	defer close(s.readDone)
	defer close(s.out)
	buffer := make([]byte, terminalReadBuffer)
	for {
		n, err := s.pty.Read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			select {
			case s.out <- chunk:
				s.Touch()
			case <-s.closing:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// reap waits for the shell and releases Done. It lets the PTY reader queue the
// last output first, so the exit frame always follows the shell's output.
func (s *terminalSession) reap() {
	code := terminalExitCode(s.cmd.Wait())
	select {
	case <-s.readDone:
	case <-time.After(terminalDrainGrace):
	}
	s.finish(code)
}

// finish records the exit code and releases Done exactly once, so a reaper
// racing a forced close cannot close the channel twice.
func (s *terminalSession) finish(code int) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.exitCode = code
	s.mu.Unlock()
	close(s.done)
}

func (s *terminalSession) currentExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// terminalExitCode maps a Wait error to a shell exit code (-1 for a signal).
func terminalExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// waitTerminal waits for ch to close, reporting whether it did within d.
func waitTerminal(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}
