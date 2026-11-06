package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"os/user"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// terminalSessionsRequest performs one plain HTTP call against the background
// tmux session routes and returns the status and decoded body.
func terminalSessionsRequest(t *testing.T, method, url, secret string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return resp.StatusCode, decoded
}

func TestTerminalSessionsRoutesRequireTheTerminalSecret(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, nil)
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	status, body := terminalSessionsRequest(t, http.MethodGet, base+"/api/v1/terminal/sessions", "")
	if status != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("anonymous listing = %d %v; want 401 unauthorized", status, body)
	}

	status, body = terminalSessionsRequest(t, http.MethodDelete, base+"/api/v1/terminal/sessions/maidkit-x", "wrong")
	if status != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("wrong secret delete = %d %v; want 401 unauthorized", status, body)
	}
}

func TestTerminalSessionsRoutesAreClosedWhenTheTerminalIsDisabled(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.Terminal.Enabled = false
	})
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	status, body := terminalSessionsRequest(t, http.MethodGet, base+"/api/v1/terminal/sessions", "metrics-secret")
	if status != http.StatusForbidden || body["error"] != "terminal disabled" {
		t.Fatalf("listing with the terminal off = %d %v; want 403", status, body)
	}
	status, body = terminalSessionsRequest(t, http.MethodDelete, base+"/api/v1/terminal/sessions/maidkit-x", "metrics-secret")
	if status != http.StatusForbidden || body["error"] != "terminal disabled" {
		t.Fatalf("delete with the terminal off = %d %v; want 403", status, body)
	}
}

func TestTerminalSessionKillValidatesTheName(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, nil)
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	for _, name := range []string{"-x", strings.Repeat("a", 41), "a;b"} {
		status, body := terminalSessionsRequest(
			t,
			http.MethodDelete,
			base+"/api/v1/terminal/sessions/"+name,
			"metrics-secret",
		)
		// A name with a slash or a semicolon is rejected before the handler by
		// the router or by the handler's own validation; both are a refusal, not
		// a kill.
		if status != http.StatusBadRequest && status != http.StatusNotFound {
			t.Fatalf("kill %q = %d %v; want 400", name, status, body)
		}
		if status == http.StatusBadRequest && body["error"] != "terminal tmux name is invalid" {
			t.Fatalf("kill %q = %d %v; want the invalid-name message", name, status, body)
		}
	}
}

func TestTerminalSessionsReportsTmuxAvailability(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, nil)
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	status, body := terminalSessionsRequest(t, http.MethodGet, base+"/api/v1/terminal/sessions", "metrics-secret")
	if terminalTmuxInstalled() {
		if status != http.StatusOK || body["ok"] != true || body["tmux"] != true {
			t.Fatalf("listing = %d %v; want 200 with tmux true", status, body)
		}
		if sessions, ok := body["sessions"]; !ok || sessions == nil {
			t.Fatalf("listing = %v; want a sessions array", body)
		}
		return
	}
	// A host without tmux says so instead of pretending the list is empty.
	if status != http.StatusNotImplemented || body["error"] != errTerminalTmuxUnavailable.Error() {
		t.Fatalf("listing without tmux = %d %v; want 501", status, body)
	}
}

func TestTerminalSessionKillWithoutTmux(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, nil)
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	status, body := terminalSessionsRequest(t, http.MethodDelete, base+"/api/v1/terminal/sessions/maidkit-x", "metrics-secret")
	if terminalTmuxInstalled() {
		if status != http.StatusNotFound {
			t.Fatalf("kill of an unknown session = %d %v; want 404", status, body)
		}
		return
	}
	if status != http.StatusNotImplemented {
		t.Fatalf("kill without tmux = %d %v; want 501", status, body)
	}
}

func TestTerminalSessionsHonoursTheUserAllowlist(t *testing.T) {
	_, wsURL, _ := terminalDaemon(t, func(cfg *config.DaemonConfig) {
		current, err := user.Current()
		if err != nil {
			t.Fatalf("current user: %v", err)
		}
		cfg.Terminal.Users = []string{current.Username}
	})
	base := strings.TrimSuffix(strings.Replace(wsURL, "ws://", "http://", 1), "/api/v1/terminal")

	status, body := terminalSessionsRequest(
		t,
		http.MethodGet,
		base+"/api/v1/terminal/sessions?user=not-in-the-allowlist",
		"metrics-secret",
	)
	if status != http.StatusForbidden || body["error"] != "terminal user not allowed" {
		t.Fatalf("listing as a refused user = %d %v; want 403", status, body)
	}
}

// A session counts against the terminal cap exactly like a shell, so the cap
// must be reachable through the shared manager rather than a second counter.
func TestTerminalSessionsShareTheSessionCap(t *testing.T) {
	app, _, _ := terminalDaemon(t, nil)
	policy := app.rt.Load().terminal
	if terminalSessionCap(policy) != 1 {
		t.Fatalf("cap = %d; want the configured 1", terminalSessionCap(policy))
	}
	session, err := app.terminal.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		session.Close("test cleanup")
		deadline := time.Now().Add(5 * time.Second)
		for app.terminal.Count() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err := app.terminal.Open(policy, terminalRequest{Shell: "/bin/sh", Cols: 80, Rows: 24}, "test"); err == nil {
		t.Fatal("second session opened past the cap")
	}
}
