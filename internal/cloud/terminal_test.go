package cloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

// relayDaemon registers an enabled daemon and opts it into relays.
func relayDaemon(t *testing.T, svc *Service, account, workspace, name string) Credential {
	t.Helper()
	daemon, err := svc.CreateDaemon(context.Background(), account, workspace, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDaemon(context.Background(), account, daemon.ID, nil, nil, new(true)); err != nil {
		t.Fatal(err)
	}
	return daemon
}

func TestTerminalTicketMintAndAuthenticate(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	ticket, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "/bin/bash", "deploy", 120, 36)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.SessionID == "" || ticket.Ticket == "" || ticket.DaemonID != daemon.ID {
		t.Fatalf("ticket %#v", ticket)
	}
	if ticket.ExpiresAt.Before(time.Now()) {
		t.Fatalf("ticket already expired: %v", ticket.ExpiresAt)
	}
	// Only the hash is stored; the plain ticket never lands in the row.
	var row database.TerminalSession
	if err := db.WithContext(ctx).Where("id = ?", ticket.SessionID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.TicketHash == ticket.Ticket || row.TicketHash != hashTerminalTicket(ticket.Ticket) {
		t.Fatalf("ticket hash %q", row.TicketHash)
	}
	if row.Status != terminalStatusPending || row.Columns != 120 || row.Rows != 36 {
		t.Fatalf("row %#v", row)
	}

	view, err := svc.AuthenticateTerminalTicket(ctx, daemon.ID, ticket.SessionID, ticket.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != ticket.SessionID || view.InvokedBy != "@alice" || view.Shell != "/bin/bash" {
		t.Fatalf("view %#v", view)
	}
}

func TestTerminalTicketRejections(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	ticket, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateTerminalTicket(ctx, daemon.ID, ticket.SessionID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong ticket: %v", err)
	}
	if _, err := svc.AuthenticateTerminalTicket(ctx, daemon.ID, ticket.SessionID, ticket.Ticket); err != nil {
		t.Fatal(err)
	}
	// Single use: the successful handshake consumed the hash.
	if _, err := svc.AuthenticateTerminalTicket(ctx, daemon.ID, ticket.SessionID, ticket.Ticket); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("re-used ticket: %v", err)
	}

	second, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", second.SessionID).
		Update("created_at", time.Now().Add(-2*terminalTicketTTL)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateTerminalTicket(ctx, daemon.ID, second.SessionID, second.Ticket); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired ticket: %v", err)
	}
}

func TestTerminalSessionAuthorization(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	// A non-member of the daemon's workspace cannot open a session.
	if _, err := svc.CreateTerminalSession(ctx, "account-b", daemon.ID, "@bob", "", "", 0, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member: %v", err)
	}

	// A daemon that has not opted into relays is rejected.
	plain, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTerminalSession(ctx, "account-a", plain.ID, "@alice", "", "", 0, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("relay disabled: %v", err)
	}

	// Unknown daemon.
	if _, err := svc.CreateTerminalSession(ctx, "account-a", "missing", "@alice", "", "", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown daemon: %v", err)
	}
}

func TestTerminalWorkspaceCap(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{"ws-a": {"max_daemons": 10, terminalQuotaKey: 1}}
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("cap not enforced: %v", err)
	}
}

func TestTerminalWorkspaceCapDefault(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	for i := range terminalDefaultWorkspaceSessions {
		if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("default cap not enforced: %v", err)
	}
}

