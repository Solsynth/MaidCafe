package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

// Terminal session lifecycle. A session is created `pending`, leased to a
// daemon long-poll as `offered`, marked `active` when the daemon's agent
// socket attaches, and ends `closed` (either socket ended) or `failed`
// (never picked up, never attached, or an error before the shell ran).
const (
	terminalStatusPending = "pending"
	terminalStatusOffered = "offered"
	terminalStatusActive  = "active"
	terminalStatusClosed  = "closed"
	terminalStatusFailed  = "failed"
)

const (
	// terminalTicketTTL is how long a minted browser ticket stays valid.
	terminalTicketTTL = 60 * time.Second
	// terminalLeaseDuration is how long a daemon may hold a leased session
	// before it is re-offered; a daemon that dies mid-pickup loses nothing.
	terminalLeaseDuration = 90 * time.Second
	// terminalPendingExpiry fails a session that is never picked up or never
	// attached to before the shell can have run.
	terminalPendingExpiry = 3 * time.Minute
	// terminalRetention prunes finished rows after this long.
	terminalRetention = 7 * 24 * time.Hour
	// terminalActiveMaxAge fails an `active` row whose sockets died without
	// either side reporting it. The daemon's own idleTimeout/maxLifetime
	// normally closes those, so a row this old is a leak, and a leak would
	// hold one of the workspace's session slots until an operator revoked it.
	terminalActiveMaxAge = 24 * time.Hour
	// terminalMaxWait caps the daemon's long-poll hold (HTTP `wait`).
	terminalMaxWait = 30 * time.Second
	// terminalMaxFrame is the largest WebSocket frame the relay forwards;
	// coder/websocket closes the connection with 1009 past it.
	terminalMaxFrame = 1 << 20
	// terminalDefaultWorkspaceSessions caps concurrent sessions per workspace
	// when the workspace service reports no terminal_sessions dimension.
	terminalDefaultWorkspaceSessions = 4
	// terminalQuotaKey is the workspace quota dimension for the cap above.
	terminalQuotaKey = "terminal_sessions"
	// terminalPendingLimit bounds how many sessions one pickup may lease.
	terminalPendingLimit = 8
)

// TerminalTicket is the one-time credential a browser uses to open its
// session socket. The plain ticket is returned once; only its hash is stored.
type TerminalTicket struct {
	SessionID string    `json:"session_id"`
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	DaemonID  string    `json:"daemon_id"`
}

