package daemon

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestTerminalCredentialSources(t *testing.T) {
	const secret = "s3cret!"
	encoded := base64.RawURLEncoding.EncodeToString([]byte(secret))
	// "s3cret!" is 7 bytes, so the padded encoding ends in "==" and a
	// subprotocol token cannot carry it.
	padded := base64.URLEncoding.EncodeToString([]byte(secret))
	if encoded == padded {
		t.Fatalf("expected %q to carry padding", padded)
	}
	for _, tc := range []struct {
		name          string
		subprotocol   string
		authorization string
		want          string
		ok            bool
	}{
		{name: "subprotocol token", subprotocol: terminalSubprotocolPrefix + encoded, want: secret, ok: true},
		{name: "padded base64 rejected", subprotocol: terminalSubprotocolPrefix + padded},
		{name: "empty token rejected", subprotocol: terminalSubprotocolPrefix},
		{name: "unknown token rejected", subprotocol: "maidcafe.other." + encoded},
		{name: "invalid base64 rejected", subprotocol: terminalSubprotocolPrefix + "!!!!"},
		{name: "bearer header", authorization: "Bearer " + secret, want: secret, ok: true},
		{
			name:          "valid header wins over an invalid token",
			subprotocol:   terminalSubprotocolPrefix + "!!!!",
			authorization: "Bearer " + secret,
			want:          secret,
			ok:            true,
		},
		{name: "nothing offered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/terminal", nil)
			if tc.subprotocol != "" {
				request.Header.Set("Sec-WebSocket-Protocol", tc.subprotocol)
			}
			if tc.authorization != "" {
				request.Header.Set("Authorization", tc.authorization)
			}
			got, ok := terminalCredential(request)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("terminalCredential() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	t.Run("query string is never a credential", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/terminal?tk="+secret, nil)
		if got, ok := terminalCredential(request); ok {
			t.Fatalf("terminalCredential() accepted query credential %q", got)
		}
	})
	t.Run("offered token is echoed", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/terminal", nil)
		request.Header.Set("Sec-WebSocket-Protocol", "other, "+terminalSubprotocolPrefix+encoded)
		offered := offeredSubprotocols(request)
		if len(offered) != 1 || offered[0] != terminalSubprotocolPrefix+encoded {
			t.Fatalf("offeredSubprotocols() = %v", offered)
		}
	})
	t.Run("no offered token", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/terminal", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		if offered := offeredSubprotocols(request); offered != nil {
			t.Fatalf("offeredSubprotocols() = %v, want nil", offered)
		}
	})
}

func TestTerminalRemoteAllowed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  config.TerminalConfig
		addr    string
		allowed bool
	}{
		{name: "loopback ipv4", addr: "127.0.0.1:1", allowed: true},
		{name: "loopback ipv6", addr: "[::1]:1", allowed: true},
		{name: "rfc1918 ten", addr: "10.0.0.5:1", allowed: true},
		{name: "rfc1918 one-nine-two", addr: "192.168.1.4:1", allowed: true},
		{name: "tailscale cgnat", addr: "100.101.2.3:1", allowed: true},
		{name: "public address", addr: "8.8.8.8:1"},
		{name: "unparsable address", addr: "garbage"},
		{name: "public address with allowRemote", policy: config.TerminalConfig{AllowRemote: true}, addr: "8.8.8.8:1", allowed: true},
		{name: "unparsable address with allowRemote", policy: config.TerminalConfig{AllowRemote: true}, addr: "garbage", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalRemoteAllowed(tc.policy, tc.addr); got != tc.allowed {
				t.Fatalf("terminalRemoteAllowed(%q) = %v, want %v", tc.addr, got, tc.allowed)
			}
		})
	}
}

