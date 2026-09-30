package handler

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Cloud-relayed terminal wire constants. The frames are the daemon's v1
// terminal frames passed through verbatim; the cloud never parses PTY bytes.
const (
	// terminalSubprotocolPrefix carries the browser credential in a WebSocket
	// handshake: browsers cannot set an Authorization header on `new
	// WebSocket(...)`, but they can offer a subprotocol token.
	terminalSubprotocolPrefix = "maidcafe.terminal."
	// terminalMaxFrame is the largest frame forwarded; coder/websocket closes
	// the connection with 1009 past the read limit.
	terminalMaxFrame = 1 << 20
	// terminalMaxWait caps the daemon long-poll hold (the route's `wait`).
	terminalMaxWait = 30 * time.Second
	// terminalPairTimeout is how long one socket waits for its peer before the
	// pairing is abandoned (the contract's 90s attach window).
	terminalPairTimeout = 90 * time.Second
	// terminalWriteTimeout bounds one forwarded frame's write.
	terminalWriteTimeout = 30 * time.Second
	// terminalFinishTimeout bounds the end-state write after a socket ends.
	terminalFinishTimeout = 5 * time.Second
	// terminalMinLimit/terminalMaxLimit bound the pickup `limit` parameter.
	terminalMinLimit = 1
	terminalMaxLimit = 8

	terminalCloseReason = "socket closed"
)

type terminalRole int

const (
	terminalRoleBrowser terminalRole = iota
	terminalRoleAgent
)

// terminalPair is the in-memory pairing of one session's browser and agent
// sockets. Each socket has exactly one reader and one writer across the two
// pump goroutines, so coder/websocket's single-reader/single-writer rule holds.
type terminalPair struct {
	id string

	mu      sync.Mutex
	browser *websocket.Conn
	agent   *websocket.Conn
	ready   chan struct{}
	readyOn sync.Once

	bytesIn  atomic.Int64 // browser -> daemon (keystrokes)
	bytesOut atomic.Int64 // daemon -> browser (PTY output)

	exitMu     sync.Mutex
	exitSeen   bool
	exitCode   int
	exitReason string

	closeOn  sync.Once
	finishOn sync.Once
	// pumps counts in-flight frame pumps, so the end state is stored only
	// after both byte counters are final.
	pumps sync.WaitGroup
}

// terminalRegistry pairs sockets by session id. In-memory only: the cloud must
// be single-instance (or sticky-route by session id) for pairing to work.
type terminalRegistry struct {
	mu    sync.Mutex
	pairs map[string]*terminalPair
}

func newTerminalRegistry() *terminalRegistry {
	return &terminalRegistry{pairs: make(map[string]*terminalPair)}
}

// register places conn into its role slot. It reports false when the role is
// already taken (a duplicate socket for the same session).
func (r *terminalRegistry) register(sessionID string, role terminalRole, conn *websocket.Conn) (*terminalPair, bool) {
	r.mu.Lock()
	pair := r.pairs[sessionID]
	if pair == nil {
		pair = &terminalPair{id: sessionID, ready: make(chan struct{})}
		r.pairs[sessionID] = pair
	}
	r.mu.Unlock()

	pair.mu.Lock()
	defer pair.mu.Unlock()
	if role == terminalRoleBrowser {
		if pair.browser != nil {
			return nil, false
		}
		pair.browser = conn
	} else {
		if pair.agent != nil {
			return nil, false
		}
		pair.agent = conn
	}
	if pair.browser != nil && pair.agent != nil {
		pair.readyOn.Do(func() { close(pair.ready) })
	}
	return pair, true
}

func (r *terminalRegistry) release(sessionID string, pair *terminalPair) {
	r.mu.Lock()
	if r.pairs[sessionID] == pair {
		delete(r.pairs, sessionID)
	}
	r.mu.Unlock()
}