// TerminalSessionView is the admin/daemon-visible view of a session. It never
// carries PTY bytes or the ticket.
type TerminalSessionView struct {
	ID        string     `json:"id"`
	DaemonID  string     `json:"daemon_id"`
	InvokedBy string     `json:"invoked_by"`
	Shell     string     `json:"shell"`
	User      string     `json:"user"`
	Cols      int        `json:"cols"`
	Rows      int        `json:"rows"`
	Status    string     `json:"status"`
	ExitCode  int        `json:"exit_code"`
	Error     string     `json:"error"`
	BytesIn   int64      `json:"bytes_in"`
	BytesOut  int64      `json:"bytes_out"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// terminalWaiter is one daemon's long-poll registration. The channel is
// buffered (size 1) so a wake that races the poll's own list may be lost
// without blocking the creator; the poll lists again after every wake.
type terminalWaiter struct {
	ch chan struct{}
	at time.Time
}

func viewTerminalSession(row database.TerminalSession) TerminalSessionView {
	return TerminalSessionView{
		ID:        row.ID,
		DaemonID:  row.DaemonID,
		InvokedBy: row.InvokedBy,
		Shell:     row.Shell,
		User:      row.User,
		Cols:      row.Columns,
		Rows:      row.Rows,
		Status:    row.Status,
		ExitCode:  row.ExitCode,
		Error:     row.Error,
		BytesIn:   row.BytesIn,
		BytesOut:  row.BytesOut,
		StartedAt: row.StartedAt,
		EndedAt:   row.EndedAt,
		CreatedAt: row.CreatedAt,
	}
}

func terminalStatusesLive() []string {
	return []string{terminalStatusPending, terminalStatusOffered, terminalStatusActive}
}

func hashTerminalTicket(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// newTerminalTicket mints a 32-byte ticket, base64url without padding.
func newTerminalTicket() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// wakeTerminalWaiters releases a daemon's long-poll, if one is parked.
func (s *Service) wakeTerminalWaiters(daemonID string) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if w, ok := s.terminalWait[daemonID]; ok {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

// CreateTerminalSession mints a ticket for an interactive shell on
// [daemonID]. It enforces the workspace concurrency cap and requires the
// daemon to have opted in with terminal_relay_enabled.
func (s *Service) CreateTerminalSession(ctx context.Context, accountID, daemonID, invokedBy, shell, user string, cols, rows int) (TerminalTicket, error) {
	d, err := s.daemonForAccount(ctx, accountID, daemonID)
	if err != nil {
		return TerminalTicket{}, err
	}
	if !d.TerminalRelayEnabled {
		return TerminalTicket{}, ErrForbidden
	}
	if cols < 0 || cols > 1000 {
		return TerminalTicket{}, fmt.Errorf("cols must be between 0 and 1000")
	}
	if rows < 0 || rows > 500 {
		return TerminalTicket{}, fmt.Errorf("rows must be between 0 and 500")
	}
	shell = strings.TrimSpace(shell)
	user = strings.TrimSpace(user)
	if len(shell) > 1024 {
		return TerminalTicket{}, fmt.Errorf("shell must be at most 1024 bytes")
	}
	if len(user) > 64 {
		return TerminalTicket{}, fmt.Errorf("user must be at most 64 bytes")
	}

	quotas, err := s.workspaceQuota(ctx, d.WorkspaceID)
	if err != nil {
		return TerminalTicket{}, err
	}
	limit := quotas[terminalQuotaKey]
	if limit <= 0 {
		limit = terminalDefaultWorkspaceSessions
	}
	var live int64
	if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("workspace_id = ? AND status IN ?", d.WorkspaceID, terminalStatusesLive()).
		Count(&live).Error; err != nil {
		return TerminalTicket{}, err
	}
	if live >= limit {
		return TerminalTicket{}, ErrRateLimited
	}

	ticket, err := newTerminalTicket()
	if err != nil {
		return TerminalTicket{}, fmt.Errorf("mint terminal ticket: %w", err)
	}
	now := time.Now().UTC()
	row := database.TerminalSession{
		ID:          uuid.NewString(),
		DaemonID:    d.ID,
		WorkspaceID: d.WorkspaceID,
		AccountID:   accountID,
		InvokedBy:   invokedBy,
		Shell:       shell,
		User:        user,
		Columns:     cols,
		Rows:        rows,
		TicketHash:  hashTerminalTicket(ticket),
		Status:      terminalStatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return TerminalTicket{}, err
	}
	s.wakeTerminalWaiters(d.ID)
	return TerminalTicket{
		SessionID: row.ID,
		Ticket:    ticket,
		ExpiresAt: now.Add(terminalTicketTTL),
		DaemonID:  d.ID,
	}, nil
}

// AuthenticateTerminalTicket validates a browser handshake and consumes the
// single-use ticket. The stored hash is cleared on success so the ticket
// cannot be replayed; a session that already ended, an expired ticket, or a
// mismatch all surface as ErrUnauthorized.
func (s *Service) AuthenticateTerminalTicket(ctx context.Context, daemonID, sessionID, ticket string) (TerminalSessionView, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || ticket == "" {
		return TerminalSessionView{}, ErrUnauthorized
	}
	var row database.TerminalSession
	if err := s.db.WithContext(ctx).Where("id = ? AND daemon_id = ?", sessionID, daemonID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TerminalSessionView{}, ErrUnauthorized
		}
		return TerminalSessionView{}, err
	}
	switch row.Status {
	case terminalStatusPending, terminalStatusOffered, terminalStatusActive:
	default:
		return TerminalSessionView{}, ErrUnauthorized
	}
	now := time.Now().UTC()
	if now.After(row.CreatedAt.Add(terminalTicketTTL)) {
		return TerminalSessionView{}, ErrUnauthorized
	}
	expected := hashTerminalTicket(ticket)
	if row.TicketHash == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(row.TicketHash)) != 1 {
		return TerminalSessionView{}, ErrUnauthorized
	}
	// Single use: consume the hash. The compare-and-swap guards a concurrent
	// handshake racing this one.
	res := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("id = ? AND ticket_hash = ?", row.ID, row.TicketHash).
		Updates(map[string]any{"ticket_hash": "", "updated_at": now})
	if res.Error != nil {
		return TerminalSessionView{}, res.Error
	}
	if res.RowsAffected == 0 {
		return TerminalSessionView{}, ErrUnauthorized
	}
	row.TicketHash = ""
	return viewTerminalSession(row), nil
}

// ListPendingTerminalSessions leases up to terminalPendingLimit pending
// sessions for the authenticated daemon. Leases older than
// terminalLeaseDuration are reclaimed first, so a daemon that died mid-pickup
// does not strand a session.
func (s *Service) ListPendingTerminalSessions(ctx context.Context, daemonID, secret string) ([]TerminalSessionView, error) {
	if _, err := s.authenticateDaemon(ctx, daemonID, secret); err != nil {
		return nil, ErrUnauthorized
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("daemon_id = ? AND status = ? AND leased_at < ?", daemonID, terminalStatusOffered, now.Add(-terminalLeaseDuration)).
		Updates(map[string]any{"status": terminalStatusPending, "leased_at": nil, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	var rows []database.TerminalSession
	if err := s.db.WithContext(ctx).
		Where("daemon_id = ? AND status = ? AND created_at > ?", daemonID, terminalStatusPending, now.Add(-terminalPendingExpiry)).
		Order("created_at ASC").Limit(terminalPendingLimit).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []TerminalSessionView{}, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("daemon_id = ? AND id IN ? AND status = ?", daemonID, ids, terminalStatusPending).
		Updates(map[string]any{"status": terminalStatusOffered, "leased_at": now, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	views := make([]TerminalSessionView, 0, len(rows))
	for _, row := range rows {
		row.Status = terminalStatusOffered
		row.LeasedAt = &now
		views = append(views, viewTerminalSession(row))
	}
	return views, nil
}

// AttachTerminalAgent marks the daemon's agent socket attached, moving the
// session to `active` and stamping started_at. A finished session is gone.
func (s *Service) AttachTerminalAgent(ctx context.Context, daemonID, secret, sessionID string) (TerminalSessionView, error) {
	if _, err := s.authenticateDaemon(ctx, daemonID, secret); err != nil {
		return TerminalSessionView{}, ErrUnauthorized
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return TerminalSessionView{}, ErrNotFound
	}
	var row database.TerminalSession
	if err := s.db.WithContext(ctx).Where("id = ? AND daemon_id = ?", sessionID, daemonID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TerminalSessionView{}, ErrNotFound
		}
		return TerminalSessionView{}, err
	}
	switch row.Status {
	case terminalStatusPending, terminalStatusOffered, terminalStatusActive:
	default:
		return TerminalSessionView{}, ErrNotFound
	}
	if row.Status != terminalStatusActive || row.StartedAt == nil {
		now := time.Now().UTC()
		if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{"status": terminalStatusActive, "started_at": now, "updated_at": now}).Error; err != nil {
			return TerminalSessionView{}, err
		}
		row.Status = terminalStatusActive
		row.StartedAt = &now
	}
	return viewTerminalSession(row), nil
}

// FinishTerminalSession records the end of a session. It is idempotent: a row
// that already ended is left untouched. [failure] is a fixed close reason.
func (s *Service) FinishTerminalSession(ctx context.Context, sessionID, status string, exitCode int, failure string, bytesIn, bytesOut int64) error {
	if status != terminalStatusClosed && status != terminalStatusFailed {
		return fmt.Errorf("invalid terminal status %q", status)
	}
	if len(failure) > 512 {
		failure = failure[:512]
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("id = ? AND status IN ?", sessionID, terminalStatusesLive()).
		Updates(map[string]any{
			"status":     status,
			"exit_code":  exitCode,
			"error":      failure,
			"bytes_in":   bytesIn,
			"bytes_out":  bytesOut,
			"ended_at":   now,
			"updated_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	return nil
}

// ListTerminalSessions returns a daemon's recent sessions to a workspace
// member (admin visibility only; no bytes or tickets).
func (s *Service) ListTerminalSessions(ctx context.Context, accountID, daemonID string, limit int) ([]TerminalSessionView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	var rows []database.TerminalSession
	if err := s.db.WithContext(ctx).Where("daemon_id = ?", daemonID).
		Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]TerminalSessionView, len(rows))
	for i := range rows {
		views[i] = viewTerminalSession(rows[i])
	}
	return views, nil
}

// CloseTerminalSession revokes a session on behalf of a workspace member. The
// caller closes the in-memory sockets; this only records the end state.
func (s *Service) CloseTerminalSession(ctx context.Context, accountID, daemonID, sessionID string) error {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return err
	}
	var row database.TerminalSession
	if err := s.db.WithContext(ctx).Where("id = ? AND daemon_id = ?", sessionID, daemonID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	switch row.Status {
	case terminalStatusClosed, terminalStatusFailed:
		return nil
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("id = ? AND status IN ?", row.ID, terminalStatusesLive()).
		Updates(map[string]any{"status": terminalStatusClosed, "ended_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	return nil
}

// SweepTerminalSessions fails sessions that were never picked up or attached,
// fails sessions left `active` past terminalActiveMaxAge, and prunes finished
// rows past retention. It runs at startup and every minute from the cloud main
// loop.
func (s *Service) SweepTerminalSessions(ctx context.Context, now time.Time) error {
	if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("status IN ? AND created_at < ?", []string{terminalStatusPending, terminalStatusOffered}, now.Add(-terminalPendingExpiry)).
		Updates(map[string]any{
			"status":     terminalStatusFailed,
			"error":      "not picked up",
			"ended_at":   now,
			"updated_at": now,
		}).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Model(&database.TerminalSession{}).
		Where("status = ? AND COALESCE(started_at, created_at) < ?", terminalStatusActive, now.Add(-terminalActiveMaxAge)).
		Updates(map[string]any{
			"status":     terminalStatusFailed,
			"error":      "stale session",
			"ended_at":   now,
			"updated_at": now,
		}).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).
		Where("status IN ? AND updated_at < ?", []string{terminalStatusClosed, terminalStatusFailed}, now.Add(-terminalRetention)).
		Delete(&database.TerminalSession{}).Error; err != nil {
		return err
	}
	return nil
}

// TerminalWait registers a pickup long-poll for [daemonID] and returns the
// function that parks it until a session is created for that daemon, the
// timeout elapses, or ctx is done; it reports whether a create woke it.
//
// Registering is deliberately separate from parking. The pickup caller
// registers, then lists, then parks: a session created while it is listing
// leaves a token in the buffered channel, so the park returns immediately
// instead of sleeping a whole poll interval. Registering after the list would
// drop that wake-up and delay the shell by up to pollWait.
func (s *Service) TerminalWait(daemonID string) func(context.Context, time.Duration) bool {
	s.terminalMu.Lock()
	w, ok := s.terminalWait[daemonID]
	if !ok {
		w = &terminalWaiter{ch: make(chan struct{}, 1), at: time.Now()}
		s.terminalWait[daemonID] = w
	} else {
		w.at = time.Now()
	}
	s.terminalMu.Unlock()

	return func(ctx context.Context, timeout time.Duration) bool {
		defer s.releaseTerminalWait(daemonID, w)
		if timeout <= 0 {
			return false
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-w.ch:
			return true
		}
	}
}

// releaseTerminalWait drops one registration, pruning entries that have gone
// quiet so a long-lived cloud does not accumulate a channel per daemon id.
func (s *Service) releaseTerminalWait(daemonID string, w *terminalWaiter) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if s.terminalWait[daemonID] == w {
		delete(s.terminalWait, daemonID)
	}
	if len(s.terminalWait) > 10_000 {
		cutoff := time.Now().Add(-time.Hour)
		for id, other := range s.terminalWait {
			if other.at.Before(cutoff) {
				delete(s.terminalWait, id)
			}
		}
	}
}
