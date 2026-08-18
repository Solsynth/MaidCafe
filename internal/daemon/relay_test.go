package daemon

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	"src.solsynth.dev/solsynth/maidcafe/internal/server"
)

func newRelayCloud(t *testing.T) (*cloud.Service, string) {
	t.Helper()
	db, err := database.NewSQLite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	svc := cloud.NewService(db, nil, relayWorkspaces{})
	cloudServer := httptest.NewServer(server.NewRouter(nil, svc, nil))
	t.Cleanup(cloudServer.Close)
	return svc, cloudServer.URL
}

// relayWorkspaces grants account-a member access to ws-a, mirroring the
// production DyWorkspaceService contract.
type relayWorkspaces struct{}

func (relayWorkspaces) IsMemberWithRole(_ context.Context, workspaceID, accountID string, _ []int32) (bool, error) {
	return workspaceID == "ws-a" && accountID == "account-a", nil
}

func (relayWorkspaces) GetPlanQuota(_ context.Context, workspaceID string) (map[string]int64, error) {
	return map[string]int64{"max_daemons": 10}, nil
}

func relayDaemonConfig(id, cloudURL, cloudSecret, command string) config.DaemonConfig {
	return config.DaemonConfig{
		ID:                id,
		CloudURL:          cloudURL,
		CloudSecret:       cloudSecret,
		RequestTimeout:    5 * time.Second,
		ScriptTimeout:     5 * time.Second,
		MaxBodyBytes:      65536,
		MaxConcurrentRuns: 1,
		Webhooks: []config.WebhookConfig{
			{Name: "backup", Secret: "secret", Command: command, Enabled: true},
		},
	}
}

func TestWebhookRelayExecutesAndReportsThroughCloud(t *testing.T) {
	svc, cloudURL := newRelayCloud(t)
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"job":"incremental"}`)
	enqueued, err := svc.EnqueueWebhook(
		ctx, "account-a", daemon.ID, "backup", body, signedHeader("secret", body), "@alice", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "stdin")
	script := executable(t, "#!/bin/sh\ncat > "+output+"\nprintf 'relayed-ok'")
	executor := NewWebhookExecutor(relayDaemonConfig(daemon.ID, cloudURL, daemon.Secret, script))
	publisher, err := NewCloudPublisher(
		relayDaemonConfig(daemon.ID, cloudURL, daemon.Secret, script),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	publisherBox := &atomic.Pointer[CloudPublisher]{}
	publisherBox.Store(publisher)
	relay := NewWebhookRelay(publisherBox, executor, nil, nil)
	relay.pollOnce(ctx)

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("stdin mismatch: %q", got)
	}
	result, err := svc.GetWebhookResult(ctx, "account-a", daemon.ID, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" || result.ResultCode != 200 {
		t.Fatalf("result = %#v", result)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.ResultBody)
	if err != nil || string(decoded) != "relayed-ok" {
		t.Fatalf("result body = %q err = %v", result.ResultBody, err)
	}
}

func TestWebhookRelayRejectsBadSignatureWithoutExecuting(t *testing.T) {
	svc, cloudURL := newRelayCloud(t)
	ctx := context.Background()
	daemon, err := svc.CreateDaemon(ctx, "account-a", "ws-a", "host")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("payload")
	enqueued, err := svc.EnqueueWebhook(
		ctx, "account-a", daemon.ID, "backup", body, signedHeader("wrong-secret", body), "@alice", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "stdin")
	script := executable(t, "#!/bin/sh\ncat > "+output)
	cfg := relayDaemonConfig(daemon.ID, cloudURL, daemon.Secret, script)
	executor := NewWebhookExecutor(cfg)
	publisher, err := NewCloudPublisher(
		cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	publisherBox := &atomic.Pointer[CloudPublisher]{}
	publisherBox.Store(publisher)
	relay := NewWebhookRelay(publisherBox, executor, nil, nil)
	relay.pollOnce(ctx)

	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("script ran despite bad signature")
	}
	result, err := svc.GetWebhookResult(ctx, "account-a", daemon.ID, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" || result.ResultError == "" {
		t.Fatalf("result = %#v", result)
	}
}
