package daemon

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// terminalProtocolVersion is the wire protocol version announced in the hello
// frame. Clients must reject an unknown version instead of guessing.
const terminalProtocolVersion = "v1"

// terminalSubprotocolPrefix carries the credential in a WebSocket handshake:
// browsers cannot set an Authorization header on `new WebSocket(...)`, but
// they can offer a subprotocol token.
const terminalSubprotocolPrefix = "maidcafe.terminal."

// terminalCGNAT is Tailscale's address range, which a tailnet listener serves
// and which is neither private nor link-local by net.IP's definition.
var terminalCGNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// terminalHelloFrame is the first frame of every session. User names the
// account the shell actually runs as ("" in the request means the daemon's
// own account).
type terminalHelloFrame struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	Session string `json:"session"`
	Shell   string `json:"shell"`
	User    string `json:"user"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// terminalExitFrame reports why the session ended. Reason is empty for a
// normal shell exit and otherwise the daemon's close reason.
type terminalExitFrame struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

type terminalErrorFrame struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// terminalResizeFrame is the only text frame a client sends. The dimensions
// are pointers so a missing field is a protocol error rather than a resize to
// zero.
type terminalResizeFrame struct {
	Type string `json:"type"`
	Cols *int   `json:"cols"`
	Rows *int   `json:"rows"`
}

// terminalWriter adapts gin's response writer for coder/websocket.
//
// Hijack: coder/websocket reaches the raw connection through http.Hijacker,
// which the embedded http.ResponseWriter interface does not expose. No
// deadline fixup is needed here: net/http clears the request deadlines itself
// when hijacking (net/http's conn.hijackLocked calls rwc.SetDeadline(zero)), so
// the daemon's requestTimeout does not cap a session.
//
// WriteHeaderNow: gin buffers the status handed to WriteHeader and only flushes
// it when that method is called. coder/websocket flushes the handshake response
// through it before hijacking, so without this forwarding the 101 response
// never reaches the client and the handshake hangs until it times out.
type terminalWriter struct{ http.ResponseWriter }

func (w terminalWriter) WriteHeaderNow() {
	if writer, ok := w.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		writer.WriteHeaderNow()
	}
}

func (w terminalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

// handleTerminal serves the opt-in interactive shell over WebSocket. It is
// registered without authorizeMetrics because a browser cannot send an
// Authorization header on a WebSocket handshake: the credential arrives as a
// Sec-WebSocket-Protocol token instead, and a middleware 401 would run before
// that token is read.
func (a *App) handleTerminal(c *gin.Context) {
	policy := a.rt.Load().terminal
	if !policy.Enabled {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal disabled"})
		return
	}
	if !terminalSupported() {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"ok": false, "error": "terminal unsupported on this platform"})
		return
	}
	if !terminalRemoteAllowed(policy, c.Request.RemoteAddr) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal remote access is disabled"})
		return
	}
	secret, ok := terminalCredential(c.Request)
	if expected := terminalExpectedSecret(policy, a.cfg.MetricsSecret); !ok || expected == "" ||
		subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return
	}
	request, status, err := terminalQueryRequest(c)
	if err != nil {
		c.AbortWithStatusJSON(status, gin.H{"ok": false, "error": err.Error()})
		return
	}
	request, err = a.terminal.Resolve(policy, request)
	switch {
	case errors.Is(err, errTerminalShellNotAllowed):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal shell not allowed"})
		return
	case errors.Is(err, errTerminalUserNotAllowed):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "terminal user not allowed"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if a.terminal.Count() >= terminalSessionCap(policy) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"ok": false, "error": "too many terminal sessions"})
		return
	}
	conn, err := websocket.Accept(terminalWriter{c.Writer}, c.Request, &websocket.AcceptOptions{
		// Echoing the caller's own offered token makes the credential visible
		// in the handshake response, which some clients require.
		Subprotocols:    offeredSubprotocols(c.Request),
		OriginPatterns:  policy.AllowedOrigins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// websocket.Accept writes the response itself (403 for a rejected
		// Origin), and there is no connection left to answer on.
		return
	}
	if _, offered := terminalSubprotocolSecret(c.Request.Header.Get("Sec-WebSocket-Protocol")); offered && conn.Subprotocol() == "" {
		// The client authenticated with a token but the subprotocol was not
		// negotiated: nothing to answer, so drop the connection.
		conn.CloseNow()
		return
	}
	conn.SetReadLimit(terminalMaxInputFrame)
	session, err := a.terminal.Open(policy, request, c.Request.RemoteAddr)
	if err != nil {
		a.terminal.recordStartFailure(c.Request.RemoteAddr, err)
		writeTerminalText(c.Request.Context(), conn, terminalErrorFrame{Type: "error", Message: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "terminal start failed")
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		terminalPump(ctx, session, conn, policy)
		cancel()
	}()
	terminalReadLoop(ctx, conn, session)
	// The read loop ends either because the shell exited (the pump closed the
	// connection after the exit frame) or because the client went away first;
	// either way the session must be released, and only the first close
	// attributes the reason.
	closeReason := terminalReasonClient
	if terminalDone(session) {
		closeReason = ""
	}
	session.Close(closeReason)
	// Wait for the pump so the exit frame is flushed before the handler
	// returns and the request context is torn down.
	<-writeDone
}

// terminalPump is the only writer on the connection. It forwards PTY output,
// keeps the socket alive with pings, enforces the idle and lifetime caps, and
// emits the exit frame.
func terminalPump(ctx context.Context, session *terminalSession, conn *websocket.Conn, policy config.TerminalConfig) {
	writeTerminalText(ctx, conn, terminalHelloFrame{
		Type:    "hello",
		Version: terminalProtocolVersion,
		Session: session.ID(),
		Shell:   session.Shell(),
		User:    terminalSessionUser(session),
		Cols:    session.Cols(),
		Rows:    session.Rows(),
	})
	ping := time.NewTicker(terminalPingInterval)
	defer ping.Stop()
	idle := time.NewTicker(terminalIdleCheck)
	defer idle.Stop()
	output := session.Output()
	for {
		// An ended session outranks buffered output: a chunk read now would
		// race the exit frame, while the drain below still delivers it.
		select {
		case <-session.Done():
			terminalExit(ctx, conn, session, output)
			return
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "daemon shutdown")
			return
		default:
		}
		select {
		case <-session.Done():
			terminalExit(ctx, conn, session, output)
			return
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "daemon shutdown")
			return
		case chunk, ok := <-output:
			if !ok {
				// The PTY reader ended; keep serving pings and the idle check
				// until the shell's exit is reported.
				output = nil
				continue
			}
			if err := writeTerminalChunk(ctx, conn, session, chunk); err != nil {
				session.Close(terminalReasonUnreachable)
				return
			}
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, terminalPingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				session.Close(terminalReasonUnreachable)
				return
			}
		case <-idle.C:
			if policy.IdleTimeout > 0 && session.Idle() > policy.IdleTimeout {
				session.Close(terminalReasonIdle)
			}
			if policy.MaxLifetime > 0 && time.Since(session.Started()) > policy.MaxLifetime {
				session.Close(terminalReasonLifetime)
			}
		}
	}
}

// terminalExit flushes what the shell wrote before it died and reports the end
// of the session, so the exit frame is always the last frame.
func terminalExit(ctx context.Context, conn *websocket.Conn, session *terminalSession, output <-chan []byte) {
	if !drainTerminalOutput(ctx, conn, session, output) {
		// The connection is gone; there is nobody left to report to.
		return
	}
	code, reason := session.Exit()
	writeTerminalText(ctx, conn, terminalExitFrame{Type: "exit", Code: code, Reason: reason})
	status := websocket.StatusNormalClosure
	if reason == terminalReasonProtocol {
		status = websocket.StatusPolicyViolation
	}
	_ = conn.Close(status, terminalCloseReason(reason))
}

// drainTerminalOutput delivers output the shell produced before it exited, so
// the exit frame never overtakes the last bytes. It reports false when a write
// failed. The PTY reader closes the queue before the exit is released, so an
// empty closed queue means every byte was delivered.
func drainTerminalOutput(ctx context.Context, conn *websocket.Conn, session *terminalSession, output <-chan []byte) bool {
	for {
		select {
		case chunk, ok := <-output:
			if !ok {
				return true
			}
			if err := writeTerminalChunk(ctx, conn, session, chunk); err != nil {
				return false
			}
		default:
			// The reader may still be running (a forced close after the kill
			// grace); report the exit rather than block the pump forever.
			return true
		}
	}
}

// terminalReadLoop applies client frames to the session until the connection
// ends or the client breaks the protocol.
func terminalReadLoop(ctx context.Context, conn *websocket.Conn, session *terminalSession) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := session.Write(data); err != nil {
				session.Close(terminalReasonUnreachable)
				return
			}
		case websocket.MessageText:
			var frame terminalResizeFrame
			if err := json.Unmarshal(data, &frame); err != nil || frame.Type != "resize" {
				writeTerminalText(ctx, conn, terminalErrorFrame{Type: "error", Message: "unsupported control frame"})
				session.Close(terminalReasonProtocol)
				return
			}
			if err := terminalResize(session, frame); err != nil {
				writeTerminalText(ctx, conn, terminalErrorFrame{Type: "error", Message: err.Error()})
				session.Close(terminalReasonProtocol)
				return
			}
		default:
			writeTerminalText(ctx, conn, terminalErrorFrame{Type: "error", Message: "unsupported message type"})
			session.Close(terminalReasonProtocol)
			return
		}
	}
}

func terminalResize(session *terminalSession, frame terminalResizeFrame) error {
	if frame.Cols == nil || *frame.Cols < terminalMinCols || *frame.Cols > terminalMaxCols {
		return fmt.Errorf("resize cols must be an integer between %d and %d", terminalMinCols, terminalMaxCols)
	}
	if frame.Rows == nil || *frame.Rows < terminalMinRows || *frame.Rows > terminalMaxRows {
		return fmt.Errorf("resize rows must be an integer between %d and %d", terminalMinRows, terminalMaxRows)
	}
	return session.Resize(*frame.Cols, *frame.Rows)
}

// writeTerminalChunk writes one PTY chunk. A stalled client hits the write
// timeout, which the library answers by closing the connection.
func writeTerminalChunk(ctx context.Context, conn *websocket.Conn, session *terminalSession, chunk []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageBinary, chunk); err != nil {
		return err
	}
	session.Touch()
	return nil
}

func writeTerminalText(ctx context.Context, conn *websocket.Conn, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	_ = conn.Write(writeCtx, websocket.MessageText, payload)
}

// terminalCredential reads the terminal credential from the handshake. Only
// headers are consulted, matching the daemon-wide rule that secrets never
// travel in the query string or a cookie.
func terminalCredential(r *http.Request) (string, bool) {
	if secret, ok := terminalSubprotocolSecret(r.Header.Get("Sec-WebSocket-Protocol")); ok {
		return secret, true
	}
	return bearerSecret(r)
}

// terminalSubprotocolSecret decodes the first `maidcafe.terminal.<base64url>`
// token of a Sec-WebSocket-Protocol header. The encoding is unpadded base64url
// (a subprotocol token cannot contain "=").
func terminalSubprotocolSecret(header string) (string, bool) {
	for _, token := range strings.Split(header, ",") {
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, terminalSubprotocolPrefix) {
			continue
		}
		raw := strings.TrimPrefix(token, terminalSubprotocolPrefix)
		if raw == "" {
			return "", false
		}
		secret, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return "", false
		}
		return string(secret), true
	}
	return "", false
}

// offeredSubprotocols echoes the client's own terminal token back so the
// handshake negotiates it. Header-authenticated clients offer none and
// negotiate the empty subprotocol.
func offeredSubprotocols(r *http.Request) []string {
	for _, token := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, terminalSubprotocolPrefix) {
			return []string{token}
		}
	}
	return nil
}

// terminalExpectedSecret resolves the accepted credential: the dedicated
// terminal secret when configured, the metrics secret otherwise.
func terminalExpectedSecret(policy config.TerminalConfig, metricsSecret string) string {
	if policy.Secret != "" {
		return policy.Secret
	}
	return metricsSecret
}

// terminalRemoteAllowed reports whether a peer may open a session. Only the
// transport peer address is used: X-Forwarded-For would let any caller claim a
// loopback source.
func terminalRemoteAllowed(policy config.TerminalConfig, remoteAddr string) bool {
	if policy.AllowRemote {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || terminalCGNAT.Contains(ip)
}

// terminalQueryRequest reads the session request from the query string.
func terminalQueryRequest(c *gin.Context) (terminalRequest, int, error) {
	request := terminalRequest{
		Shell: strings.TrimSpace(c.Query("shell")),
		User:  strings.TrimSpace(c.Query("user")),
	}
	cols, err := terminalQueryDimension(c.Query("cols"), terminalMinCols, terminalMaxCols)
	if err != nil {
		return request, http.StatusBadRequest, fmt.Errorf("cols must be an integer between %d and %d", terminalMinCols, terminalMaxCols)
	}
	rows, err := terminalQueryDimension(c.Query("rows"), terminalMinRows, terminalMaxRows)
	if err != nil {
		return request, http.StatusBadRequest, fmt.Errorf("rows must be an integer between %d and %d", terminalMinRows, terminalMaxRows)
	}
	request.Cols, request.Rows = cols, rows
	return request, 0, nil
}

// terminalQueryDimension parses an optional dimension; an absent value means
// "use the default" and is reported as 0 for Resolve to fill in.
func terminalQueryDimension(raw string, min, max int) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("must be between %d and %d", min, max)
	}
	return parsed, nil
}

// terminalSessionUser names the account the shell runs as, falling back to the
// daemon's own account when the request did not select one.
func terminalSessionUser(session *terminalSession) string {
	if name := session.User(); name != "" {
		return name
	}
	return currentTerminalUser()
}

func terminalDone(session *terminalSession) bool {
	select {
	case <-session.Done():
		return true
	default:
		return false
	}
}

// terminalCloseReason bounds the close-frame reason so the library cannot
// reject it for length.
func terminalCloseReason(reason string) string {
	if reason == "" {
		return "shell exited"
	}
	if len(reason) > 100 {
		return reason[:100]
	}
	return reason
}
