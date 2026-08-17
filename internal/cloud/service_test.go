package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

type fakePublisher struct{ events []NotificationEvent }

func (f *fakePublisher) Publish(_ context.Context, event NotificationEvent) error {
	f.events = append(f.events, event)
	return nil
}

// fakeWorkspaces grants membership to the accounts listed for each workspace
// and serves per-workspace quota maps (defaulting to a generous daemon limit).
type fakeWorkspaces struct {
	members map[string][]string
	quotas  map[string]map[string]int64
	err     error
}

func (f *fakeWorkspaces) IsMemberWithRole(_ context.Context, workspaceID, accountID string, requiredRoles []int32) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	for _, account := range f.members[workspaceID] {
		if account == accountID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeWorkspaces) GetPlanQuota(_ context.Context, workspaceID string) (map[string]int64, error) {
	if f.err != nil {
		return nil, f.err
	}
	if quota, ok := f.quotas[workspaceID]; ok {
		return quota, nil
	}
	return map[string]int64{"max_daemons": 10}, nil
}

func testService(t *testing.T) (*Service, *database.DB, *fakePublisher, *fakeWorkspaces) {
	t.Helper()
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{}
	workspaces := &fakeWorkspaces{members: map[string][]string{"ws-a": {"account-a"}, "ws-b": {"account-a"}}}
	return NewService(db, publisher, workspaces), db, publisher, workspaces
}
func TestDaemonCredentialsOwnershipAndRotation(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" {
		t.Fatalf("missing one-time credential: %#v", created)
	}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, MetricInput{SentAt: time.Now(), UptimeSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDaemon(ctx, "account-b", created.ID); err != ErrForbidden {
		t.Fatalf("expected account isolation, got %v", err)
	}
	rotated, err := svc.RotateSecret(ctx, "account-a", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, MetricInput{SentAt: time.Now()}); err != ErrUnauthorized {
		t.Fatalf("old secret accepted: %v", err)
	}
	if err := svc.IngestMetric(ctx, created.ID, rotated, MetricInput{SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DisableDaemon(ctx, "account-a", created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, created.ID, rotated, MetricInput{SentAt: time.Now()}); err != ErrUnauthorized {
		t.Fatalf("disabled daemon accepted: %v", err)
	}
	history, err := svc.ListMetrics(ctx, "account-a", created.ID, 100, nil)
	if err != nil || len(history) != 2 {
		t.Fatalf("disabled daemon lost metric history: %v %#v", err, history)
	}
}
func TestNotificationPersistenceAndEventPublication(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	notification, err := svc.CreateNotification(ctx, daemon.ID, daemon.Secret, NotificationInput{Kind: "webhook.failure", Title: "failed", Subtitle: "nightly", Body: "details", Metadata: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 1 || publisher.events[0].NotificationID != notification.ID {
		t.Fatalf("event mismatch: %#v", publisher.events)
	}
	event := publisher.events[0]
	if event.Subtitle != "From host: nightly" {
		t.Fatalf("event subtitle missing daemon source: %#v", event)
	}
	var meta map[string]any
	if err := json.Unmarshal(event.Metadata, &meta); err != nil {
		t.Fatalf("event metadata: %v", err)
	}
	if meta["daemon_id"] != daemon.ID || meta["daemon_name"] != "host" || meta["x"] != float64(1) {
		t.Fatalf("unexpected enriched metadata: %#v", meta)
	}
	rows, err := svc.ListNotifications(ctx, "account-a", "ws-a", true, "", 50, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("unread listing: %v %#v", err, rows)
	}
	if err := svc.MarkNotificationRead(ctx, "account-a", notification.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkNotificationRead(ctx, "account-a", notification.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.ListNotifications(ctx, "account-a", "ws-a", true, "", 50, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("read acknowledgement: %v %#v", err, rows)
	}
	if _, err := svc.ListNotifications(ctx, "account-b", "ws-a", false, "", 50, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member listing expected forbidden, got %v", err)
	}
}
func TestMetricHistoryIsOwnedAndOrdered(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-time.Minute)
	second := time.Now().UTC()
	for _, sentAt := range []time.Time{first, second} {
		if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{SentAt: sentAt, UptimeSeconds: 5}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := svc.ListMetrics(ctx, "account-a", daemon.ID, 100, nil)
	if err != nil || len(history) != 2 || !history[0].SentAt.After(history[1].SentAt) {
		t.Fatalf("metric history: %v %#v", err, history)
	}
	if _, err := svc.ListMetrics(ctx, "account-b", daemon.ID, 100, nil); err != ErrForbidden {
		t.Fatalf("expected metric ownership error, got %v", err)
	}
}
func TestMetricIngestAndPushRequestPersistence(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	// The daemon reports the full MetricsPayload (load/swap/disk/net extras);
	// the strict decoder must accept it or every push 400s and last_seen_at
	// never updates.
	now := time.Now().UTC()
	if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{
		SentAt: now, UptimeSeconds: 42, ProcessMemoryBytes: 1024,
		CPUPercent: 12.5, CPUCount: 4,
		Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemoryUsedPercent: 55, MemoryUsedBytes: 1 << 30, MemoryTotalBytes: 2 << 30,
		SwapTotalKb: 1024, SwapFreeKb: 512,
		DiskTotalKb: 102400, DiskAvailableKb: 51200,
		NetRxBytes: 100, NetTxBytes: 200,
		WebhookExecutions: 2, WebhookFailures: 1,
	}); err != nil {
		t.Fatalf("full daemon payload rejected: %v", err)
	}
	// Alarms are evaluated daemon-side; ingest itself never publishes.
	if len(publisher.events) != 0 {
		t.Fatalf("metric ingest published %d events, want 0", len(publisher.events))
	}
	history, err := svc.ListMetrics(ctx, "account-a", daemon.ID, 100, nil)
	if err != nil || len(history) != 1 {
		t.Fatalf("metric history: %v %#v", err, history)
	}
	m := history[0]
	if m.CPUCount != 4 || m.Load1 != 0.5 || m.SwapTotalKb != 1024 ||
		m.DiskAvailableKb != 51200 || m.NetTxBytes != 200 {
		t.Fatalf("metric extras not stored: %+v", m)
	}
	var row database.Daemon
	if err := db.WithContext(ctx).Where("id = ?", daemon.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastSeenAt == nil || row.LastSeenAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("last_seen_at not updated: %+v", row)
	}
	requested, err := svc.CreatePushNotification(ctx, "account-a", daemon.ID, NotificationInput{Kind: "maintenance", Title: "Restart", Body: "Restart after backup"})
	if err != nil || requested.Title != "Restart" {
		t.Fatalf("push request: %v %#v", err, requested)
	}
	if len(publisher.events) != 1 || publisher.events[0].Kind != "maintenance" {
		t.Fatalf("notification publication mismatch: %#v", publisher.events)
	}
	if publisher.events[0].Subtitle != "From host" {
		t.Fatalf("push subtitle missing daemon source: %#v", publisher.events[0])
	}
}

func TestEvaluateDisconnectedDaemonsTransitionsAndPublishes(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	svc.accounts = languageAccountClient{language: "zh-TW"}
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-10 * time.Minute)
	if err := db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", daemon.ID).
		Updates(map[string]any{"last_seen_at": lastSeen}).Error; err != nil {
		t.Fatal(err)
	}

	if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, 5*time.Minute, 5*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 1 || publisher.events[0].Kind != "daemon.disconnected" {
		t.Fatalf("disconnect notification: %#v", publisher.events)
	}
	if publisher.events[0].DaemonID != daemon.ID || publisher.events[0].NotificationID == "" {
		t.Fatalf("disconnect event identity: %#v", publisher.events[0])
	}
	var metadata map[string]any
	if err := json.Unmarshal(publisher.events[0].Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["threshold_seconds"] != float64(300) || metadata["daemon_id"] != daemon.ID {
		t.Fatalf("disconnect event metadata: %#v", metadata)
	}
	var row database.Daemon
	if err := db.WithContext(ctx).Where("id = ?", daemon.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.DisconnectedAt == nil {
		t.Fatalf("daemon was not marked disconnected: %+v", row)
	}
	if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, 5*time.Minute, 5*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("duplicate disconnect notification: %#v", publisher.events)
	}

	if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{SentAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	row = database.Daemon{}
	if err := db.WithContext(ctx).Where("id = ?", daemon.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.DisconnectedAt != nil {
		t.Fatalf("metric did not recover daemon: %+v", row)
	}
	if len(publisher.events) != 2 || publisher.events[1].Kind != "daemon.reconnected" ||
		publisher.events[1].Title != "守護程式已重新連線" ||
		!strings.HasPrefix(publisher.events[1].Body, "指標已恢復回報，間隔 ") {
		t.Fatalf("reconnect notification: %#v", publisher.events)
	}

	if err := db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", daemon.ID).
		Updates(map[string]any{"last_seen_at": now.Add(-10 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, 5*time.Minute, 5*time.Minute, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 2 {
		t.Fatalf("disconnect cooldown failed: %#v", publisher.events)
	}

	if err := db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", daemon.ID).
		Updates(map[string]any{"disconnected_at": nil, "last_seen_at": now.Add(-10 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, 5*time.Minute, 5*time.Minute, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 3 {
		t.Fatalf("outage did not re-alarm after cooldown: %#v", publisher.events)
	}
}

func TestEvaluateDisconnectedDaemonsIgnoresNeverSeenAndDisabled(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	neverSeen, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "never-seen")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DisableDaemon(ctx, "account-a", disabled.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, time.Minute, time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 0 {
		t.Fatalf("unexpected notification for never-seen/disabled daemons: %#v", publisher.events)
	}
	var row database.Daemon
	if err := db.WithContext(ctx).Where("id = ?", neverSeen.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.DisconnectedAt != nil {
		t.Fatalf("never-seen daemon marked disconnected: %+v", row)
	}
}

func TestActionSyncAndList(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	// The daemon reports its actions on every metrics tick.
	if err := svc.SyncActions(ctx, daemon.ID, daemon.Secret, []ActionInput{
		{Name: "backup", DisplayName: "Backup data", Enabled: true, NotifyOnFailure: true, Timeout: "2m", User: "root"},
		{Name: "cleanup", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.ListActions(ctx, "account-a", daemon.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list actions: %v %#v", err, listed)
	}
	backup, cleanup := listed[0], listed[1]
	if backup.Name != "backup" || backup.DisplayName != "Backup data" ||
		!backup.Enabled || !backup.NotifyOnFailure || backup.Timeout != "2m" ||
		backup.User != "root" {
		t.Fatalf("backup action not stored: %+v", backup)
	}
	if cleanup.Name != "cleanup" || cleanup.Enabled {
		t.Fatalf("cleanup action not stored: %+v", cleanup)
	}
	// Re-syncing replaces the list; an empty report clears it.
	if err := svc.SyncActions(ctx, daemon.ID, daemon.Secret, nil); err != nil {
		t.Fatal(err)
	}
	listed, err = svc.ListActions(ctx, "account-a", daemon.ID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("cleared actions: %v %#v", err, listed)
	}
	// Non-members cannot list; bad secrets cannot sync.
	if _, err := svc.ListActions(ctx, "account-b", daemon.ID); err != ErrForbidden {
		t.Fatalf("non-member listing expected forbidden, got %v", err)
	}
	if err := svc.SyncActions(ctx, daemon.ID, "wrong-secret", []ActionInput{{Name: "x", Enabled: true}}); err != ErrUnauthorized {
		t.Fatalf("bad daemon secret expected unauthorized, got %v", err)
	}
	if err := svc.SyncActions(ctx, daemon.ID, daemon.Secret, []ActionInput{{Name: "", Enabled: true}}); err == nil {
		t.Fatalf("empty action name accepted")
	}
	if err := svc.SyncActions(ctx, daemon.ID, daemon.Secret, []ActionInput{{Name: "dup", Enabled: true}, {Name: "dup", Enabled: true}}); err == nil {
		t.Fatalf("duplicate action names accepted")
	}
}

func TestWebhookRelayLifecycle(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("backup now")
	signature := "abc123"
	enqueued, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "backup", body, signature, "@alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued.Status != "pending" || enqueued.Name != "backup" || enqueued.Body != base64.StdEncoding.EncodeToString(body) {
		t.Fatalf("enqueued view = %#v", enqueued)
	}
	if _, err := svc.EnqueueWebhook(ctx, "account-b", created.ID, "backup", body, signature, "@bob", nil); err != ErrForbidden {
		t.Fatalf("account isolation failed: %v", err)
	}
	if _, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "", body, signature, "@alice", nil); err == nil {
		t.Fatalf("empty name accepted")
	}
	// Actions carry no secret, so invocation goes through the relay without
	// a signature; the daemon verifies webhooks at execution.
	actionRequest, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "cleanup", []byte("{}"), "", "cleanup-bot", nil)
	if err != nil {
		t.Fatalf("signature-less action enqueue rejected: %v", err)
	}
	if actionRequest.Signature != "" || actionRequest.InvokedBy != "cleanup-bot" {
		t.Fatalf("action request metadata: %#v", actionRequest)
	}

	pending, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != enqueued.ID || pending[0].Signature != signature {
		t.Fatalf("pending = %#v", pending)
	}
	// Leased requests are not returned again.
	pendingAgain, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingAgain) != 0 {
		t.Fatalf("leased request re-returned: %#v", pendingAgain)
	}
	if _, err := svc.ListPendingWebhooks(ctx, created.ID, "wrong-secret", 10); err != ErrUnauthorized {
		t.Fatalf("bad daemon secret: %v", err)
	}

	if err := svc.CompleteWebhook(ctx, created.ID, created.Secret, enqueued.ID, 200, []byte("ok"), ""); err != nil {
		t.Fatal(err)
	}
	result, err := svc.GetWebhookResult(ctx, "account-a", created.ID, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" || result.ResultCode != 200 || result.ResultBody != base64.StdEncoding.EncodeToString([]byte("ok")) {
		t.Fatalf("result = %#v", result)
	}
	if _, err := svc.GetWebhookResult(ctx, "account-b", created.ID, enqueued.ID); err != ErrForbidden {
		t.Fatalf("result isolation failed: %v", err)
	}
}

func TestWebhookRelayReclaimsExpiredLeases(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "backup", []byte("x"), "sig", "@alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-10 * time.Minute)
	if err := db.Model(&database.WebhookRequest{}).Where("id = ?", enqueued.ID).
		Update("leased_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != enqueued.ID {
		t.Fatalf("expired lease not reclaimed: %#v", pending)
	}
}

func TestWorkspaceMembershipGatesDaemonAccess(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()

	// account-b is not a member of ws-a: creation must fail closed.
	if _, err := svc.CreateDaemon(ctx, "account-b", "ws-a", "intruder"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member create expected forbidden, got %v", err)
	}
	if _, err := svc.ListDaemons(ctx, "account-b", "ws-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member list expected forbidden, got %v", err)
	}

	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceID != "ws-a" {
		t.Fatalf("daemon workspace not persisted: %#v", created)
	}
	// Direct id access without membership is forbidden, not just creation.
	if _, err := svc.GetDaemon(ctx, "account-b", created.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member read expected forbidden, got %v", err)
	}
	if _, err := svc.ListMetrics(ctx, "account-b", created.ID, 10, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member metrics expected forbidden, got %v", err)
	}

	// Daemons only appear in their own workspace listing.
	daemons, err := svc.ListDaemons(ctx, "account-a", "ws-b")
	if err != nil || len(daemons) != 0 {
		t.Fatalf("workspace isolation: %v %#v", err, daemons)
	}
	daemons, err = svc.ListDaemons(ctx, "account-a", "ws-a")
	if err != nil || len(daemons) != 1 || daemons[0].ID != created.ID {
		t.Fatalf("workspace listing: %v %#v", err, daemons)
	}
}

func TestWorkspaceServiceFailureFailsClosed(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.err = errors.New("workspace service unreachable")

	if _, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host"); err == nil {
		t.Fatal("creation granted while workspace service is down")
	}
	// Seed a daemon row directly so the read path is exercised.
	if err := db.Create(&database.Daemon{ID: "d-1", AccountID: "account-a", WorkspaceID: "ws-a", Name: "host", SecretHash: "hash", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDaemon(ctx, "account-a", "d-1"); err == nil {
		t.Fatal("read granted while workspace service is down")
	}
}

func TestCreateDaemonRequiresWorkspace(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	if _, err := svc.CreateDaemon(ctx, "account-a", "", "host"); err == nil {
		t.Fatal("daemon created without workspace_id")
	}
}

func TestCreateDaemonEnforcesQuotaLimit(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{"ws-a": {"max_daemons": 1}}

	if _, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "first"); err != nil {
		t.Fatalf("first daemon should be created: %v", err)
	}
	if _, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "second"); err == nil {
		t.Fatal("second daemon created past the quota limit")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("unexpected error: %v", err)
	}

	// A different workspace with a higher limit is unaffected.
	if _, err := svc.CreateDaemon(ctx, "account-a", "ws-b", "host"); err != nil {
		t.Fatalf("ws-b should be unaffected by ws-a's limit: %v", err)
	}
}

func TestMetricIngestRateLimited(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{"ws-a": {"max_daemons": 10, "polling_interval_seconds": 3600}}

	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, MetricInput{SentAt: time.Now(), UptimeSeconds: 1}); err != nil {
		t.Fatalf("first ingest should be accepted: %v", err)
	}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, MetricInput{SentAt: time.Now(), UptimeSeconds: 2}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}

	// A daemon in a workspace without the dimension is not throttled.
	other, err := svc.CreateDaemon(ctx, "account-a", "ws-b", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, other.ID, other.Secret, MetricInput{SentAt: time.Now(), UptimeSeconds: 1}); err != nil {
		t.Fatalf("ws-b ingest should not be throttled: %v", err)
	}
}

func TestMetricIngestRejectsOutOfRetention(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{"ws-a": {"max_daemons": 10, "metrics_retention_days": 1}}

	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	stale := MetricInput{SentAt: time.Now().Add(-48 * time.Hour), UptimeSeconds: 1}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, stale); !errors.Is(err, ErrMetricOutOfRetention) {
		t.Fatalf("expected ErrMetricOutOfRetention, got %v", err)
	}
	fresh := MetricInput{SentAt: time.Now(), UptimeSeconds: 1}
	if err := svc.IngestMetric(ctx, created.ID, created.Secret, fresh); err != nil {
		t.Fatalf("fresh metric should be accepted: %v", err)
	}
}

func TestWebhookRelayPickupRateLimited(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{"ws-a": {"max_daemons": 10, "polling_interval_seconds": 3600}}

	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10); err != nil {
		t.Fatalf("first pickup should be accepted: %v", err)
	}
	if _, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestPruneMetricsPerWorkspaceRetention(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{
		"ws-a": {"max_daemons": 10, "metrics_retention_days": 1},
		"ws-b": {"max_daemons": 10}, // no retention dimension — keep everything
	}
	now := time.Now().UTC()

	a, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateDaemon(ctx, "account-a", "ws-b", "host")
	if err != nil {
		t.Fatal(err)
	}

	rows := []database.DaemonMetric{
		{ID: "m-a-old", DaemonID: a.ID, SentAt: now.Add(-72 * time.Hour), ReceivedAt: now.Add(-72 * time.Hour)},
		{ID: "m-a-new", DaemonID: a.ID, SentAt: now, ReceivedAt: now},
		{ID: "m-b-old", DaemonID: b.ID, SentAt: now.Add(-72 * time.Hour), ReceivedAt: now.Add(-72 * time.Hour)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	if err := svc.PruneMetrics(ctx); err != nil {
		t.Fatal(err)
	}

	var remaining []string
	if err := db.Model(&database.DaemonMetric{}).Pluck("id", &remaining).Error; err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"m-a-new": true, "m-b-old": true}
	if len(remaining) != len(want) {
		t.Fatalf("expected %v remaining, got %v", want, remaining)
	}
	for _, id := range remaining {
		if !want[id] {
			t.Fatalf("unexpected remaining metric %s", id)
		}
	}
}

func TestDaemonQuotaEndpoint(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{
		"ws-a": {"max_daemons": 5, "polling_interval_seconds": 30, "metrics_retention_days": 30},
	}

	created, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	view, err := svc.GetDaemonQuota(ctx, created.ID, created.Secret)
	if err != nil {
		t.Fatalf("daemon quota should be served: %v", err)
	}
	if view.WorkspaceID != "ws-a" {
		t.Fatalf("expected workspace ws-a, got %s", view.WorkspaceID)
	}
	want := map[string]int64{"max_daemons": 5, "polling_interval_seconds": 30, "metrics_retention_days": 30}
	if len(view.Quotas) != len(want) {
		t.Fatalf("expected quotas %v, got %v", want, view.Quotas)
	}
	for k, v := range want {
		if view.Quotas[k] != v {
			t.Fatalf("expected quotas[%s] = %d, got %d", k, v, view.Quotas[k])
		}
	}

	if _, err := svc.GetDaemonQuota(ctx, created.ID, "wrong-secret"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for bad secret, got %v", err)
	}
}

func TestWorkspaceQuotaEndpointForAccount(t *testing.T) {
	svc, db, _, workspaces := testService(t)
	defer db.Close()
	ctx := context.Background()
	workspaces.quotas = map[string]map[string]int64{
		"ws-a": {"max_daemons": 5, "polling_interval_seconds": 30, "metrics_retention_days": 30},
	}

	view, err := svc.GetWorkspaceQuota(ctx, "account-a", "ws-a")
	if err != nil {
		t.Fatalf("member quota should be served: %v", err)
	}
	if view.WorkspaceID != "ws-a" {
		t.Fatalf("expected workspace ws-a, got %s", view.WorkspaceID)
	}
	want := map[string]int64{"max_daemons": 5, "polling_interval_seconds": 30, "metrics_retention_days": 30}
	if len(view.Quotas) != len(want) {
		t.Fatalf("expected quotas %v, got %v", want, view.Quotas)
	}
	for k, v := range want {
		if view.Quotas[k] != v {
			t.Fatalf("expected quotas[%s] = %d, got %d", k, v, view.Quotas[k])
		}
	}

	if _, err := svc.GetWorkspaceQuota(ctx, "account-b", "ws-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-member, got %v", err)
	}
	if _, err := svc.GetWorkspaceQuota(ctx, "account-a", "  "); err == nil {
		t.Fatal("empty workspace id accepted")
	}
}

func TestCredentialLifecycleAndScopes(t *testing.T) {
	svc, db, _, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	// A host-linked credential for CI/CD: only the backup action on this
	// host's daemons.
	created, err := svc.CreateCredential(ctx, "account-a", "ci-backup", nil, []string{"host-42"}, []string{"backup"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || !strings.HasPrefix(created.Token, CredentialTokenPrefix) {
		t.Fatalf("credential token missing or unprefixed: %#v", created)
	}
	resolved, err := svc.CredentialByToken(ctx, created.Token)
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if resolved.ID != created.ID || resolved.Label != "ci-backup" {
		t.Fatalf("resolved credential mismatch: %+v", resolved)
	}
	if len(created.DaemonIDs) != 0 || len(created.HostIDs) != 1 || created.HostIDs[0] != "host-42" ||
		len(created.ActionNames) != 1 || created.ActionNames[0] != "backup" {
		t.Fatalf("credential scopes not stored: %#v", created)
	}

	// The daemon must carry the host id for the host scope to match.
	if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{SentAt: time.Now(), HostID: "host-42"}); err != nil {
		t.Fatal(err)
	}
	// Allowed: backup on a host-42 daemon.
	if _, err := svc.EnqueueWebhook(ctx, "account-a", daemon.ID, "backup", []byte("{}"), "", "ci-backup", resolved); err != nil {
		t.Fatalf("in-scope action rejected: %v", err)
	}
	// Blocked: different action.
	if _, err := svc.EnqueueWebhook(ctx, "account-a", daemon.ID, "cleanup", []byte("{}"), "", "ci-backup", resolved); !errors.Is(err, ErrForbidden) {
		t.Fatalf("out-of-scope action expected forbidden, got %v", err)
	}
	// Blocked: daemon on a different host.
	other, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueWebhook(ctx, "account-a", other.ID, "backup", []byte("{}"), "", "ci-backup", resolved); !errors.Is(err, ErrForbidden) {
		t.Fatalf("out-of-scope daemon expected forbidden, got %v", err)
	}

	// Listing hides the token; deletion revokes it.
	list, err := svc.ListCredentials(ctx, "account-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("credential list: %v %#v", err, list)
	}
	if err := svc.DeleteCredential(ctx, "account-a", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CredentialByToken(ctx, created.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked credential expected unauthorized, got %v", err)
	}
	// Other accounts cannot delete it.
	if err := svc.DeleteCredential(ctx, "account-b", created.ID); err == nil {
		t.Fatalf("cross-account credential delete accepted")
	}
}