// close tears down a live pair (used by the revoke route).
func (r *terminalRegistry) close(sessionID string) {
	r.mu.Lock()
	pair := r.pairs[sessionID]
	r.mu.Unlock()
	if pair != nil {
		pair.shutdown()
	}
}

// wait blocks until both sockets are present, ctx is done, or the timeout
// elapses.
func (p *terminalPair) wait(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.ready:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (p *terminalPair) shutdown() {
	p.closeOn.Do(func() {
		p.mu.Lock()
		browser, agent := p.browser, p.agent
		p.mu.Unlock()
		if browser != nil {
			_ = browser.CloseNow()
		}
		if agent != nil {
			_ = agent.CloseNow()
		}
	})
}

// observeText records the daemon's exit frame so the stored end state carries
// the shell's exit code and reason. It never alters the forwarded bytes.
func (p *terminalPair) observeText(data []byte) {
	var frame struct {
		Type   string `json:"type"`
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &frame); err != nil || frame.Type != "exit" {
		return
	}
	p.exitMu.Lock()
	if !p.exitSeen {
		p.exitSeen = true
		p.exitCode = frame.Code
		p.exitReason = frame.Reason
	}
	p.exitMu.Unlock()
}

func (p *terminalPair) exitState() (int, string, bool) {
	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	return p.exitCode, p.exitReason, p.exitSeen
}

// browserTerminal serves the browser leg of a relayed session. The credential
// is the ticket subprotocol minted by POST /api/daemons/:id/terminal.
func browserTerminal(svc *cloud.Service, cfg *config.Config, reg *terminalRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		daemonID := c.Param("id")
		token, ok := terminalSubprotocolToken(c.Request)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		sessionID, ticket, ok := strings.Cut(token, ".")
		if !ok || sessionID == "" || ticket == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		if _, err := svc.AuthenticateTerminalTicket(c.Request.Context(), daemonID, sessionID, ticket); err != nil {
			serviceStatus(c, err)
			return
		}
		conn, err := websocket.Accept(terminalWriter{c.Writer}, c.Request, &websocket.AcceptOptions{
			Subprotocols:    terminalOfferedSubprotocols(c.Request),
			OriginPatterns:  terminalAllowedOrigins(cfg),
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			// websocket.Accept already wrote its own status (e.g. 403 for a
			// rejected Origin).
			return
		}
		conn.SetReadLimit(terminalMaxFrame)
		serveTerminalPair(c.Request.Context(), svc, reg, sessionID, terminalRoleBrowser, conn)
	}
}

// agentTerminal serves the daemon leg. The daemon authenticates with its cloud
// secret, exactly like its other cloud calls.
func agentTerminal(svc *cloud.Service, reg *terminalRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		sessionID := strings.TrimSpace(c.Query("session"))
		if sessionID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "session is required"})
			return
		}
		if _, err := svc.AttachTerminalAgent(c.Request.Context(), c.Param("id"), secret, sessionID); err != nil {
			serviceStatus(c, err)
			return
		}
		conn, err := websocket.Accept(terminalWriter{c.Writer}, c.Request, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		conn.SetReadLimit(terminalMaxFrame)
		serveTerminalPair(c.Request.Context(), svc, reg, sessionID, terminalRoleAgent, conn)
	}
}

// serveTerminalPair waits for the peer socket, then pumps frames in one
// direction until either socket ends.
func serveTerminalPair(ctx context.Context, svc *cloud.Service, reg *terminalRegistry, sessionID string, role terminalRole, conn *websocket.Conn) {
	pair, ok := reg.register(sessionID, role, conn)
	if !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, "session already attached")
		return
	}
	defer reg.release(sessionID, pair)

	if !pair.wait(ctx, terminalPairTimeout) {
		_ = writeTerminalError(ctx, conn, "terminal peer did not attach")
		_ = conn.Close(websocket.StatusInternalError, "peer not attached")
		finishTerminalPair(pair, svc, "failed", "peer not attached")
		return
	}

	var src, dst *websocket.Conn
	if role == terminalRoleBrowser {
		src, dst = pair.browser, pair.agent
	} else {
		src, dst = pair.agent, pair.browser
	}
	pumpTerminal(ctx, pair, src, dst, role)
	finishTerminalPair(pair, svc, "closed", "")
}