func TestTerminalPickupLeaseAndReclaim(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	ticket, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "/bin/sh", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListPendingTerminalSessions(ctx, daemon.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad secret: %v", err)
	}
	pending, err := svc.ListPendingTerminalSessions(ctx, daemon.ID, daemon.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != ticket.SessionID || pending[0].Status != terminalStatusOffered {
		t.Fatalf("pickup %#v", pending)
	}
	// A fresh lease is not re-offered.
	again, err := svc.ListPendingTerminalSessions(ctx, daemon.ID, daemon.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("lease re-offered early: %#v", again)
	}
	// An expired lease is reclaimed.
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", ticket.SessionID).
		Update("leased_at", time.Now().UTC().Add(-2*terminalLeaseDuration)).Error; err != nil {
		t.Fatal(err)
	}
	reclaimed, err := svc.ListPendingTerminalSessions(ctx, daemon.ID, daemon.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != ticket.SessionID {
		t.Fatalf("expired lease not reclaimed: %#v", reclaimed)
	}
}

func TestTerminalAgentAttachAndFinish(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	ticket, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AttachTerminalAgent(ctx, daemon.ID, "wrong", ticket.SessionID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad secret: %v", err)
	}
	attached, err := svc.AttachTerminalAgent(ctx, daemon.ID, daemon.Secret, ticket.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if attached.Status != terminalStatusActive || attached.StartedAt == nil {
		t.Fatalf("attach %#v", attached)
	}
	if err := svc.FinishTerminalSession(ctx, ticket.SessionID, terminalStatusClosed, 0, "", 12, 34); err != nil {
		t.Fatal(err)
	}
	if err := svc.FinishTerminalSession(ctx, ticket.SessionID, terminalStatusClosed, 1, "", 99, 99); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListTerminalSessions(ctx, "account-a", daemon.ID, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list %v %#v", err, rows)
	}
	if rows[0].Status != terminalStatusClosed || rows[0].BytesIn != 12 || rows[0].BytesOut != 34 || rows[0].EndedAt == nil {
		t.Fatalf("finish not recorded: %#v", rows[0])
	}
}

func TestTerminalCloseSession(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	ticket, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseTerminalSession(ctx, "account-b", daemon.ID, ticket.SessionID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member close: %v", err)
	}
	if err := svc.CloseTerminalSession(ctx, "account-a", daemon.ID, ticket.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseTerminalSession(ctx, "account-a", daemon.ID, ticket.SessionID); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	rows, err := svc.ListTerminalSessions(ctx, "account-a", daemon.ID, 0)
	if err != nil || len(rows) != 1 || rows[0].Status != terminalStatusClosed {
		t.Fatalf("close state %v %#v", err, rows)
	}
}

func TestSweepTerminalSessions(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")
	now := time.Now().UTC()

	stale, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", stale.SessionID).
		Update("created_at", now.Add(-terminalPendingExpiry-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	fresh, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	old, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", old.SessionID).
		Updates(map[string]any{"status": terminalStatusClosed, "updated_at": now.Add(-terminalRetention - time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}

	if err := svc.SweepTerminalSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	var staleRow database.TerminalSession
	if err := db.WithContext(ctx).Where("id = ?", stale.SessionID).First(&staleRow).Error; err != nil {
		t.Fatal(err)
	}
	if staleRow.Status != terminalStatusFailed {
		t.Fatalf("stale session not failed: %#v", staleRow)
	}
	var freshRow database.TerminalSession
	if err := db.WithContext(ctx).Where("id = ?", fresh.SessionID).First(&freshRow).Error; err != nil {
		t.Fatal(err)
	}
	if freshRow.Status != terminalStatusPending {
		t.Fatalf("fresh session touched: %#v", freshRow)
	}
	var count int64
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", old.SessionID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired row not pruned")
	}
}

func TestSweepFailsStaleActiveSessions(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")
	now := time.Now().UTC()

	// A live session whose sockets died without either side reporting it would
	// otherwise hold a workspace session slot forever.
	abandoned, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AttachTerminalAgent(ctx, daemon.ID, daemon.Secret, abandoned.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&database.TerminalSession{}).Where("id = ?", abandoned.SessionID).
		Update("started_at", now.Add(-terminalActiveMaxAge-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	running, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AttachTerminalAgent(ctx, daemon.ID, daemon.Secret, running.SessionID); err != nil {
		t.Fatal(err)
	}

	if err := svc.SweepTerminalSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	var abandonedRow database.TerminalSession
	if err := db.WithContext(ctx).Where("id = ?", abandoned.SessionID).First(&abandonedRow).Error; err != nil {
		t.Fatal(err)
	}
	if abandonedRow.Status != terminalStatusFailed || abandonedRow.Error != "stale session" {
		t.Fatalf("stale active session not failed: %#v", abandonedRow)
	}
	if abandonedRow.EndedAt == nil {
		t.Fatal("stale active session has no end timestamp")
	}
	var runningRow database.TerminalSession
	if err := db.WithContext(ctx).Where("id = ?", running.SessionID).First(&runningRow).Error; err != nil {
		t.Fatal(err)
	}
	if runningRow.Status != terminalStatusActive {
		t.Fatalf("live session touched: %#v", runningRow)
	}
}

func TestTerminalWait(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon := relayDaemon(t, svc, "account-a", "ws-a", "host")

	// No session yet: a tiny wait must return promptly and false.
	start := time.Now()
	if svc.TerminalWait(daemon.ID)(ctx, 20*time.Millisecond) {
		t.Fatal("woke without a session")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait blocked too long: %v", elapsed)
	}

	// A create wakes a poll that is already parked.
	done := make(chan bool, 1)
	parked := svc.TerminalWait(daemon.ID)
	go func() { done <- parked(ctx, 5*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case woke := <-done:
		if !woke {
			t.Fatal("wait did not wake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not wake in time")
	}

	// A create that lands between registering and parking must not be lost.
	// The pickup caller registers, lists, then parks; this is the token the
	// create leaves behind making that park return at once instead of holding
	// the whole poll interval.
	registered := svc.TerminalWait(daemon.ID)
	if _, err := svc.CreateTerminalSession(ctx, "account-a", daemon.ID, "@alice", "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if !registered(ctx, 5*time.Second) {
		t.Fatal("park missed a create that arrived before it parked")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("park did not return promptly: %v", elapsed)
	}
}

func TestUpdateDaemonTogglesTerminalRelay(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if daemon.TerminalRelayEnabled {
		t.Fatal("relay enabled by default")
	}
	updated, err := svc.UpdateDaemon(ctx, "account-a", daemon.ID, nil, nil, new(true))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.TerminalRelayEnabled {
		t.Fatal("relay flag not set")
	}
	updated, err = svc.UpdateDaemon(ctx, "account-a", daemon.ID, nil, nil, new(false))
	if err != nil {
		t.Fatal(err)
	}
	if updated.TerminalRelayEnabled {
		t.Fatal("relay flag not cleared")
	}
}
