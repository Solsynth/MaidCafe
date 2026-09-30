package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// fakeTerminalConn is an in-memory terminalConn: it replays a scripted inbox of
// client frames and then blocks until the session loop closes it, recording
// every frame the loop wrote. It lets the shared session loop be driven without
// a socket on either side.
type fakeTerminalConn struct {
	mu      sync.Mutex
	inbox   []fakeTerminalFrame
	written []fakeTerminalFrame
	closed  chan struct{}
	once    sync.Once
	code    websocket.StatusCode
	reason  string
}

type fakeTerminalFrame struct {
	typ  websocket.MessageType
	data []byte
}

func newFakeTerminalConn(inbox ...fakeTerminalFrame) *fakeTerminalConn {
	return &fakeTerminalConn{inbox: inbox, closed: make(chan struct{})}
}

func (c *fakeTerminalConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	c.mu.Lock()
	if len(c.inbox) > 0 {
		frame := c.inbox[0]
		c.inbox = c.inbox[1:]
		c.mu.Unlock()
		return frame.typ, frame.data, nil
	}
	c.mu.Unlock()
	<-c.closed
	return websocket.MessageText, nil, errors.New("test terminal connection closed")
}

func (c *fakeTerminalConn) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, fakeTerminalFrame{typ: typ, data: append([]byte(nil), data...)})
	return nil
}

func (c *fakeTerminalConn) Ping(context.Context) error { return nil }

func (c *fakeTerminalConn) Close(code websocket.StatusCode, reason string) error {
	c.once.Do(func() {
		c.mu.Lock()
		c.code, c.reason = code, reason
		c.mu.Unlock()
		close(c.closed)
	})
	return nil
}

func (c *fakeTerminalConn) CloseNow() error { return c.Close(websocket.StatusInternalError, "closed") }

// textFrames decodes every text frame written so far, in order.
func (c *fakeTerminalConn) textFrames(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	frames := append([]fakeTerminalFrame(nil), c.written...)
	c.mu.Unlock()
	var texts []map[string]any
	for _, frame := range frames {
		if frame.typ != websocket.MessageText {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(frame.data, &decoded); err != nil {
			t.Fatalf("decode written text frame %q: %v", frame.data, err)
		}
		texts = append(texts, decoded)
	}
	return texts
}

func (c *fakeTerminalConn) closeStatus() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}

// dump renders every frame written so far, for failure messages.
func (c *fakeTerminalConn) dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	for _, frame := range c.written {
		kind := "text"
		if frame.typ == websocket.MessageBinary {
			kind = "binary"
		}
		fmt.Fprintf(&out, "%s %q; ", kind, frame.data)
	}
	return out.String()
}

// runSessionWithin runs the shared session loop with a watchdog so a stuck
// session fails the test instead of hanging it.
func runSessionWithin(t *testing.T, session *terminalSession, conn terminalConn, policy config.TerminalConfig) string {
	t.Helper()
	result := make(chan string, 1)
	go func() {
		result <- runTerminalSession(context.Background(), session, conn, policy)
	}()
	select {
	case reason := <-result:
		return reason
	case <-time.After(20 * time.Second):
		t.Fatal("runTerminalSession did not return")
		return ""
	}
}

// frameIndex returns the position of the first text frame of the given type, or
// -1. The pump writes hello and the exit frame from another goroutine, so only
// the relative order of the error and exit frames is guaranteed.
func frameIndex(frames []map[string]any, kind string) int {
	for i, frame := range frames {
		if frame["type"] == kind {
			return i
		}
	}
	return -1
}

// TestRunTerminalSessionFrames drives the extracted session loop over a fake
// connection, so the v1 codec is proven transport-agnostic: a clean shell exit
// ends with the exit frame and close 1000, and a protocol violation answers
// with the error frame, the exit frame and close 1008.
func TestRunTerminalSessionFrames(t *testing.T) {
	if !terminalSupported() {
		t.Skip("no PTY on this platform")
	}
	policy := config.TerminalConfig{Shells: []string{"/bin/sh"}, IdleTimeout: time.Minute, MaxLifetime: time.Hour}

	t.Run("clean exit", func(t *testing.T) {
		manager := newTerminalManager(nil, nil)
		defer manager.CloseAll()
		session, err := manager.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "test")
		if err != nil {
			t.Fatal(err)
		}
		// An explicit status, because a login shell's `exit` may report the
		// status of its own logout path instead of 0.
		conn := newFakeTerminalConn(fakeTerminalFrame{typ: websocket.MessageBinary, data: []byte("exit 0\n")})
		if reason := runSessionWithin(t, session, conn, policy); reason != "" {
			t.Fatalf("close reason = %q, want an empty reason for a clean exit", reason)
		}
		frames := conn.textFrames(t)
		hello, exit := frameIndex(frames, "hello"), frameIndex(frames, "exit")
		if hello < 0 {
			t.Fatalf("no hello frame in %v", frames)
		}
		if exit < 0 {
			t.Fatalf("no exit frame in %v", frames)
		}
		if frames[hello]["version"] != terminalProtocolVersion || frames[hello]["shell"] != "/bin/sh" {
			t.Fatalf("hello = %v", frames[hello])
		}
		if frames[hello]["cols"] != float64(80) || frames[hello]["rows"] != float64(24) {
			t.Fatalf("hello size = %vx%v, want 80x24", frames[hello]["cols"], frames[hello]["rows"])
		}
		if frames[exit]["code"] != float64(0) || frames[exit]["reason"] != "" {
			t.Fatalf("exit = %v, want code 0 and an empty reason (frames: %s)", frames[exit], conn.dump())
		}
		if status := conn.closeStatus(); status != websocket.StatusNormalClosure {
			t.Fatalf("close status = %d, want %d", status, websocket.StatusNormalClosure)
		}
	})

	t.Run("protocol violation", func(t *testing.T) {
		manager := newTerminalManager(nil, nil)
		defer manager.CloseAll()
		session, err := manager.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "test")
		if err != nil {
			t.Fatal(err)
		}
		conn := newFakeTerminalConn(fakeTerminalFrame{typ: websocket.MessageText, data: []byte(`{"type":"bogus"}`)})
		if reason := runSessionWithin(t, session, conn, policy); reason != terminalReasonProtocol {
			t.Fatalf("close reason = %q, want %q", reason, terminalReasonProtocol)
		}
		frames := conn.textFrames(t)
		problem, exit := frameIndex(frames, "error"), frameIndex(frames, "exit")
		if problem < 0 || exit < 0 || problem > exit {
			t.Fatalf("frames = %v, want an error frame before the exit frame", frames)
		}
		if frames[problem]["message"] != "unsupported control frame" {
			t.Fatalf("error frame = %v", frames[problem])
		}
		if frames[exit]["reason"] != terminalReasonProtocol {
			t.Fatalf("exit frame = %v", frames[exit])
		}
		if status := conn.closeStatus(); status != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %d, want %d", status, websocket.StatusPolicyViolation)
		}
	})
}

