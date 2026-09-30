package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// terminalToken is the subprotocol token a browser client carries.
func terminalToken(secret string) string {
	return terminalSubprotocolPrefix + base64.RawURLEncoding.EncodeToString([]byte(secret))
}

// terminalDaemon starts a daemon with the terminal enabled and returns the
// app, the endpoint URL, and the audit path it writes to.
func terminalDaemon(t *testing.T, mutate func(*config.DaemonConfig)) (*App, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the daemon has no PTY on Windows")
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	cfg := config.DaemonConfig{
		ID:                "host-1",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		RequestTimeout:    time.Second,
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		AuditPath:         auditPath,
		Runtimes:          []string{"java"},
		ProcessesLimit:    50,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		Terminal: config.TerminalConfig{
			Enabled:     true,
			Shells:      []string{"/bin/sh"},
			MaxSessions: 1,
			IdleTimeout: time.Minute,
			MaxLifetime: time.Hour,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
	})
	return app, "ws://" + app.ListenAddr() + "/api/v1/terminal", auditPath
}

// dialTerminal returns the connection on success and the HTTP status and body
// on a rejected handshake.
func dialTerminal(t *testing.T, ctx context.Context, url string, options *websocket.DialOptions) (*websocket.Conn, int, string) {
	t.Helper()
	conn, resp, err := websocket.Dial(ctx, url, options)
	if err == nil {
		return conn, http.StatusSwitchingProtocols, ""
	}
	if resp == nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	body := ""
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(data)
	}
	return nil, resp.StatusCode, body
}

// terminalClient drives one session from the client side, buffering the binary
// output it reads along the way.
type terminalClient struct {
	t      *testing.T
	ctx    context.Context
	conn   *websocket.Conn
	output strings.Builder
}

func newTerminalClient(t *testing.T, ctx context.Context, conn *websocket.Conn) *terminalClient {
	return &terminalClient{t: t, ctx: ctx, conn: conn}
}

// kind reads frames until it sees a text frame of the given type.
func (c *terminalClient) kind(name string) map[string]any {
	c.t.Helper()
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.t.Fatalf("read %s frame: %v (output %q)", name, err, c.output.String())
		}
		if typ == websocket.MessageBinary {
			c.output.Write(data)
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			c.t.Fatalf("decode text frame %q: %v", data, err)
		}
		if frame["type"] != name {
			c.t.Fatalf("text frame type = %v, want %s (frame %s)", frame["type"], name, data)
		}
		return frame
	}
}

// waitOutput reads frames until the buffered output contains needle.
func (c *terminalClient) waitOutput(needle string) {
	c.t.Helper()
	for !strings.Contains(c.output.String(), needle) {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.t.Fatalf("read output: %v (output %q)", err, c.output.String())
		}
		if typ == websocket.MessageBinary {
			c.output.Write(data)
		}
	}
}

func (c *terminalClient) write(data []byte) {
	c.t.Helper()
	if err := c.conn.Write(c.ctx, websocket.MessageBinary, data); err != nil {
		c.t.Fatalf("write %q: %v", data, err)
	}
}

func (c *terminalClient) resize(cols, rows int) {
	c.t.Helper()
	payload, err := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, payload); err != nil {
		c.t.Fatalf("write resize: %v", err)
	}
}

// closeStatus reads until the connection ends and reports the close code.
func (c *terminalClient) closeStatus() websocket.StatusCode {
	c.t.Helper()
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
		if typ == websocket.MessageBinary {
			c.output.Write(data)
		}
	}
}

func TestTerminalHTTPRejectsBadCredentials(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name    string
		options *websocket.DialOptions
		status  int
	}{
		{name: "no credential", options: nil, status: http.StatusUnauthorized},
		{name: "wrong secret", options: &websocket.DialOptions{
			Subprotocols: []string{terminalToken("wrong-secret")},
		}, status: http.StatusUnauthorized},
		{name: "malformed token", options: &websocket.DialOptions{
			Subprotocols: []string{terminalSubprotocolPrefix + "!!!!"},
		}, status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, status, body := dialTerminal(t, ctx, url, tc.options)
			if conn != nil {
				conn.CloseNow()
				t.Fatal("handshake unexpectedly succeeded")
			}
			if status != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", status, tc.status, body)
			}
		})
	}
}

func TestTerminalHTTPDisabledAndPlatform(t *testing.T) {
	_, url, _ := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.Enabled = false
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn != nil {
		conn.CloseNow()
		t.Fatal("handshake succeeded while the terminal is disabled")
	}
	if status != http.StatusForbidden || !strings.Contains(body, "terminal disabled") {
		t.Fatalf("status = %d, body %q", status, body)
	}
}

func TestTerminalHTTPOriginPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin := http.Header{"Origin": {"http://evil.example"}}

	_, url, _ := terminalDaemon(t, nil)
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
		HTTPHeader:   origin,
	})
	if conn != nil {
		conn.CloseNow()
		t.Fatal("cross-origin handshake succeeded without an allowlist entry")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", status, body)
	}

	_, allowedURL, _ := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.AllowedOrigins = []string{"evil.example"}
	})
	allowed, allowedStatus, allowedBody := dialTerminal(t, ctx, allowedURL, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
		HTTPHeader:   origin,
	})
	if allowed == nil {
		t.Fatalf("allowlisted origin rejected: status %d, body %q", allowedStatus, allowedBody)
	}
	allowed.CloseNow()
}

func TestTerminalHTTPRequestValidation(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name   string
		query  string
		status int
		body   string
	}{
		{name: "unlisted shell", query: "?shell=/bin/zsh", status: http.StatusForbidden, body: "terminal shell not allowed"},
		{name: "unlisted user", query: "?user=nobody", status: http.StatusForbidden, body: "terminal user not allowed"},
		{name: "zero cols", query: "?cols=0", status: http.StatusBadRequest, body: "cols must be an integer"},
		{name: "non-numeric cols", query: "?cols=abcd", status: http.StatusBadRequest, body: "cols must be an integer"},
		{name: "oversized rows", query: "?rows=501", status: http.StatusBadRequest, body: "rows must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, status, body := dialTerminal(t, ctx, url+tc.query, &websocket.DialOptions{
				Subprotocols: []string{terminalToken("metrics-secret")},
			})
			if conn != nil {
				conn.CloseNow()
				t.Fatal("handshake unexpectedly succeeded")
			}
			if status != tc.status || !strings.Contains(body, tc.body) {
				t.Fatalf("status = %d, body %q; want %d containing %q", status, body, tc.status, tc.body)
			}
		})
	}
}

func TestTerminalHTTPSessionEchoAndExit(t *testing.T) {
	_, url, auditPath := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := terminalToken("metrics-secret")
	conn, status, body := dialTerminal(t, ctx, url+"?cols=100&rows=30", &websocket.DialOptions{Subprotocols: []string{token}})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	if got := conn.Subprotocol(); got != token {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, token)
	}
	client := newTerminalClient(t, ctx, conn)
	hello := client.kind("hello")
	sessionID, _ := hello["session"].(string)
	if sessionID == "" {
		t.Fatalf("hello carries no session id: %v", hello)
	}
	if hello["version"] != "v1" || hello["shell"] != "/bin/sh" {
		t.Fatalf("hello = %v", hello)
	}
	if hello["cols"] != float64(100) || hello["rows"] != float64(30) {
		t.Fatalf("hello size = %vx%v, want 100x30", hello["cols"], hello["rows"])
	}
	if hello["user"] != currentTerminalUser() {
		t.Fatalf("hello user = %v, want %q", hello["user"], currentTerminalUser())
	}
	client.write([]byte("echo hello\n"))
	client.waitOutput("hello")
	client.write([]byte("exit\n"))
	exit := client.kind("exit")
	if exit["code"] != float64(0) || exit["reason"] != "" {
		t.Fatalf("exit frame = %v, want code 0 and an empty reason", exit)
	}
	if status := client.closeStatus(); status != websocket.StatusNormalClosure {
		t.Fatalf("close status = %d, want %d", status, websocket.StatusNormalClosure)
	}
	// A shell that exits on its own is a successful run, recorded once.
	line := waitTerminalAudit(t, auditPath, `"display_name":"`+sessionID+`"`)
	if !strings.Contains(line, `"ok":true`) || !strings.Contains(line, `"exit_code":0`) {
		t.Fatalf("audit line = %s", line)
	}
}

func TestTerminalHTTPResize(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url+"?cols=120&rows=40", &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	if hello := client.kind("hello"); hello["cols"] != float64(120) || hello["rows"] != float64(40) {
		t.Fatalf("hello = %v", hello)
	}
	client.resize(100, 30)
	client.write([]byte("stty size\n"))
	client.waitOutput("30 100")
}

// TestTerminalHTTPSurvivesTheHijackDeadline pins that a session outlives the
// daemon's requestTimeout: the session sits idle for three times that budget
// and still serves the next keystroke.
func TestTerminalHTTPSurvivesTheHijackDeadline(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	client.kind("hello")
	time.Sleep(3 * time.Second)
	client.write([]byte("echo alive\n"))
	client.waitOutput("alive")
}

func TestTerminalHTTPHeaderCredential(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer metrics-secret"}},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	if hello := client.kind("hello"); hello["shell"] != "/bin/sh" {
		t.Fatalf("hello = %v", hello)
	}
	if got := conn.Subprotocol(); got != "" {
		t.Fatalf("negotiated subprotocol = %q, want none", got)
	}
}