func TestTerminalResolve(t *testing.T) {
	policy := config.TerminalConfig{
		Enabled: true,
		Shells:  []string{"/bin/sh", "/bin/bash"},
		Users:   []string{"deploy"},
	}
	resolved, err := newTerminalManager(nil, nil).Resolve(policy, terminalRequest{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Shell != "/bin/sh" {
		t.Fatalf("default shell = %q, want /bin/sh", resolved.Shell)
	}
	if resolved.Cols != terminalDefaultCols || resolved.Rows != terminalDefaultRows {
		t.Fatalf("default size = %dx%d", resolved.Cols, resolved.Rows)
	}
	explicit, err := newTerminalManager(nil, nil).Resolve(policy, terminalRequest{
		Shell: "/bin/bash", User: "deploy", Cols: 120, Rows: 40,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if explicit.Shell != "/bin/bash" || explicit.User != "deploy" || explicit.Cols != 120 || explicit.Rows != 40 {
		t.Fatalf("Resolve() = %#v", explicit)
	}
	if _, err := newTerminalManager(nil, nil).Resolve(policy, terminalRequest{Shell: "/bin/zsh"}); err != errTerminalShellNotAllowed {
		t.Fatalf("unlisted shell error = %v", err)
	}
	if _, err := newTerminalManager(nil, nil).Resolve(policy, terminalRequest{User: "nobody"}); err != errTerminalUserNotAllowed {
		t.Fatalf("unlisted user error = %v", err)
	}
	empty := config.TerminalConfig{}
	if _, err := newTerminalManager(nil, nil).Resolve(empty, terminalRequest{}); err != errTerminalShellNotAllowed {
		t.Fatalf("empty policy error = %v", err)
	}
}

func TestTerminalQueryDimensions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
		ok    bool
	}{
		{name: "absent uses the default", value: "", want: 0, ok: true},
		{name: "blank uses the default", value: "  ", want: 0, ok: true},
		{name: "in range", value: "132", want: 132, ok: true},
		{name: "lower bound", value: "1", want: 1, ok: true},
		{name: "upper bound", value: "1000", want: 1000, ok: true},
		{name: "zero out of range", value: "0"},
		{name: "negative out of range", value: "-1"},
		{name: "above the bound", value: "1001"},
		{name: "not a number", value: "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := terminalQueryDimension(tc.value, terminalMinCols, terminalMaxCols)
			if (err == nil) != tc.ok {
				t.Fatalf("terminalQueryDimension(%q) error = %v", tc.value, err)
			}
			if err == nil && got != tc.want {
				t.Fatalf("terminalQueryDimension(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// TestTerminalSessionCapDefaults pins the zero-value contract: a policy built
// without an explicit cap still admits the documented default number of
// sessions.
func TestTerminalSessionCapDefaults(t *testing.T) {
	if got := terminalSessionCap(config.TerminalConfig{}); got != config.TerminalDefaultMaxSessions {
		t.Fatalf("terminalSessionCap(zero) = %d, want %d", got, config.TerminalDefaultMaxSessions)
	}
	if got := terminalSessionCap(config.TerminalConfig{MaxSessions: 5}); got != 5 {
		t.Fatalf("terminalSessionCap(5) = %d, want 5", got)
	}
	if manager := newTerminalManager(nil, nil); manager.Count() != 0 {
		t.Fatalf("fresh manager Count() = %d", manager.Count())
	}
}

// TestTerminalManagerRejectsBeyondCap checks the concurrency rejection without
// a socket.
func TestTerminalManagerRejectsBeyondCap(t *testing.T) {
	if !terminalSupported() {
		t.Skip("no PTY on this platform")
	}
	manager := newTerminalManager(nil, nil)
	policy := config.TerminalConfig{Shells: []string{"/bin/sh"}, MaxSessions: 1}
	if _, err := manager.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	if _, err := manager.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "127.0.0.1:1"); err != errTerminalTooMany {
		t.Fatalf("second Open() error = %v, want %v", err, errTerminalTooMany)
	}
}

// TestTerminalSessionLifecycle drives a real shell through the manager only:
// output reaches the queue, an exit command closes Done, and Close records the
// exit without a socket in the picture.
func TestTerminalSessionLifecycle(t *testing.T) {
	if !terminalSupported() {
		t.Skip("no PTY on this platform")
	}
	manager := newTerminalManager(nil, nil)
	session, err := manager.Open(config.TerminalConfig{Shells: []string{"/bin/sh"}}, terminalRequest{
		Shell: "/bin/sh", Cols: 80, Rows: 24,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(terminalReasonShutdown)
	if _, err := session.Write([]byte("echo manager-probe\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Resize(100, 30); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if got := session.Cols(); got != 100 {
		t.Fatalf("Cols() = %d after Resize", got)
	}
	deadline := time.After(10 * time.Second)
	output := ""
	for !strings.Contains(output, "manager-probe") {
		select {
		case chunk := <-session.Output():
			output += string(chunk)
		case <-deadline:
			t.Fatalf("timeout waiting for shell output, got %q", output)
		}
	}
	if _, err := session.Write([]byte("exit\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the shell to exit")
	}
	code, reason := session.Exit()
	if code != 0 || reason != "" {
		t.Fatalf("Exit() = %d, %q; want 0, \"\"", code, reason)
	}
	session.Close("")
	manager.CloseAll()
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count() = %d after CloseAll", got)
	}
}