// pumpTerminal forwards every frame verbatim until a socket ends. It never
// coalesces or drops: a slow destination blocks the read, which is exactly the
// backpressure the PTY reader needs.
func pumpTerminal(ctx context.Context, pair *terminalPair, src, dst *websocket.Conn, role terminalRole) {
	pair.pumps.Add(1)
	// LIFO: unblock the peer pump first, then retire this one, so the end
	// state is only recorded after both counters are final.
	defer pair.pumps.Done()
	defer pair.shutdown()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if role == terminalRoleBrowser {
			pair.bytesIn.Add(int64(len(data)))
		} else {
			pair.bytesOut.Add(int64(len(data)))
			if typ == websocket.MessageText {
				pair.observeText(data)
			}
		}
		writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
		err = dst.Write(writeCtx, typ, data)
		cancel()
		if err != nil {
			return
		}
	}
}

// finishTerminalPair records the end state exactly once, even though both
// pump goroutines return. A [reason] of "" falls back to the daemon's exit
// reason, which is empty for a normal shell exit.
//
// The write waits for the peer pump so the byte counters are final, and it is
// retried and then logged rather than dropped: a row left `active` forever
// holds one of the workspace's concurrent-session slots, and a terminal that
// silently stops being openable is hard to trace back to a single lost write.
func finishTerminalPair(pair *terminalPair, svc *cloud.Service, status, reason string) {
	pair.finishOn.Do(func() {
		_ = pair.waitPumps(terminalFinishTimeout)
		code, exitReason, seen := pair.exitState()
		if reason == "" {
			if seen {
				reason = exitReason
			} else {
				reason = terminalCloseReason
			}
		}
		bytesIn := pair.bytesIn.Load()
		bytesOut := pair.bytesOut.Load()
		var err error
		for _, backoff := range []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond} {
			if backoff > 0 {
				time.Sleep(backoff)
			}
			ctx, cancel := context.WithTimeout(context.Background(), terminalFinishTimeout)
			err = svc.FinishTerminalSession(ctx, pair.id, status, code, reason, bytesIn, bytesOut)
			cancel()
			if err == nil {
				return
			}
		}
		slog.Warn("terminal session end state not recorded",
			"session", pair.id, "status", status, "error", err)
	})
}

// waitPumps blocks until no pump is still forwarding frames, so a finished
// pair's byte counters are final before they are stored. It reports whether
// every pump returned within [timeout].
func (p *terminalPair) waitPumps(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		p.pumps.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func writeTerminalError(ctx context.Context, conn *websocket.Conn, message string) error {
	payload, err := json.Marshal(gin.H{"type": "error", "message": message})
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

// createTerminalSession mints a relay ticket for the browser. A user-level API
// credential is rejected: a terminal requires an interactive Solar user.
func createTerminalSession(svc *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if credentialFrom(c) != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "terminal sessions require an interactive user"})
			return
		}
		var in struct {
			Shell string `json:"shell"`
			User  string `json:"user"`
			Cols  int    `json:"cols"`
			Rows  int    `json:"rows"`
		}
		if !parseJSON(c, &in) {
			return
		}
		if in.Cols < 0 || in.Cols > 1000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cols must be between 0 and 1000"})
			return
		}
		if in.Rows < 0 || in.Rows > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rows must be between 0 and 500"})
			return
		}
		out, err := svc.CreateTerminalSession(c.Request.Context(), accountID(c), c.Param("id"), "@"+userHandle(c), in.Shell, in.User, in.Cols, in.Rows)
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) || errors.Is(err, cloud.ErrRateLimited) {
				serviceStatus(c, err)
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

func listTerminalSessions(svc *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if raw := c.Query("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
			limit = parsed
		}
		out, err := svc.ListTerminalSessions(c.Request.Context(), accountID(c), c.Param("id"), limit)
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"sessions": out})
	}
}

