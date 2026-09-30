package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	"src.solsynth.dev/solsynth/maidcafe/internal/server"
)

// stackWorkspaces satisfies cloud.WorkspaceClient for the end-to-end test: any
// account is a member of any workspace and quotas are unlimited.
type stackWorkspaces struct{}

func (stackWorkspaces) IsMemberWithRole(context.Context, string, string, []int32) (bool, error) {
	return true, nil
}

func (stackWorkspaces) GetPlanQuota(context.Context, string) (map[string]int64, error) {
	return map[string]int64{"max_daemons": 10}, nil
}

// TestRelayedTerminalEndToEnd is the full-stack proof for the cloud-relayed
// terminal: the real cloud router (with real pairing, real tickets and real
// per-session sockets), the real daemon relay client, and a real shell in a
// real PTY, all in one process.
//
// The httptest server deliberately installs Read/WriteTimeout values shorter
// than the session's life, mirroring the production cloud's 30s write timeout:
// a hijacked connection keeps the server deadlines, so the test only passes if
// both cloud sockets clear them.
func TestRelayedTerminalEndToEnd(t *testing.T) {
	const (
		accountID   = "acct-1"
		workspaceID = "ws-1"
		identity    = "alice@stack.test"
	)

	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := cloud.NewService(db, nil, stackWorkspaces{})
	credential, err := svc.CreateDaemon(ctx, accountID, workspaceID, "stack-host")
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := svc.UpdateDaemon(ctx, accountID, credential.ID, nil, nil, &enabled); err != nil {
		t.Fatal(err)
	}

	cloudCfg := &config.Config{HTTP: config.HTTPConfig{Port: "8080"}}
	router := server.NewRouter(cloudCfg, svc, nil)
	cloudServer := httptest.NewUnstartedServer(router)
	cloudServer.Config.ReadTimeout = 2 * time.Second
	cloudServer.Config.ReadHeaderTimeout = 2 * time.Second
	cloudServer.Config.WriteTimeout = 2 * time.Second
	cloudServer.Start()
	defer cloudServer.Close()

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	app, err := NewApp(config.DaemonConfig{
		ID:                credential.ID,
		Version:           "stack",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		CloudURL:          cloudServer.URL,
		CloudSecret:       credential.Secret,
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Hour,
		Runtimes:          []string{"java"},
		ProcessesLimit:    50,
		RequestTimeout:    10 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		AuditPath:         auditPath,
		Terminal: config.TerminalConfig{
			Shells:      []string{"/bin/sh"},
			MaxSessions: 1,
			IdleTimeout: time.Minute,
			MaxLifetime: time.Hour,
			Relay: config.TerminalRelayConfig{
				Enabled:  true,
				Users:    []string{identity},
				PollWait: 4 * time.Second,
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, stopApp := context.WithCancel(context.Background())
	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		if err := app.Run(runCtx); err != nil {
			t.Errorf("daemon run: %v", err)
		}
	}()
	defer func() {
		stopApp()
		<-appDone
	}()

	ticket, err := svc.CreateTerminalSession(ctx, accountID, credential.ID, identity, "", "", 120, 36)
	if err != nil {
		t.Fatal(err)
	}

	// The browser leg speaks the daemon's v1 protocol; the credential rides as
	// a subprotocol token because a browser cannot set a handshake header.
	token := "maidcafe.terminal." + base64.RawURLEncoding.EncodeToString(
		[]byte(ticket.SessionID+"."+ticket.Ticket),
	)
	wsURL := "ws" + strings.TrimPrefix(cloudServer.URL, "http") +
		"/api/daemons/" + credential.ID + "/terminal"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{token},
	})
	if err != nil {
		t.Fatalf("dial relayed terminal: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	hello := readTerminalText(t, ctx, conn)
	if hello["type"] != "hello" || hello["version"] != "v1" {
		t.Fatalf("unexpected hello frame: %#v", hello)
	}
	if hello["shell"] != "/bin/sh" || hello["cols"] != float64(120) || hello["rows"] != float64(36) {
		t.Fatalf("hello does not reflect the ticket geometry: %#v", hello)
	}

	// The shell reached us through the cloud, the daemon's outbound socket and
	// a real PTY: type a command and read its output back.
	writeTerminalBinary(t, ctx, conn, "echo stack-ok\n")
	readTerminalOutput(t, ctx, conn, "stack-ok")

	// Geometry: the PTY was opened at the ticket size, and a resize frame
	// travels the whole way (cloud pump included) into the PTY.
	writeTerminalBinary(t, ctx, conn, "stty size\n")
	readTerminalOutput(t, ctx, conn, "36 120")
	resize, err := json.Marshal(map[string]any{"type": "resize", "cols": 100, "rows": 30})
	if err != nil {
		t.Fatal(err)
	}
	stackWriteText(t, ctx, conn, string(resize))
	writeTerminalBinary(t, ctx, conn, "stty size\n")
	readTerminalOutput(t, ctx, conn, "30 100")

	// Outlive the cloud's 2s server deadlines: a hijacked connection keeps
	// them unless the handler clears them, so this is the check that the relay
	// survives past the cloud's real 30s WriteTimeout.
	time.Sleep(3 * time.Second)
	writeTerminalBinary(t, ctx, conn, "echo still-alive\n")
	readTerminalOutput(t, ctx, conn, "still-alive")

	writeTerminalBinary(t, ctx, conn, "exit\n")
	exit := readTerminalText(t, ctx, conn)
	if exit["type"] != "exit" || exit["code"] != float64(0) {
		t.Fatalf("unexpected exit frame: %#v", exit)
	}

	// The recorded session reflects a clean end with both byte counters set.
	deadline := time.Now().Add(10 * time.Second)
	for {
		sessions, err := svc.ListTerminalSessions(ctx, accountID, credential.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 1 {
			t.Fatalf("expected one session, got %d", len(sessions))
		}
		session := sessions[0]
		if session.Status == "closed" {
			if session.BytesIn <= 0 || session.BytesOut <= 0 {
				t.Fatalf("byte counters not recorded: %#v", session)
			}
			if session.StartedAt == nil || session.EndedAt == nil {
				t.Fatalf("session timestamps not recorded: %#v", session)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never closed, last state %#v", session)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The daemon audited the session with the cloud identity it was told.
	audit := string(mustRead(t, auditPath))
	if !strings.Contains(audit, `"source":"terminal"`) {
		t.Fatalf("audit entry missing terminal source: %s", audit)
	}
	if !strings.Contains(audit, `"invoked_by":"cloud:`+identity+`"`) {
		t.Fatalf("audit entry missing relayed identity: %s", audit)
	}
	if !strings.Contains(audit, `"ok":true`) {
		t.Fatalf("audit entry does not record a clean session: %s", audit)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	return data
}

// readTerminalText reads frames until the next text frame and decodes it,
// discarding intervening PTY output.
func readTerminalText(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("decode text frame %q: %v", data, err)
		}
		return frame
	}
}

// readTerminalOutput reads binary frames until the accumulated output contains
// want.
func readTerminalOutput(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) {
	t.Helper()
	var output strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for {
		if strings.Contains(output.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in shell output:\n%s", want, output.String())
		}
		readCtx, cancel := context.WithDeadline(ctx, deadline)
		typ, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			t.Fatalf("read frame while waiting for %q: %v\noutput:\n%s", want, err, output.String())
		}
		if typ != websocket.MessageBinary {
			continue
		}
		// PTY output is text; keep it readable for failure messages.
		output.Write(data)
	}
}

func writeTerminalBinary(t *testing.T, ctx context.Context, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(payload)); err != nil {
		t.Fatalf("write keystrokes: %v", err)
	}
}

func stackWriteText(t *testing.T, ctx context.Context, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("write control frame: %v", err)
	}
}
