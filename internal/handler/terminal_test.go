package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
)

type terminalWorkspaces struct{}

func (terminalWorkspaces) IsMemberWithRole(_ context.Context, workspaceID, accountID string, _ []int32) (bool, error) {
	return workspaceID == "ws-a" && accountID == "account-a", nil
}

func (terminalWorkspaces) GetPlanQuota(_ context.Context, _ string) (map[string]int64, error) {
	return map[string]int64{"max_daemons": 10}, nil
}

type terminalHarness struct {
	engine *gin.Engine
	server *httptest.Server
	svc    *cloud.Service
	db     *database.DB
	daemon cloud.Credential
}

func newTerminalHarness(t *testing.T, cfg *config.Config, withCredential bool) *terminalHarness {
	t.Helper()
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, nil, terminalWorkspaces{})
	daemon, err := svc.CreateDaemon(context.Background(), "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	userAuth := func(c *gin.Context) {
		dyauth.WithAuth(c, &dyauth.AuthResult{Account: &gen.DyAccount{Id: "account-a"}}, dyauth.TokenInfo{})
		if withCredential {
			WithCredential(c, &database.Credential{ID: "cred", AccountID: "account-a", Label: "ci"})
		}
		c.Next()
	}
	RegisterRoutes(engine, svc, userAuth, cfg)
	server := httptest.NewServer(engine)
	h := &terminalHarness{engine: engine, server: server, svc: svc, db: db, daemon: daemon}
	t.Cleanup(h.close)
	return h
}

func (h *terminalHarness) close() {
	h.server.Close()
	h.db.Close()
}

func (h *terminalHarness) enableRelay(t *testing.T) {
	t.Helper()
	if _, err := h.svc.UpdateDaemon(context.Background(), "account-a", h.daemon.ID, nil, nil, new(true)); err != nil {
		t.Fatal(err)
	}
}

func (h *terminalHarness) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + path
}

