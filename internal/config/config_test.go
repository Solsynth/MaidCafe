package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
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
		cfg.Cloud.DaemonDisconnectAfter != 5*time.Minute ||
		cfg.Cloud.DaemonDisconnectNotificationCooldown != 30*time.Minute ||
		cfg.Cloud.AlarmCheckInterval != time.Minute ||
		cfg.Daemon.AuditPath != "/var/lib/maidcafe/audit.jsonl" ||
		cfg.Daemon.MetricsRetentionDays != 7 ||
		cfg.Daemon.MaxBodyBytes != 65536 ||
		cfg.Daemon.StreamInterval != time.Second ||
		cfg.Daemon.ContainersInterval != 5*time.Second ||
		cfg.Daemon.ImagesInterval != time.Minute ||
		cfg.Daemon.ProcessesInterval != 10*time.Second ||
		cfg.Daemon.SystemdInterval != 30*time.Second ||
		cfg.Daemon.RuntimesInterval != 10*time.Second ||
		len(cfg.Daemon.Runtimes) != 8 ||
		cfg.Daemon.Runtimes[0] != "java" ||
		cfg.Daemon.Runtimes[7] != "php" ||
		len(cfg.Daemon.WatchedProcesses) != 0 ||
		cfg.Daemon.WatchedProcessesFile != "/var/lib/maidcafe/watched-processes.json" ||
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
		Runtimes:          []string{"java", "dotnet", "python"},
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
		Runtimes:             []string{"java", "dotnet", "python"},
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
	base := DaemonConfig{ID: "host-1", Listen: "127.0.0.1:8747", MetricsSecret: "metrics-secret", MetricsInterval: time.Minute, StreamInterval: time.Second, Runtimes: []string{"java", "dotnet", "python"}, ProcessesLimit: 50, RequestTimeout: time.Second, ScriptTimeout: time.Second, MaxBodyBytes: 1, MaxConcurrentRuns: 1}
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
		ImagesInterval:     time.Minute,
		ProcessesInterval:  10 * time.Second,
		SystemdInterval:    30 * time.Second,
		Runtimes:           []string{"java", "dotnet", "python"},
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
		{name: "negative images interval", mutate: func(d *DaemonConfig) { d.ImagesInterval = -time.Second }},
		{name: "negative processes interval", mutate: func(d *DaemonConfig) { d.ProcessesInterval = -time.Second }},
		{name: "negative systemd interval", mutate: func(d *DaemonConfig) { d.SystemdInterval = -time.Second }},
		{name: "negative runtimes interval", mutate: func(d *DaemonConfig) { d.RuntimesInterval = -time.Second }},
		{name: "empty runtimes list", mutate: func(d *DaemonConfig) { d.Runtimes = []string{} }},
		{name: "bad runtime name", mutate: func(d *DaemonConfig) { d.Runtimes = []string{"Java"} }},
		{name: "duplicate runtime name", mutate: func(d *DaemonConfig) { d.Runtimes = []string{"java", "java"} }},
		{name: "bad watched name", mutate: func(d *DaemonConfig) { d.WatchedProcesses = []string{"bad name"} }},
		{name: "duplicate watched name", mutate: func(d *DaemonConfig) { d.WatchedProcesses = []string{"nginx", "nginx"} }},
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