func closeTerminalSession(svc *cloud.Service, reg *terminalRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if err := svc.CloseTerminalSession(c.Request.Context(), accountID(c), c.Param("id"), sessionID); err != nil {
			serviceStatus(c, err)
			return
		}
		// Tear down the live sockets, if this instance holds them.
		reg.close(sessionID)
		c.Status(http.StatusNoContent)
	}
}

// listPendingTerminalSessions is the daemon's pickup long-poll. It returns
// leased sessions immediately; only when there are none does it wait for a
// create, then lists again.
func listPendingTerminalSessions(svc *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		daemonID := c.Param("id")
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < terminalMinLimit || parsed > terminalMaxLimit {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
		}
		var wait time.Duration
		if raw := strings.TrimSpace(c.Query("wait")); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil || parsed < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid wait"})
				return
			}
			if parsed > terminalMaxWait {
				parsed = terminalMaxWait
			}
			wait = parsed
		}

		// Register before listing: a session created while this request is
		// listing leaves a wake-up token behind, so the park below returns at
		// once instead of holding a whole poll interval.
		park := svc.TerminalWait(daemonID)
		sessions, err := svc.ListPendingTerminalSessions(c.Request.Context(), daemonID, secret)
		if err != nil {
			serviceStatus(c, err)
			return
		}
		if len(sessions) == 0 && wait > 0 {
			park(c.Request.Context(), wait)
			sessions, err = svc.ListPendingTerminalSessions(c.Request.Context(), daemonID, secret)
			if err != nil {
				serviceStatus(c, err)
				return
			}
		} else {
			// Nothing pending and no wait requested: release the registration
			// rather than leaving an idle channel behind.
			park(c.Request.Context(), 0)
		}
		c.JSON(http.StatusOK, gin.H{"sessions": sessions})
	}
}

func terminalAllowedOrigins(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return cfg.HTTP.AllowedOrigins
}

// terminalSubprotocolToken decodes the first `maidcafe.terminal.<base64url>`
// token of a Sec-WebSocket-Protocol header. The encoding is unpadded base64url
// (a subprotocol token cannot contain "=").
func terminalSubprotocolToken(r *http.Request) (string, bool) {
	for _, token := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, terminalSubprotocolPrefix) {
			continue
		}
		raw := strings.TrimPrefix(token, terminalSubprotocolPrefix)
		if raw == "" {
			return "", false
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return "", false
		}
		return string(decoded), true
	}
	return "", false
}

// terminalOfferedSubprotocols echoes the client's own token back so the
// handshake negotiates it; the daemon socket offers none.
func terminalOfferedSubprotocols(r *http.Request) []string {
	for _, token := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, terminalSubprotocolPrefix) {
			return []string{token}
		}
	}
	return nil
}

// terminalWriter adapts gin's response writer for coder/websocket.
//
// Hijack: net/http installs the server's Read/WriteTimeout deadlines on the raw
// connection and a hijacked connection keeps them, so they are cleared here.
// Without this the relay would be killed 30s into every session by the cloud's
// WriteTimeout. coder/websocket reaches the raw connection through
// http.Hijacker, which the embedded http.ResponseWriter interface does not
// expose.
//
// WriteHeaderNow: gin buffers the status handed to WriteHeader and flushes it
// only when that method is called; coder/websocket flushes the handshake
// through it, so without forwarding the 101 never reaches the client.
type terminalWriter struct{ http.ResponseWriter }

func (w terminalWriter) WriteHeaderNow() {
	if writer, ok := w.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		writer.WriteHeaderNow()
	}
}

func (w terminalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.SetDeadline(time.Time{})
	}
	return conn, rw, err
}
