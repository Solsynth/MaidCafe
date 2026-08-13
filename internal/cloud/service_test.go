package cloud

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

type fakePublisher struct{ events []NotificationEvent }

func (f *fakePublisher) Publish(_ context.Context, event NotificationEvent) error {
	f.events = append(f.events, event)
	return nil
}

func testService(t *testing.T) (*Service, *database.DB, *fakePublisher) {
	t.Helper()
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{}
	return NewService(db, publisher), db, publisher
}
func TestDaemonCredentialsOwnershipAndRotation(t *testing.T) {
	svc, db, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	created, err := svc.CreateDaemon(ctx, "account-a", "host")
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
	svc, db, publisher := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "host")
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
	rows, err := svc.ListNotifications(ctx, "account-a", true, "", 50, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("unread listing: %v %#v", err, rows)
	}
	if err := svc.MarkNotificationRead(ctx, "account-a", notification.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkNotificationRead(ctx, "account-a", notification.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.ListNotifications(ctx, "account-a", true, "", 50, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("read acknowledgement: %v %#v", err, rows)
	}
	if _, err := svc.ListNotifications(ctx, "account-b", false, "", 50, nil); err != nil {
		t.Fatal(err)
	}
}
func TestMetricHistoryIsOwnedAndOrdered(t *testing.T) {
	svc, db, _ := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "host")
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
	svc, db, publisher := testService(t)
	defer db.Close()
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAlarm(ctx, "account-a", daemon.ID, AlarmInput{Kind: "cpu_percent", Threshold: 80, Enabled: true, CooldownSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestMetric(ctx, daemon.ID, daemon.Secret, MetricInput{SentAt: time.Now(), CPUPercent: 90}); err != nil {
		t.Fatal(err)
	}
	notifications, err := svc.ListNotifications(ctx, "account-a", false, daemon.ID, 50, nil)
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