func TestTerminalHTTPDedicatedSecretOverridesMetricsSecret(t *testing.T) {
	_, url, _ := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.Secret = "terminal-secret"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn != nil {
		conn.CloseNow()
		t.Fatal("the metrics secret was accepted while a dedicated terminal secret is configured")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %q", status, body)
	}
	allowed, allowedStatus, allowedBody := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("terminal-secret")},
	})
	if allowed == nil {
		t.Fatalf("dedicated secret rejected: status %d, body %q", allowedStatus, allowedBody)
	}
	allowed.CloseNow()
}

func TestTerminalHTTPConcurrencyCap(t *testing.T) {
	_, url, _ := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := &websocket.DialOptions{Subprotocols: []string{terminalToken("metrics-secret")}}
	first, status, body := dialTerminal(t, ctx, url, options)
	if first == nil {
		t.Fatalf("first dial failed: status %d, body %q", status, body)
	}
	defer first.CloseNow()
	// The hello frame proves the session is registered, so the next dial sees
	// the cap instead of racing session setup.
	_ = newTerminalClient(t, ctx, first).kind("hello")
	second, secondStatus, secondBody := dialTerminal(t, ctx, url, options)
	if second != nil {
		second.CloseNow()
		t.Fatal("second session accepted beyond maxSessions")
	}
	if secondStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body %q; want 429", secondStatus, secondBody)
	}
}

func TestTerminalHTTPProtocolViolation(t *testing.T) {
	_, url, auditPath := terminalDaemon(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	hello := client.kind("hello")
	sessionID, _ := hello["session"].(string)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"bogus"}`)); err != nil {
		t.Fatal(err)
	}
	report := client.kind("error")
	if report["message"] == "" {
		t.Fatalf("error frame = %v", report)
	}
	exit := client.kind("exit")
	if exit["reason"] != terminalReasonProtocol {
		t.Fatalf("exit frame = %v, want reason %q", exit, terminalReasonProtocol)
	}
	if status := client.closeStatus(); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %d, want %d", status, websocket.StatusPolicyViolation)
	}
	line := waitTerminalAudit(t, auditPath, `"display_name":"`+sessionID+`"`)
	if !strings.Contains(line, terminalReasonProtocol) {
		t.Fatalf("audit line = %s", line)
	}
}

// TestTerminalHTTPStartFailure covers a shell that passes the allowlist but
// cannot be executed: the client gets an error frame and a 1011 close, and the
// attempt still lands in the audit log.
func TestTerminalHTTPStartFailure(t *testing.T) {
	const missing = "/bin/nonexistent-maidcafe-shell"
	_, url, auditPath := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.Shells = []string{missing}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url+"?shell="+missing, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	report := client.kind("error")
	if message, _ := report["message"].(string); message == "" {
		t.Fatalf("error frame = %v", report)
	}
	if closeCode := client.closeStatus(); closeCode != websocket.StatusInternalError {
		t.Fatalf("close status = %d, want %d", closeCode, websocket.StatusInternalError)
	}
	line := waitTerminalAudit(t, auditPath, `"source":"terminal"`)
	if !strings.Contains(line, terminalReasonStartFailed) {
		t.Fatalf("audit line = %s", line)
	}
}

// TestTerminalHTTPIdleTimeout exercises the idle bound: the session's own
// ticker (15s) notices an idle second and closes the shell, reporting the
// reason on the exit frame and in the audit entry.
func TestTerminalHTTPIdleTimeout(t *testing.T) {
	_, url, auditPath := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.IdleTimeout = time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, status, body := dialTerminal(t, ctx, url, &websocket.DialOptions{
		Subprotocols: []string{terminalToken("metrics-secret")},
	})
	if conn == nil {
		t.Fatalf("dial failed: status %d, body %q", status, body)
	}
	defer conn.CloseNow()
	client := newTerminalClient(t, ctx, conn)
	hello := client.kind("hello")
	sessionID, _ := hello["session"].(string)
	exit := client.kind("exit")
	if exit["reason"] != terminalReasonIdle {
		t.Fatalf("exit frame = %v, want reason %q", exit, terminalReasonIdle)
	}
	if status := client.closeStatus(); status != websocket.StatusNormalClosure {
		t.Fatalf("close status = %d, want %d", status, websocket.StatusNormalClosure)
	}
	line := waitTerminalAudit(t, auditPath, `"display_name":"`+sessionID+`"`)
	if !strings.Contains(line, terminalReasonIdle) || !strings.Contains(line, `"ok":false`) {
		t.Fatalf("audit line = %s", line)
	}
}

// waitTerminalAudit waits for an audit line containing needle, so the test
// does not race the handler that records it.
func waitTerminalAudit(t *testing.T, path, needle string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, needle) {
					return line
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no audit line containing %q in %s", needle, path)
	return ""
}