func (h *terminalHarness) mint(t *testing.T) cloud.TerminalTicket {
	t.Helper()
	ticket, err := h.svc.CreateTerminalSession(context.Background(), "account-a", h.daemon.ID, "@alice", "/bin/sh", "", 120, 40)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func terminalToken(ticket cloud.TerminalTicket) string {
	return "maidcafe.terminal." + base64.RawURLEncoding.EncodeToString([]byte(ticket.SessionID+"."+ticket.Ticket))
}

func readFrame(t *testing.T, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return typ, data
}

// TestTerminalRelayEndToEnd drives a session through the real HTTP server:
// browser socket with the ticket subprotocol, agent socket with the daemon
// secret, and frames forwarded verbatim in both directions.
func TestTerminalRelayEndToEnd(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)
	h.enableRelay(t)
	ticket := h.mint(t)
	token := terminalToken(ticket)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	browser, resp, err := websocket.Dial(dialCtx, h.wsURL("/api/daemons/"+h.daemon.ID+"/terminal"), &websocket.DialOptions{
		Subprotocols: []string{token},
	})
	if err != nil {
		t.Fatalf("browser dial: %v (%v)", err, resp)
	}
	defer browser.CloseNow()
	if got := browser.Subprotocol(); got != token {
		t.Fatalf("negotiated subprotocol %q", got)
	}

	agent, resp, err := websocket.Dial(dialCtx, h.wsURL("/api/daemons/"+h.daemon.ID+"/terminal/agent?session="+ticket.SessionID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + h.daemon.Secret}},
	})
	if err != nil {
		t.Fatalf("agent dial: %v (%v)", err, resp)
	}
	defer agent.CloseNow()

	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()

	hello := []byte("hello-pty")
	if err := browser.Write(writeCtx, websocket.MessageBinary, hello); err != nil {
		t.Fatal(err)
	}
	if typ, data := readFrame(t, agent); typ != websocket.MessageBinary || string(data) != string(hello) {
		t.Fatalf("browser->agent binary mismatch: %v %q", typ, data)
	}

	resize := []byte(`{"type":"resize","cols":100,"rows":30}`)
	if err := browser.Write(writeCtx, websocket.MessageText, resize); err != nil {
		t.Fatal(err)
	}
	if typ, data := readFrame(t, agent); typ != websocket.MessageText || string(data) != string(resize) {
		t.Fatalf("browser->agent text mismatch: %v %q", typ, data)
	}

	output := []byte("world-pty")
	if err := agent.Write(writeCtx, websocket.MessageBinary, output); err != nil {
		t.Fatal(err)
	}
	if typ, data := readFrame(t, browser); typ != websocket.MessageBinary || string(data) != string(output) {
		t.Fatalf("agent->browser binary mismatch: %v %q", typ, data)
	}

	exit := []byte(`{"type":"exit","code":7,"reason":""}`)
	if err := agent.Write(writeCtx, websocket.MessageText, exit); err != nil {
		t.Fatal(err)
	}
	if typ, data := readFrame(t, browser); typ != websocket.MessageText || string(data) != string(exit) {
		t.Fatalf("agent->browser text mismatch: %v %q", typ, data)
	}

	// Ending either socket closes both and records the end state once.
	_ = browser.Close(websocket.StatusNormalClosure, "")

	wantIn := int64(len(hello) + len(resize))
	wantOut := int64(len(output) + len(exit))
	deadline := time.Now().Add(5 * time.Second)
	for {
		var row database.TerminalSession
		err := h.db.WithContext(context.Background()).Where("id = ?", ticket.SessionID).First(&row).Error
		if err == nil && row.Status == "closed" {
			if row.BytesIn != wantIn || row.BytesOut != wantOut {
				t.Fatalf("byte counts in=%d out=%d, want %d/%d", row.BytesIn, row.BytesOut, wantIn, wantOut)
			}
			if row.ExitCode != 7 {
				t.Fatalf("exit code %d, want 7", row.ExitCode)
			}
			if row.EndedAt == nil || row.StartedAt == nil {
				t.Fatalf("session timestamps missing: %#v", row)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session not closed: %v %#v", err, row)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTerminalBrowserRejectsBadTicket(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)
	h.enableRelay(t)
	ticket := h.mint(t)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// No credential at all.
	if _, resp, err := websocket.Dial(dialCtx, h.wsURL("/api/daemons/"+h.daemon.ID+"/terminal"), nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credential: %v %v", err, resp)
	}
	// Wrong ticket for a real session.
	bad := "maidcafe.terminal." + base64.RawURLEncoding.EncodeToString([]byte(ticket.SessionID+".nope"))
	if _, resp, err := websocket.Dial(dialCtx, h.wsURL("/api/daemons/"+h.daemon.ID+"/terminal"), &websocket.DialOptions{Subprotocols: []string{bad}}); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad ticket: %v %v", err, resp)
	}
}

func TestTerminalPendingRouteReturnsImmediately(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)
	h.enableRelay(t)
	ticket := h.mint(t)

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/api/daemons/"+h.daemon.ID+"/terminal/requests/pending?wait=50ms&limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+h.daemon.Secret)
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending route %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Sessions []cloud.TerminalSessionView `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].ID != ticket.SessionID || body.Sessions[0].Status != "offered" {
		t.Fatalf("pending response %#v", body.Sessions)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("pending route held for %v", elapsed)
	}
}

func TestTerminalCreateRejectsCredential(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, true)
	h.enableRelay(t)

	req := httptest.NewRequest(http.MethodPost, "/api/daemons/"+h.daemon.ID+"/terminal", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("credential create %d %s", rec.Code, rec.Body)
	}
}

func TestTerminalCreateRejectsRelayDisabled(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)

	req := httptest.NewRequest(http.MethodPost, "/api/daemons/"+h.daemon.ID+"/terminal", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("relay-disabled create %d %s", rec.Code, rec.Body)
	}
}

func TestTerminalCreateWorkspaceCap(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)
	h.enableRelay(t)
	// Fill the default workspace cap (4 concurrent sessions).
	for range 4 {
		h.mint(t)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/daemons/"+h.daemon.ID+"/terminal", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("cap create %d %s", rec.Code, rec.Body)
	}
}

func TestTerminalCreateAndListRoutes(t *testing.T) {
	h := newTerminalHarness(t, &config.Config{}, false)
	h.enableRelay(t)

	req := httptest.NewRequest(http.MethodPost, "/api/daemons/"+h.daemon.ID+"/terminal", strings.NewReader(`{"shell":"/bin/bash","cols":100,"rows":30}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var ticket cloud.TerminalTicket
	if err := json.Unmarshal(rec.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.SessionID == "" || ticket.Ticket == "" || ticket.DaemonID != h.daemon.ID {
		t.Fatalf("ticket %#v", ticket)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/daemons/"+h.daemon.ID+"/terminals", nil)
	listRec := httptest.NewRecorder()
	h.engine.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list %d %s", listRec.Code, listRec.Body)
	}
	if !strings.Contains(listRec.Body.String(), ticket.SessionID) {
		t.Fatalf("list missing session: %s", listRec.Body)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/daemons/"+h.daemon.ID+"/terminal/"+ticket.SessionID, nil)
	delRec := httptest.NewRecorder()
	h.engine.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("revoke %d %s", delRec.Code, delRec.Body)
	}
}
