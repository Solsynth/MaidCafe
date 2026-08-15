package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

type fakePublisher struct{ events []NotificationEvent }

func (f *fakePublisher) Publish(_ context.Context, event NotificationEvent) error {
	f.events = append(f.events, event)
	return nil
}

// fakeWorkspaces grants membership to the accounts listed for each workspace.
type fakeWorkspaces struct {
	members map[string][]string
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
	notification, err := svc.CreateNotification(ctx, daemon.ID, daemon.Secret, NotificationInput{Kind: "webhook.failure", Title: "failed", Body: "details", Metadata: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 1 || publisher.events[0].NotificationID != notification.ID {
		t.Fatalf("event mismatch: %#v", publisher.events)
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
func TestMetricAlarmAndPushRequestPersistence(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAlarm(ctx, "account-a", daemon.ID, AlarmInput{Kind: "cpu_percent", Threshold: 80, Enabled: true, CooldownSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{SentAt: time.Now(), CPUPercent: 90}); err != nil {
		t.Fatal(err)
	}
	notifications, err := svc.ListNotifications(ctx, "account-a", "ws-a", false, daemon.ID, 50, nil)
	if err != nil || len(notifications) != 1 || notifications[0].Kind != "daemon.alarm.cpu_percent" {
		t.Fatalf("alarm notification: %v %#v", err, notifications)
	}
	requested, err := svc.CreatePushNotification(ctx, "account-a", daemon.ID, NotificationInput{Kind: "maintenance", Title: "Restart", Body: "Restart after backup"})
	if err != nil || requested.Title != "Restart" {
		t.Fatalf("push request: %v %#v", err, requested)
	}
	if len(publisher.events) != 2 ||
		publisher.events[0].Kind != "daemon.alarm.cpu_percent" ||
		publisher.events[1].Kind != "maintenance" {
		t.Fatalf("notification publication mismatch: %#v", publisher.events)
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
	enqueued, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "backup", body, signature)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued.Status != "pending" || enqueued.Name != "backup" || enqueued.Body != base64.StdEncoding.EncodeToString(body) {
		t.Fatalf("enqueued view = %#v", enqueued)
	}
	if _, err := svc.EnqueueWebhook(ctx, "account-b", created.ID, "backup", body, signature); err != ErrForbidden {
		t.Fatalf("account isolation failed: %v", err)
	}
	if _, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "", body, signature); err == nil {
		t.Fatalf("empty name accepted")
	}

	pending, err := svc.ListPendingWebhooks(ctx, created.ID, created.Secret, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != enqueued.ID || pending[0].Signature != signature {
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
	enqueued, err := svc.EnqueueWebhook(ctx, "account-a", created.ID, "backup", []byte("x"), "sig")
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
