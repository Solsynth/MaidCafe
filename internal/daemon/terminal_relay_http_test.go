package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// relayTestCloud fakes the cloud's relayed-terminal routes. The pickup
// long-poll hands the queued sessions over once and then holds like the real
// cloud, and the agent route upgrades and runs a browser-side script against
// the daemon's outbound socket.
type relayTestCloud struct {
	t      *testing.T
	server *httptest.Server
	secret string
	agent  func(*websocket.Conn)
	mu     sync.Mutex
	queued []terminalRelaySession
}

func newRelayTestCloud(t *testing.T, secret string, sessions []terminalRelaySession, agent func(*websocket.Conn)) *relayTestCloud {
	t.Helper()
	cloud := &relayTestCloud{t: t, secret: secret, queued: sessions, agent: agent}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemons/", cloud.handle)
	cloud.server = httptest.NewServer(mux)
	t.Cleanup(cloud.server.Close)
	return cloud
}

func (c *relayTestCloud) handle(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+c.secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/terminal/requests/pending"):
		c.handlePending(w, r)
	case strings.HasSuffix(r.URL.Path, "/terminal/agent"):
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			c.t.Errorf("accept agent socket: %v", err)
			return
		}
		defer conn.CloseNow()
		if c.agent != nil {
			c.agent(conn)
		}
	default:
		http.NotFound(w, r)
	}
}

func (c *relayTestCloud) handlePending(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	batch := c.queued
	c.queued = nil
	c.mu.Unlock()
	if len(batch) == 0 {
		// Hold the poll like the real cloud, so the relay cannot hot-loop and
		// the test's cancel is what ends it.
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": batch})
}

// relayApp starts a daemon (without an HTTP listener of its own) whose terminal
// relay points at the fake cloud.
func relayApp(t *testing.T, cloud *relayTestCloud, mutate func(*config.DaemonConfig)) (*App, string) {
	t.Helper()
	app, _, auditPath := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.CloudURL = cloud.server.URL
		cfg.CloudSecret = cloud.secret
		cfg.Terminal.Relay = config.TerminalRelayConfig{Enabled: true}
		if mutate != nil {
			mutate(cfg)
		}
	})
	return app, auditPath
}

// startTerminalRelay runs the app's relay under its own context and returns a
// stop function that cancels the polling and waits for it to end.
func startTerminalRelay(t *testing.T, app *App) func() {
	t.Helper()
	if app.terminalRelay == nil {
		t.Fatal("the app has no terminal relay")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.terminalRelay.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("terminal relay did not stop")
		}
	}
}

// waitFor polls cond until it holds, failing the test at the deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readRelayOutput reads frames until the accumulated output contains needle.
func readRelayOutput(ctx context.Context, conn *websocket.Conn, output *strings.Builder, needle string) error {
	for !strings.Contains(output.String(), needle) {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("waiting for %q in %q: %w", needle, output.String(), err)
		}
		if typ == websocket.MessageBinary {
			output.Write(data)
		}
	}
	return nil
}

// TestTerminalRelayServesShellThroughAgentSocket drives one relayed session end
// to end: the fake cloud hands over a session, the daemon dials the agent
// socket and starts a shell, the script acts as the browser (resize, commands,
// exit), and the shell's output comes back through that socket.
func TestTerminalRelayServesShellThroughAgentSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the daemon has no PTY on Windows")
	}
	type relayResult struct {
		hello     map[string]any
		output    strings.Builder
		exit      map[string]any
		closeCode websocket.StatusCode
		err       error
	}
	results := make(chan relayResult, 1)
	script := func(conn *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		got := relayResult{}
		defer func() { results <- got }()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			got.err = err
			return
		}
		if typ != websocket.MessageText {
			got.err = fmt.Errorf("hello frame type = %v, want text", typ)
			return
		}
		if err := json.Unmarshal(data, &got.hello); err != nil {
			got.err = err
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":110,"rows":32}`)); err != nil {
			got.err = err
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte("stty size\n")); err != nil {
			got.err = err
			return
		}
		// The pickup asked for 90x30; the resize above must have applied before
		// the command ran.
		if err := readRelayOutput(ctx, conn, &got.output, "32 110"); err != nil {
			got.err = err
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo relay-ok\n")); err != nil {
			got.err = err
			return
		}
		if err := readRelayOutput(ctx, conn, &got.output, "relay-ok"); err != nil {
			got.err = err
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte("exit\n")); err != nil {
			got.err = err
			return
		}
		for {
			typ, data, err = conn.Read(ctx)
			if err != nil {
				got.closeCode = websocket.CloseStatus(err)
				return
			}
			if typ == websocket.MessageBinary {
				got.output.Write(data)
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				got.err = err
				return
			}
			if frame["type"] == "exit" {
				got.exit = frame
			}
		}
	}
	cloud := newRelayTestCloud(t, "cloud-secret", []terminalRelaySession{{
		ID: "session-1", Shell: "/bin/sh", User: "", Cols: 90, Rows: 30, InvokedBy: "alice@solsynth.dev",
	}}, script)
	app, auditPath := relayApp(t, cloud, nil)
	stop := startTerminalRelay(t, app)
	defer stop()

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("agent script: %v", got.err)
		}
		if got.hello["type"] != "hello" || got.hello["version"] != terminalProtocolVersion {
			t.Fatalf("hello = %v", got.hello)
		}
		if got.hello["shell"] != "/bin/sh" || got.hello["user"] != currentTerminalUser() {
			t.Fatalf("hello = %v", got.hello)
		}
		if session, _ := got.hello["session"].(string); session == "" {
			t.Fatalf("hello carries no session id: %v", got.hello)
		}
		if !strings.Contains(got.output.String(), "relay-ok") {
			t.Fatalf("shell output = %q, want the echoed command", got.output.String())
		}
		if got.exit["code"] != float64(0) || got.exit["reason"] != "" {
			t.Fatalf("exit frame = %v", got.exit)
		}
		if got.closeCode != websocket.StatusNormalClosure {
			t.Fatalf("close code = %d, want %d", got.closeCode, websocket.StatusNormalClosure)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the relayed session")
	}
	waitFor(t, "the session to be released", func() bool { return app.terminal.Count() == 0 })
	waitFor(t, "the audit entry", func() bool {
		data, err := os.ReadFile(auditPath)
		return err == nil && strings.Contains(string(data), `"source":"terminal"`) &&
			strings.Contains(string(data), "cloud:alice@solsynth.dev")
	})
}

