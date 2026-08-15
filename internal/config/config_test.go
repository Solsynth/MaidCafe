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
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsAndDaemonValidation(t *testing.T) {
	path := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
[[daemon.webhooks]]
name = "backup"
secret = "secret"
command = "/bin/cat"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Listen != "127.0.0.1:8747" ||
		cfg.Daemon.CloudURL != "https://mk.solsynth.dev" ||
		cfg.Daemon.CloudSecret != "" ||
		cfg.Daemon.MetricsInterval != time.Minute ||
		cfg.Daemon.MetricsHistoryPath != "/var/lib/maidcafe/metrics" ||
		cfg.Daemon.MetricsRetentionDays != 7 ||
		cfg.Daemon.MaxBodyBytes != 65536 ||
		cfg.Daemon.StreamInterval != time.Second ||
		cfg.Daemon.ContainersInterval != 5*time.Second ||
		cfg.Daemon.ProcessesInterval != 10*time.Second ||
		cfg.Daemon.SystemdInterval != 30*time.Second ||
		cfg.Daemon.ProcessesLimit != 50 {
		t.Fatalf("unexpected daemon defaults: %#v", cfg.Daemon)
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatal(err)
	}
}
func TestHTTPDaemonRequiresMetricsSecret(t *testing.T) {
	cfg := Config{Daemon: DaemonConfig{
		ID:                "host-1",
		Transport:         "http",
		Listen:            "127.0.0.1:8747",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
	}}
	if err := cfg.ValidateDaemon(); err == nil {
		t.Fatal("expected HTTP daemon without metrics secret to be rejected")
	}
	cfg.Daemon.MetricsSecret = "metrics-secret"
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatalf("HTTP daemon with metrics secret rejected: %v", err)
	}
}

func TestDaemonRejectsRetentionOverThirtyDays(t *testing.T) {
	cfg := Config{Daemon: DaemonConfig{
		ID:                   "host-1",
		Transport:            "stdio",
		MetricsRetentionDays: 31,
		MetricsInterval:      time.Minute,
		StreamInterval:       time.Second,
		ProcessesLimit:       50,
		RequestTimeout:       time.Second,
		ScriptTimeout:        time.Second,
		MaxBodyBytes:         1,
		MaxConcurrentRuns:    1,
	}}
	if err := cfg.ValidateDaemon(); err == nil {
		t.Fatal("expected retention limit validation error")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateDaemon(); err == nil {
		t.Fatal("expected invalid webhook")
	}
	cloud, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := cloud.ValidateCloud(); err == nil {
		t.Fatal("expected cloud requirements error")
	}
}

func TestDaemonCloudURLValidation(t *testing.T) {
	base := DaemonConfig{ID: "host-1", Listen: "127.0.0.1:8747", MetricsSecret: "metrics-secret", MetricsInterval: time.Minute, StreamInterval: time.Second, ProcessesLimit: 50, RequestTimeout: time.Second, ScriptTimeout: time.Second, MaxBodyBytes: 1, MaxConcurrentRuns: 1}
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{name: "canonical HTTPS", url: "https://mk.solsynth.dev", ok: true},
		{name: "localhost HTTP", url: "http://localhost:8080", ok: true},
		{name: "loopback HTTP", url: "http://127.0.0.1:8747", ok: true},
		{name: "empty optional URL", url: "", ok: true},
		{name: "plain HTTP host", url: "http://maidcafe.example.com", ok: false},
		{name: "hostless URL", url: "https:///missing-host", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Daemon: base}
			cfg.Daemon.CloudURL = tc.url
			if err := cfg.ValidateDaemon(); (err == nil) != tc.ok {
				t.Fatalf("ValidateDaemon(%q) error = %v, want success %v", tc.url, err, tc.ok)
			}
		})
	}
}
func TestDaemonStreamValidation(t *testing.T) {
	base := DaemonConfig{
		ID:                 "host-1",
		Transport:          "stdio",
		MetricsInterval:    time.Minute,
		StreamInterval:     time.Second,
		ContainersInterval: 5 * time.Second,
		ProcessesInterval:  10 * time.Second,
		SystemdInterval:    30 * time.Second,
		ProcessesLimit:     50,
		RequestTimeout:     time.Second,
		ScriptTimeout:      time.Second,
		MaxBodyBytes:       1,
		MaxConcurrentRuns:  1,
	}
	if err := (&Config{Daemon: base}).ValidateDaemon(); err != nil {
		t.Fatalf("valid stream config rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DaemonConfig)
	}{
		{name: "zero stream interval", mutate: func(d *DaemonConfig) { d.StreamInterval = 0 }},
		{name: "negative containers interval", mutate: func(d *DaemonConfig) { d.ContainersInterval = -time.Second }},
		{name: "negative processes interval", mutate: func(d *DaemonConfig) { d.ProcessesInterval = -time.Second }},
		{name: "negative systemd interval", mutate: func(d *DaemonConfig) { d.SystemdInterval = -time.Second }},
		{name: "zero processes limit", mutate: func(d *DaemonConfig) { d.ProcessesLimit = 0 }},
		{name: "oversized processes limit", mutate: func(d *DaemonConfig) { d.ProcessesLimit = 501 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Daemon: base}
			tc.mutate(&cfg.Daemon)
			if err := cfg.ValidateDaemon(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestDaemonRejectsPrivilegedListenPort(t *testing.T) {
	cfg := Config{Daemon: DaemonConfig{
		ID:                "host-1",
		Transport:         "http",
		Listen:            "127.0.0.1:80",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
	}}
	if err := cfg.ValidateDaemon(); err == nil {
		t.Fatal("expected privileged daemon port to be rejected")
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv("DAEMON_ID", "env-host")
	t.Setenv("DAEMON_REQUEST_TIMEOUT", "2s")
	t.Setenv("DAEMON_STREAM_INTERVAL", "2s")
	t.Setenv("DAEMON_CONTAINERS_INTERVAL", "7s")
	t.Setenv("DAEMON_PROCESSES_INTERVAL", "3s")
	t.Setenv("DAEMON_SYSTEMD_INTERVAL", "45s")
	t.Setenv("DAEMON_PROCESSES_LIMIT", "77")
	t.Setenv("RING_TARGET", "metoer:9090")
	cfg, err := Load(writeConfig(t, "[daemon]\nid = \"file-host\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.ID != "env-host" || cfg.Daemon.RequestTimeout != 2*time.Second || cfg.Ring.Target != "metoer:9090" {
		t.Fatalf("environment override failed: %#v", cfg)
	}
	if cfg.Daemon.StreamInterval != 2*time.Second ||
		cfg.Daemon.ContainersInterval != 7*time.Second ||
		cfg.Daemon.ProcessesInterval != 3*time.Second ||
		cfg.Daemon.SystemdInterval != 45*time.Second ||
		cfg.Daemon.ProcessesLimit != 77 {
		t.Fatalf("stream environment overrides failed: %#v", cfg.Daemon)
	}
}
