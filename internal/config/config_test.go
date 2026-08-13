package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil { t.Fatal(err) }
	return path
}

func TestLoadDefaultsAndDaemonValidation(t *testing.T) {
	path := writeConfig(t, `
[daemon]
id = "host-1"
[[daemon.webhooks]]
name = "backup"
secret = "secret"
command = "/bin/cat"
`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Daemon.Listen != "127.0.0.1:8747" || cfg.Daemon.MetricsInterval != time.Minute || cfg.Daemon.MaxBodyBytes != 65536 { t.Fatalf("defaults not loaded: %#v", cfg.Daemon) }
	if err := cfg.ValidateDaemon(); err != nil { t.Fatal(err) }
}

func TestConfigRejectsBadWebhooksAndCloudRequirements(t *testing.T) {
	path := writeConfig(t, `
[daemon]
id = "host-1"
[[daemon.webhooks]]
name = "same"
secret = "one"
command = "relative"
[[daemon.webhooks]]
name = "same"
secret = "two"
command = "/bin/cat"
`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if err := cfg.ValidateDaemon(); err == nil { t.Fatal("expected invalid webhook") }
	cloud, err := Load(writeConfig(t, ""))
	if err != nil { t.Fatal(err) }
	if err := cloud.ValidateCloud(); err == nil { t.Fatal("expected cloud requirements error") }
}

func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv("DAEMON_ID", "env-host")
	t.Setenv("DAEMON_REQUEST_TIMEOUT", "2s")
	cfg, err := Load(writeConfig(t, "[daemon]\nid = \"file-host\"\n"))
	if err != nil { t.Fatal(err) }
	if cfg.Daemon.ID != "env-host" || cfg.Daemon.RequestTimeout != 2*time.Second { t.Fatalf("environment override failed: %#v", cfg.Daemon) }
}