// relayRejection is what the fake cloud's browser side observed when the
// daemon refused a relayed session.
type relayRejection struct {
	message   string
	closeCode websocket.StatusCode
	err       error
}

// readRelayRejection reads the in-band error frame and then the close.
func readRelayRejection(ctx context.Context, conn *websocket.Conn) relayRejection {
	var got relayRejection
	typ, data, err := conn.Read(ctx)
	if err != nil {
		got.err = err
		return got
	}
	if typ != websocket.MessageText {
		got.err = fmt.Errorf("frame type = %v, want a text error frame", typ)
		return got
	}
	var frame terminalErrorFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		got.err = err
		return got
	}
	if frame.Type != "error" {
		got.err = fmt.Errorf("frame type = %q, want error", frame.Type)
		return got
	}
	got.message = frame.Message
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			got.closeCode = websocket.CloseStatus(err)
			return got
		}
	}
}

// TestTerminalRelayRejectsInBand proves every daemon-side gate answers on the
// agent socket rather than dropping it, so the browser waiting on the cloud
// learns why no shell appeared and no session is left behind.
func TestTerminalRelayRejectsInBand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the daemon has no PTY on Windows")
	}
	allowAlice := func(cfg *config.DaemonConfig) { cfg.Terminal.Relay.Users = []string{"alice@solsynth.dev"} }
	for _, tc := range []struct {
		name     string
		session  terminalRelaySession
		mutate   func(*config.DaemonConfig)
		saturate bool
		want     string
	}{
		{
			name:    "identity not allowlisted",
			session: terminalRelaySession{ID: "s", Shell: "/bin/sh", InvokedBy: "mallory@solsynth.dev", Cols: 80, Rows: 24},
			mutate:  allowAlice,
			want:    "terminal identity not allowed",
		},
		{
			name:    "shell not allowlisted",
			session: terminalRelaySession{ID: "s", Shell: "/bin/zsh", InvokedBy: "alice@solsynth.dev", Cols: 80, Rows: 24},
			want:    "terminal shell not allowed",
		},
		{
			name:    "user not allowlisted",
			session: terminalRelaySession{ID: "s", Shell: "/bin/sh", User: "nobody", InvokedBy: "alice@solsynth.dev", Cols: 80, Rows: 24},
			want:    "terminal user not allowed",
		},
		{
			name:     "sessions saturated",
			session:  terminalRelaySession{ID: "s", Shell: "/bin/sh", InvokedBy: "alice@solsynth.dev", Cols: 80, Rows: 24},
			saturate: true,
			want:     "too many terminal sessions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := make(chan relayRejection, 1)
			cloud := newRelayTestCloud(t, "cloud-secret", []terminalRelaySession{tc.session}, func(conn *websocket.Conn) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				answers <- readRelayRejection(ctx, conn)
			})
			app, _ := relayApp(t, cloud, tc.mutate)
			if tc.saturate {
				// Hold the single session slot the policy allows.
				holder, err := app.terminal.Open(app.rt.Load().terminal,
					terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "test")
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close("")
			}
			stop := startTerminalRelay(t, app)
			defer stop()
			select {
			case got := <-answers:
				if got.err != nil {
					t.Fatalf("agent script: %v", got.err)
				}
				if got.message != tc.want {
					t.Fatalf("error message = %q, want %q", got.message, tc.want)
				}
				if got.closeCode != websocket.StatusPolicyViolation {
					t.Fatalf("close code = %d, want %d", got.closeCode, websocket.StatusPolicyViolation)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("timed out waiting for the rejection")
			}
			if tc.saturate {
				if got := app.terminal.Count(); got != 1 {
					t.Fatalf("live sessions = %d, want the single holder", got)
				}
				return
			}
			waitFor(t, "no session to be left behind", func() bool { return app.terminal.Count() == 0 })
		})
	}
}