func TestTerminalRelayIdentityAllowed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		users    []string
		identity string
		allowed  bool
	}{
		{name: "empty list accepts anyone", identity: "alice@solsynth.dev", allowed: true},
		{name: "empty list accepts an empty identity", identity: "", allowed: true},
		{name: "listed identity", users: []string{"alice@solsynth.dev"}, identity: "alice@solsynth.dev", allowed: true},
		{name: "unlisted identity", users: []string{"alice@solsynth.dev"}, identity: "mallory@solsynth.dev"},
		{name: "matching is exact", users: []string{"alice@solsynth.dev"}, identity: "Alice@solsynth.dev"},
		{name: "empty identity against a list", users: []string{"alice@solsynth.dev"}, identity: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := config.TerminalConfig{Relay: config.TerminalRelayConfig{Users: tc.users}}
			if got := terminalRelayIdentityAllowed(policy, tc.identity); got != tc.allowed {
				t.Fatalf("terminalRelayIdentityAllowed(%q) = %v, want %v", tc.identity, got, tc.allowed)
			}
		})
	}
}

func TestTerminalRelayPollWait(t *testing.T) {
	if got := terminalRelayPollWait(config.TerminalConfig{}); got != config.TerminalRelayDefaultPollWait {
		t.Fatalf("pollWait(zero) = %s, want the default %s", got, config.TerminalRelayDefaultPollWait)
	}
	policy := config.TerminalConfig{Relay: config.TerminalRelayConfig{PollWait: 5 * time.Second}}
	if got := terminalRelayPollWait(policy); got != 5*time.Second {
		t.Fatalf("pollWait(5s) = %s", got)
	}
}

// TestCloudPublisherTerminalAgentTarget pins the HTTP→WS conversion: the
// scheme follows the cloud URL and any base path survives, because the relay
// routes hang off the same /api/daemons/<id> prefix as the JSON routes.
func TestCloudPublisherTerminalAgentTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cloudURL string
		want     string
	}{
		{
			name:     "https keeps its base path",
			cloudURL: "https://mk.solsynth.dev/base/",
			want:     "wss://mk.solsynth.dev/base/api/daemons/daemon-1/terminal/agent?session=s1",
		},
		{
			name:     "http loopback maps to ws",
			cloudURL: "http://127.0.0.1:8080",
			want:     "ws://127.0.0.1:8080/api/daemons/daemon-1/terminal/agent?session=s1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher, err := NewCloudPublisher(config.DaemonConfig{
				ID: "daemon-1", CloudURL: tc.cloudURL, CloudSecret: "cloud-secret", RequestTimeout: time.Second,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			target, secret, err := publisher.terminalAgentTarget("s1")
			if err != nil {
				t.Fatal(err)
			}
			if target != tc.want {
				t.Fatalf("target = %q, want %q", target, tc.want)
			}
			if secret != "cloud-secret" {
				t.Fatalf("secret = %q", secret)
			}
		})
	}
	t.Run("unconfigured publisher", func(t *testing.T) {
		var publisher *CloudPublisher
		if _, _, err := publisher.terminalAgentTarget("s1"); err == nil {
			t.Fatal("expected an error from an unconfigured publisher")
		}
	})
}

// TestCloudPublisherRequestTimeoutOverride proves the long-poll's per-call
// deadline: the configured timeout still bounds ordinary calls, while the
// override lets the pickup outlive it.
func TestCloudPublisherRequestTimeoutOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}))
	defer server.Close()
	publisher, err := NewCloudPublisher(config.DaemonConfig{
		ID: "host-1", CloudURL: server.URL, CloudSecret: "secret", RequestTimeout: 100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.request(t.Context(), "GET", "/terminal/requests/pending", nil, nil); err == nil {
		t.Fatal("expected the configured deadline to expire")
	}
	var pending terminalRelayPending
	if err := publisher.requestWithTimeout(t.Context(), 5*time.Second, "GET", "/terminal/requests/pending", nil, &pending); err != nil {
		t.Fatalf("override request failed: %v", err)
	}
}
