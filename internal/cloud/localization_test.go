package cloud

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	gen "src.solsynth.dev/sosys/go/proto"
)

type languageAccountClient struct {
	language string
}

func (c languageAccountClient) GetAccount(context.Context, *gen.DyGetAccountRequest, ...grpc.CallOption) (*gen.DyAccount, error) {
	return &gen.DyAccount{Language: c.language}, nil
}

func TestLocalizeAlarmLanguages(t *testing.T) {
	for _, tc := range []struct {
		language string
		want     string
	}{
		{"en-US", "CPU threshold exceeded"},
		{"zh-CN", "CPU 已超过阈值"},
		{"zh-TW", "CPU 已超過閾值"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			got, _, ok := localizeAlarm(tc.language, "daemon.alarm.cpu_percent", map[string]any{"value": 92.5, "threshold": 80.0})
			if !ok || got != tc.want {
				t.Fatalf("localized title = %q, ok=%v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestServiceLocalizesAlarmWithAccountPreference(t *testing.T) {
	svc := &Service{accounts: languageAccountClient{language: "zh-CN"}}
	title, body := svc.localizedAlarm(context.Background(), "account-1", "daemon.alarm.disk_used_percent", "disk_used_percent threshold exceeded", "disk_used_percent reached 91.00% (threshold 80.00%)", []byte(`{"value":91,"threshold":80}`))
	if title != "磁盘已超过阈值" || body != "磁盘使用率达到 91.00%（阈值 80.00%）。" {
		t.Fatalf("localized alarm = %q / %q", title, body)
	}
}

func TestLocalizeDisconnectedAlarm(t *testing.T) {
	title, body, ok := localizeAlarm("zh-TW", "daemon.disconnected", map[string]any{
		"age": "5m0s", "last_seen": "2026-08-17T12:00:00Z",
	})
	if !ok || title != "守護程式已中斷連線" || body != "已有 5m0s 未收到指標；最後回報時間為 2026-08-17T12:00:00Z。" {
		t.Fatalf("localized disconnect = %q / %q / %v", title, body, ok)
	}
}

func TestCreateNotificationPersistsAndPublishesLocalizedAlarm(t *testing.T) {
	svc, db, publisher, _ := testService(t)
	defer db.Close()
	svc.accounts = languageAccountClient{language: "zh-TW"}
	created, err := svc.CreateDaemon(context.Background(), "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	if title, _, ok := localizeAlarm("zh-TW", "daemon.alarm.disk_used_percent", map[string]any{"value": 91.0, "threshold": 80.0}); !ok || title != "磁碟已超過閾值" {
		t.Fatalf("catalog lookup failed: %q %v", title, ok)
	}
	view, err := svc.CreateNotification(context.Background(), created.ID, created.Secret, NotificationInput{
		Kind:     "daemon.alarm.disk_used_percent",
		Title:    "disk_used_percent threshold exceeded",
		Body:     "disk_used_percent reached 91.00% (threshold 80.00%)",
		Metadata: []byte(`{"value":91,"threshold":80}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Title != "磁碟已超過閾值" || len(publisher.events) != 1 || publisher.events[0].Title != view.Title {
		t.Fatalf("localized persistence/fanout = %#v / %#v", view, publisher.events)
	}
}