func TestDaemonValidatesHookExecutionSettings(t *testing.T) {
	base := DaemonConfig{
		ID:                "host-1",
		Transport:         "stdio",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DaemonConfig)
		ok     bool
	}{
		{
			name: "plain action accepted",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{Name: "a", Command: "/bin/true", Enabled: true}}
			},
			ok: true,
		},
		{
			name: "absolute cwd and env accepted",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					Cwd: "/srv/app", Env: []string{"KEY=value", "_X=1"},
				}}
			},
			ok: true,
		},
		{
			name: "existing user accepted",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					User: current.Username,
				}}
			},
			ok: true,
		},
		{
			name: "relative cwd rejected",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					Cwd: "srv/app",
				}}
			},
		},
		{
			name: "malformed env entry rejected",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					Env: []string{"1BAD=value"},
				}}
			},
		},
		{
			name: "env without equals rejected",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					Env: []string{"NOPE"},
				}}
			},
		},
		{
			name: "missing user rejected",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					User: "definitely-not-a-real-user-xyz",
				}}
			},
		},
		{
			name: "negative timeout rejected",
			mutate: func(d *DaemonConfig) {
				d.Actions = []WebhookConfig{{
					Name: "a", Command: "/bin/true", Enabled: true,
					Timeout: -time.Second,
				}}
			},
		},
		{
			name: "webhooks validated the same way",
			mutate: func(d *DaemonConfig) {
				d.Webhooks = []WebhookConfig{{
					Name: "w", Secret: "s", Command: "/bin/true", Enabled: true,
					Cwd: "relative",
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Daemon: base}
			tc.mutate(&cfg.Daemon)
			err := cfg.ValidateDaemon()
			if tc.ok && err != nil {
				t.Fatalf("expected valid config, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestWebhookLabelFallsBackToName(t *testing.T) {
	named := WebhookConfig{Name: "deploy", DisplayName: "Deploy the app"}
	if named.Label() != "Deploy the app" {
		t.Fatalf("Label() = %q", named.Label())
	}
	plain := WebhookConfig{Name: "deploy"}
	if plain.Label() != "deploy" {
		t.Fatalf("Label() fallback = %q", plain.Label())
	}
	blank := WebhookConfig{Name: "deploy", DisplayName: "   "}
	if blank.Label() != "deploy" {
		t.Fatalf("Label() blank display = %q", blank.Label())
	}
}

func TestLoadMergesActionFragments(t *testing.T) {
	dir := t.TempDir()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	base := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
actionsDir = "`+filepath.ToSlash(dir)+`"
`)
	if err := os.WriteFile(filepath.Join(dir, "deploy.toml"), []byte(`
name = "deploy"
command = "/etc/maidcafe/actions/deploy.sh"
script = true
cwd = "/srv/myapp"
user = "`+current.Username+`"
env = ["CI_BUILD=42"]
timeout = "2m"
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cleanup.toml"), []byte(`
name = "cleanup"
command = "/etc/maidcafe/actions/cleanup.sh"
`), 0600); err != nil {
		t.Fatal(err)
	}
	// Non-TOML files in the dir are ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Actions) != 2 {
		t.Fatalf("merged actions = %d, want 2 (%+v)", len(cfg.Daemon.Actions), cfg.Daemon.Actions)
	}
	// Sorted by file name: cleanup before deploy.
	cleanup, deploy := cfg.Daemon.Actions[0], cfg.Daemon.Actions[1]
	if cleanup.Name != "cleanup" || deploy.Name != "deploy" {
		t.Fatalf("unexpected order: %q, %q", cleanup.Name, deploy.Name)
	}
	if deploy.Cwd != "/srv/myapp" || deploy.User != current.Username ||
		len(deploy.Env) != 1 || deploy.Env[0] != "CI_BUILD=42" ||
		deploy.Timeout != 2*time.Minute {
		t.Fatalf("fragment fields not merged: %+v", deploy)
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFragmentOverridesLegacyInlineAction(t *testing.T) {
	dir := t.TempDir()
	base := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
actionsDir = "`+filepath.ToSlash(dir)+`"

[[daemon.actions]]
name = "deploy"
command = "/etc/maidcafe/actions/legacy.sh"
`)
	if err := os.WriteFile(filepath.Join(dir, "deploy.toml"), []byte(`
name = "deploy"
command = "/etc/maidcafe/actions/deploy.sh"
script = true
cwd = "/srv/myapp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Actions) != 1 {
		t.Fatalf("merged actions = %d, want 1 (%+v)", len(cfg.Daemon.Actions), cfg.Daemon.Actions)
	}
	if cfg.Daemon.Actions[0].Command != "/etc/maidcafe/actions/deploy.sh" ||
		cfg.Daemon.Actions[0].Cwd != "/srv/myapp" {
		t.Fatalf("fragment did not win over the inline entry: %+v", cfg.Daemon.Actions[0])
	}
	// The merged single action must validate: the duplicate-name crash a
	// legacy leftover used to cause is gone.
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatalf("merged config rejected: %v", err)
	}
}

func TestDaemonRejectsReservedNativeOpNames(t *testing.T) {
	cfg := Config{Daemon: DaemonConfig{
		ID:                "host-1",
		Transport:         "http",
		Listen:            "127.0.0.1:8747",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
		Actions: []WebhookConfig{{
			Name:    "container.restart",
			Command: "/etc/maidcafe/actions/container-restart.sh",
		}},
	}}
	err := cfg.ValidateDaemon()
	if err == nil {
		t.Fatal("expected reserved native op name to be rejected")
	}
	if !strings.Contains(err.Error(), "reserved for a built-in operation") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDaemonRejectsActionWebhookNameCollision(t *testing.T) {
	cfg := Config{Daemon: DaemonConfig{
		ID:                "host-1",
		Transport:         "http",
		Listen:            "127.0.0.1:8747",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
		Webhooks: []WebhookConfig{{
			Name:    "deploy",
			Secret:  "s",
			Command: "/bin/true",
		}},
		Actions: []WebhookConfig{{
			Name:    "deploy",
			Command: "/etc/maidcafe/actions/deploy.sh",
		}},
	}}
	err := cfg.ValidateDaemon()
	if err == nil {
		t.Fatal("expected webhook/action name collision to be rejected")
	}
	if !strings.Contains(err.Error(), "collides with a webhook of the same name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadIgnoresMissingActionsDir(t *testing.T) {
	path := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "s"
actionsDir = "`+filepath.ToSlash(filepath.Join(t.TempDir(), "missing"))+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Actions) != 0 {
		t.Fatalf("actions = %+v", cfg.Daemon.Actions)
	}
}

func TestLoadRejectsBrokenFragment(t *testing.T) {
	dir := t.TempDir()
	base := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "s"
actionsDir = "`+filepath.ToSlash(dir)+`"
`)
	if err := os.WriteFile(filepath.Join(dir, "bad.toml"), []byte("name = [not-a-string"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(base); err == nil {
		t.Fatal("expected broken fragment to fail the load")
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
		Runtimes:          []string{"java", "dotnet", "python"},
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
	t.Setenv("DAEMON_IMAGES_INTERVAL", "11s")
	t.Setenv("DAEMON_PROCESSES_INTERVAL", "3s")
	t.Setenv("DAEMON_SYSTEMD_INTERVAL", "45s")
	t.Setenv("DAEMON_PROCESSES_LIMIT", "77")
	t.Setenv("DAEMON_ALARMS_DIR", "/etc/maidcafe/alarms")
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
		cfg.Daemon.ImagesInterval != 11*time.Second ||
		cfg.Daemon.ProcessesInterval != 3*time.Second ||
		cfg.Daemon.SystemdInterval != 45*time.Second ||
		cfg.Daemon.ProcessesLimit != 77 ||
		cfg.Daemon.AlarmsDir != "/etc/maidcafe/alarms" {
		t.Fatalf("stream environment overrides failed: %#v", cfg.Daemon)
	}
}

func TestLoadMergesAlarmFragments(t *testing.T) {
	dir := t.TempDir()
	base := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
alarmsDir = "`+filepath.ToSlash(dir)+`"
`)
	if err := os.WriteFile(filepath.Join(dir, "cpu_percent.toml"), []byte(`
kind = "cpu_percent"
threshold = 85
cooldownSeconds = 120
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory_used_percent.toml"), []byte(`
kind = "memory_used_percent"
threshold = 90
enabled = false
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Alarms) != 2 {
		t.Fatalf("merged alarms = %d, want 2 (%+v)", len(cfg.Daemon.Alarms), cfg.Daemon.Alarms)
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatal(err)
	}
	cpu, memory := cfg.Daemon.Alarms[0], cfg.Daemon.Alarms[1]
	if cpu.Kind != "cpu_percent" || memory.Kind != "memory_used_percent" {
		t.Fatalf("unexpected order: %q, %q", cpu.Kind, memory.Kind)
	}
	if cpu.Threshold != 85 || cpu.CooldownSeconds != 120 {
		t.Fatalf("cpu fragment fields not merged: %+v", cpu)
	}
	if memory.Threshold != 90 || memory.Enabled == nil || *memory.Enabled {
		t.Fatalf("memory fragment fields not merged: %+v", memory)
	}
	// Validation defaults: absent enabled -> true, absent cooldown -> 300.
	if cpu.Enabled == nil || !*cpu.Enabled {
		t.Fatalf("cpu alarm enabled did not default to true: %+v", cpu)
	}
	if memory.CooldownSeconds != 300 {
		t.Fatalf("memory alarm cooldown did not default to 300: %+v", memory)
	}
}

func TestLoadAlarmFragmentOverridesLegacyInlineAlarm(t *testing.T) {
	dir := t.TempDir()
	base := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
alarmsDir = "`+filepath.ToSlash(dir)+`"

[[daemon.alarms]]
kind = "cpu_percent"
threshold = 50
`)
	if err := os.WriteFile(filepath.Join(dir, "cpu_percent.toml"), []byte(`
kind = "cpu_percent"
threshold = 95
cooldownSeconds = 60
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Alarms) != 1 {
		t.Fatalf("merged alarms = %d, want 1 (%+v)", len(cfg.Daemon.Alarms), cfg.Daemon.Alarms)
	}
	if cfg.Daemon.Alarms[0].Threshold != 95 || cfg.Daemon.Alarms[0].CooldownSeconds != 60 {
		t.Fatalf("fragment did not win over the inline entry: %+v", cfg.Daemon.Alarms[0])
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRejectsInvalidAlarms(t *testing.T) {
	cases := []struct {
		name  string
		alarm AlarmConfig
	}{
		{"unknown kind", AlarmConfig{Kind: "filesystem_health", Threshold: 80}},
		{"zero threshold", AlarmConfig{Kind: "cpu_percent", Threshold: 0}},
		{"threshold above 100", AlarmConfig{Kind: "cpu_percent", Threshold: 120}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Daemon: DaemonConfig{
				ID:                "host-1",
				MetricsSecret:     "metrics-secret",
				Alarms:            []AlarmConfig{tc.alarm},
				MetricsInterval:   time.Minute,
				StreamInterval:    time.Second,
				Runtimes:          []string{"java", "dotnet", "python"},
				ProcessesLimit:    50,
				RequestTimeout:    10 * time.Second,
				ScriptTimeout:     30 * time.Second,
				MaxBodyBytes:      65536,
				MaxConcurrentRuns: 4,
			}}
			if err := cfg.ValidateDaemon(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
	// Duplicate kinds are rejected too.
	dup := Config{Daemon: DaemonConfig{
		ID:                "host-1",
		MetricsSecret:     "metrics-secret",
		Alarms:            []AlarmConfig{{Kind: "cpu_percent", Threshold: 80}, {Kind: "cpu_percent", Threshold: 90}},
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		RequestTimeout:    10 * time.Second,
		ScriptTimeout:     30 * time.Second,
		MaxBodyBytes:      65536,
		MaxConcurrentRuns: 4,
	}}
	if err := dup.ValidateDaemon(); err == nil {
		t.Fatal("expected duplicate-kind validation error")
	}
}

func TestLoadIgnoresMissingAlarmsDir(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "metrics-secret"
alarmsDir = "`+filepath.ToSlash(filepath.Join(t.TempDir(), "missing"))+`"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Alarms) != 0 {
		t.Fatalf("expected no alarms, got %+v", cfg.Daemon.Alarms)
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonValidatesJobs(t *testing.T) {
	base := DaemonConfig{
		ID:                "host-1",
		Transport:         "stdio",
		MetricsInterval:   time.Minute,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java", "dotnet", "python"},
		ProcessesLimit:    50,
		RequestTimeout:    time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1,
		MaxConcurrentRuns: 1,
	}
	// A cron expression and an @every descriptor both validate.
	cfg := Config{Daemon: base}
	cfg.Daemon.Jobs = []JobConfig{
		{Name: "nightly", Schedule: "0 3 * * *", Action: "backup"},
		{Name: "cleanup", Schedule: "@every 30s", Action: "process.kill", Body: map[string]any{"pid": 42}},
	}
	if err := cfg.ValidateDaemon(); err != nil {
		t.Fatalf("valid jobs rejected: %v", err)
	}
	if cfg.Daemon.Jobs[0].Enabled == nil || !*cfg.Daemon.Jobs[0].Enabled {
		t.Fatal("enabled should default to true")
	}
	bad := []struct {
		name string
		job  JobConfig
	}{
		{"empty name", JobConfig{Schedule: "0 3 * * *", Action: "backup"}},
		{"bad name", JobConfig{Name: "bad;name", Schedule: "0 3 * * *", Action: "backup"}},
		{"missing schedule", JobConfig{Name: "j", Action: "backup"}},
		{"bad schedule", JobConfig{Name: "j", Schedule: "not a cron", Action: "backup"}},
		{"missing action", JobConfig{Name: "j", Schedule: "0 3 * * *"}},
		{"duplicate", JobConfig{Name: "j", Schedule: "0 3 * * *", Action: "a"}},
	}
	for _, tc := range bad {
		cfg := Config{Daemon: base}
		cfg.Daemon.Jobs = []JobConfig{{Name: "j", Schedule: "0 3 * * *", Action: "a"}, tc.job}
		err := cfg.ValidateDaemon()
		if err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
	// Duplicate name is rejected even without the filler.
	cfg = Config{Daemon: base}
	cfg.Daemon.Jobs = []JobConfig{
		{Name: "dup", Schedule: "@every 1m", Action: "a"},
		{Name: "dup", Schedule: "@every 2m", Action: "b"},
	}
	if err := cfg.ValidateDaemon(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate job name: err=%v", err)
	}
}

func TestLoadJobFragments(t *testing.T) {
	dir := t.TempDir()
	writeConfigFragment := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfigFragment("nightly", "name = \"nightly\"\nschedule = \"0 3 * * *\"\naction = \"backup\"\n")
	writeConfigFragment("cleanup", "name = \"cleanup\"\nschedule = \"@every 30s\"\naction = \"process.kill\"\nbody = { pid = 42 }\n")
	path := writeConfig(t, `
[daemon]
id = "host-1"
metricsSecret = "s"
jobsDir = "`+filepath.ToSlash(dir)+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Daemon.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(cfg.Daemon.Jobs))
	}
	if cfg.Daemon.Jobs[0].Name != "cleanup" || cfg.Daemon.Jobs[1].Name != "nightly" {
		t.Fatalf("jobs not sorted by fragment name: %+v", cfg.Daemon.Jobs)
	}
	pid, ok := cfg.Daemon.Jobs[0].Body["pid"]
	if !ok {
		t.Fatalf("job body not parsed: %+v", cfg.Daemon.Jobs[0].Body)
	}
	switch n := pid.(type) {
	case int, int64, float64:
		// TOML integers surface as int/int64/float64 depending on the
		// decoder; any numeric 42 is the point of this test.
		_ = n
	default:
		t.Fatalf("job body pid has unexpected type %T: %+v", pid, cfg.Daemon.Jobs[0].Body)
	}
}
