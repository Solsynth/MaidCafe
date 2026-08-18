package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
)

type Config struct {
	App       AppConfig       `mapstructure:"app"`
	HTTP      HTTPConfig      `mapstructure:"http"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Workspace WorkspaceConfig `mapstructure:"workspace"`
	Eventbus  EventbusConfig  `mapstructure:"eventbus"`
	Ring      RingConfig      `mapstructure:"ring"`
	Cloud     CloudConfig     `mapstructure:"cloud"`
	Daemon    DaemonConfig    `mapstructure:"daemon"`
}

type CloudConfig struct {
	// DaemonDisconnectAfter is the quiet period after which a daemon with a
	// previously accepted metric is considered disconnected.
	DaemonDisconnectAfter time.Duration `mapstructure:"daemonDisconnectAfter"`
	// DaemonDisconnectNotificationCooldown suppresses repeat disconnect pushes
	// for a daemon that recovers and becomes stale again within this period.
	DaemonDisconnectNotificationCooldown time.Duration `mapstructure:"daemonDisconnectNotificationCooldown"`
	// AlarmCheckInterval controls how often the cloud evaluates daemon state.
	AlarmCheckInterval time.Duration `mapstructure:"alarmCheckInterval"`
}

type AppConfig struct {
	Name string `mapstructure:"name"`
}
type HTTPConfig struct {
	Port string `mapstructure:"port"`
}
type DatabaseConfig struct {
	DSN string `mapstructure:"dsn"`
}
type AuthConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}
type WorkspaceConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}
type RingConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}
type EventbusConfig struct {
	URL           string `mapstructure:"url"`
	SubjectPrefix string `mapstructure:"subjectPrefix"`
}

// hostIDPath persists the stable machine identity the install flow writes
// once. It survives binary updates and config rewrites (no sync script
// touches it), so the cloud can link the host across daemon reinstalls.
const hostIDPath = "/etc/maidcafe/host-id"

type DaemonConfig struct {
	ID                   string `mapstructure:"id"`
	HostID               string `mapstructure:"hostId"`
	Version              string `mapstructure:"version"`
	Transport            string `mapstructure:"transport"`
	Listen               string `mapstructure:"listen"`
	MetricsSecret        string `mapstructure:"metricsSecret"`
	MetricsHistoryPath   string `mapstructure:"metricsHistoryPath"`
	MetricsRetentionDays int    `mapstructure:"metricsRetentionDays"`
	AuditPath            string `mapstructure:"auditPath"`
	// ActionsDir holds one `<slug>.toml` fragment per configured action
	// (plus the deployed script bodies and the run runtime directory). The
	// fragments are merged into Actions at load, so action changes never
	// touch the main config file.
	ActionsDir string `mapstructure:"actionsDir"`
	// AlarmsDir holds one `<kind>.toml` fragment per configured alarm. The
	// fragments are merged into Alarms at load, so alarm changes never touch
	// the main config file either.
	AlarmsDir string `mapstructure:"alarmsDir"`
	// JobsDir holds one `<slug>.toml` fragment per scheduled job. The
	// fragments are merged into Jobs at load, mirroring the action and alarm
	// fragment behavior.
	JobsDir string `mapstructure:"jobsDir"`
	// LogsDir is where the daemon persists captured container logs (one JSONL
	// file per container, rotated at 1 MiB with one generation, pruned by
	// metricsRetentionDays). Empty disables disk persistence but keeps the
	// in-memory tail.
	LogsDir                 string        `mapstructure:"logsDir"`
	CloudURL                string        `mapstructure:"cloudUrl"`
	CloudSecret             string        `mapstructure:"cloudSecret"`
	MetricsInterval         time.Duration `mapstructure:"metricsInterval"`
	StreamInterval          time.Duration `mapstructure:"streamInterval"`
	ContainersInterval      time.Duration `mapstructure:"containersInterval"`
	ImagesInterval          time.Duration `mapstructure:"imagesInterval"`
	ProcessesInterval       time.Duration `mapstructure:"processesInterval"`
	SystemdInterval         time.Duration `mapstructure:"systemdInterval"`
	RuntimesInterval        time.Duration `mapstructure:"runtimesInterval"`
	DatabaseMetricsInterval time.Duration `mapstructure:"databaseMetricsInterval"`
	// LogsInterval is the container log capture cadence. The collector tails
	// every running container with `logs --since <cursor>` and appends the
	// new lines to the disk store and the SSE stream. 0 disables log
	// tracking.
	LogsInterval time.Duration `mapstructure:"logsInterval"`
	// Runtimes is the ordered list of runtime groups the runtimes collector
	// reports, in wire order. Unknown entries are skipped by old clients.
	Runtimes []string `mapstructure:"runtimes"`
	// WatchedProcesses seeds the daemon-side watched-process list; dynamic
	// additions and removals via the API are persisted to
	// WatchedProcessesFile (authoritative once it exists).
	WatchedProcesses     []string        `mapstructure:"watchedProcesses"`
	WatchedProcessesFile string          `mapstructure:"watchedProcessesFile"`
	ProcessesLimit       int             `mapstructure:"processesLimit"`
	RequestTimeout       time.Duration   `mapstructure:"requestTimeout"`
	ScriptTimeout        time.Duration   `mapstructure:"scriptTimeout"`
	MaxBodyBytes         int64           `mapstructure:"maxBodyBytes"`
	MaxConcurrentRuns    int             `mapstructure:"maxConcurrentRuns"`
	Webhooks             []WebhookConfig `mapstructure:"webhooks"`
	Actions              []WebhookConfig `mapstructure:"actions"`
	Alarms               []AlarmConfig   `mapstructure:"alarms"`
	Jobs                 []JobConfig     `mapstructure:"jobs"`
}

// AlarmConfig declares one metric threshold the daemon evaluates against its
// own samples. Evaluation is intentionally daemon-side: the daemon reports a
// `daemon.alarm.<kind>` notification to the cloud when the threshold is
// exceeded, so the cloud only stores and forwards the resulting notification
// and never needs to reach back into the daemon.
// AlarmConfig declares one daemon metric or container threshold. Evaluation is
// intentionally daemon-side: the daemon reports a `daemon.alarm.<kind>`
// notification to the cloud when the condition is met.
type AlarmConfig struct {
	Kind string `mapstructure:"kind"`
	// Threshold is the percentage (0..100] for percentage alarms.
	Threshold float64 `mapstructure:"threshold"`
	// Target identifies the container name or ID for container_down alarms.
	// An empty target reports any down container.
	Target string `mapstructure:"target"`
	// Enabled defaults to true when absent.
	Enabled *bool `mapstructure:"enabled"`
	// CooldownSeconds is the minimum gap between two triggers of the same
	// alarm; it defaults to 300 when absent.
	CooldownSeconds int `mapstructure:"cooldownSeconds"`
}
type WebhookConfig struct {
	Name            string   `mapstructure:"name"`
	Secret          string   `mapstructure:"secret"`
	Command         string   `mapstructure:"command"`
	Args            []string `mapstructure:"args"`
	Enabled         bool     `mapstructure:"enabled"`
	NotifyOnSuccess bool     `mapstructure:"notifyOnSuccess"`
	NotifyOnFailure bool     `mapstructure:"notifyOnFailure"`
	// DisplayName is an optional human-readable label. [Name] is the slug
	// used for the API route, the deployed script file and audit records;
	// DisplayName is what notifications and clients show when present.
	DisplayName string `mapstructure:"displayName"`
	// Script marks command as a MaidKit-deployed script body. The executor
	// substitutes {{ name }} template variables from the request body into
	// the script before running it. Plain commands keep Script false.
	Script bool `mapstructure:"script"`
	// Cwd is the absolute working directory the command runs in; empty keeps
	// the daemon's own working directory.
	Cwd string `mapstructure:"cwd"`
	// User runs the command as another account through sudo (the daemon
	// itself is unprivileged). The sudoers rule granting this is deployed by
	// MaidKit; hand-configured entries must provide their own rule. Empty
	// runs the command as the daemon user.
	User string `mapstructure:"user"`
	// Env entries are KEY=VALUE assignments added to the command's
	// environment. Without a run-as user they are appended to the daemon's
	// environment; with one they are passed to sudo as command-line
	// assignments, which sudo applies on top of its reset environment.
	Env []string `mapstructure:"env"`
	// Timeout overrides the daemon-wide scriptTimeout for this hook (e.g.
	// "2m"); zero or absent uses the daemon-wide value.
	Timeout time.Duration `mapstructure:"timeout"`
}

// JobConfig declares one scheduled job: a recurring execution of a configured
// action or native operation. Jobs live as `<slug>.toml` fragments under
// daemon.jobsDir (or inline [[daemon.jobs]] entries) and run through the same
// executor as everything else, so runs land in the audit log, respect the
// concurrency limit and per-run timeout, and can notify on failure.
type JobConfig struct {
	// Name is the API slug (unique across jobs; jobs may share targets).
	Name string `mapstructure:"name"`
	// Schedule is a cron expression ("*/10 * * * *", five fields) or a
	// robfig/cron descriptor such as "@every 30s", "@hourly" or "@daily".
	Schedule string `mapstructure:"schedule"`
	// Action is the target: a configured action name or a native operation
	// slug (container.restart, process.kill, ...).
	Action string `mapstructure:"action"`
	// Body is the JSON object passed to the action on every run (template
	// variables for script actions, identity parameters for native ops).
	Body map[string]any `mapstructure:"body"`
	// Enabled defaults to true when absent.
	Enabled *bool `mapstructure:"enabled"`
	// Timeout overrides the daemon-wide scriptTimeout for this job; zero uses
	// the daemon-wide value (native compose ops keep their own 5m bound).
	Timeout time.Duration `mapstructure:"timeout"`
	// NotifyOnFailure publishes a job.failure notification when a run exits
	// non-zero or fails to start.
	NotifyOnFailure bool `mapstructure:"notifyOnFailure"`
}

var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// runtimeNamePattern constrains daemon.runtimes entries: lowercase start so
// they align with the client's runtime enum wire names.
var runtimeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// watchedProcessNamePattern constrains watched-process names (ps comm values).
var watchedProcessNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidWatchedProcessName reports whether name is a safe watched-process name
// (ps comm value). Shared by config validation and the daemon API.
func ValidWatchedProcessName(name string) bool {
	return watchedProcessNamePattern.MatchString(name)
}

// IsNativeOpName reports whether [name] collides with a built-in native
// operation slug. Shared by config validation (reserved names are rejected)
// and the daemon's relay dispatch.
func IsNativeOpName(name string) bool {
	for _, slug := range NativeOpNames {
		if slug == name {
			return true
		}
	}
	return false
}

// Label returns the human-readable name for display in notifications and
// clients, falling back to the API slug when no display name is configured.
func (h WebhookConfig) Label() string {
	if name := strings.TrimSpace(h.DisplayName); name != "" {
		return name
	}
	return h.Name
}

func validateHookExecution(hook WebhookConfig, kind string, index int) error {
	if hook.Cwd != "" && !filepath.IsAbs(hook.Cwd) {
		return fmt.Errorf("daemon.%s[%d].cwd must be an absolute path", kind, index)
	}
	if hook.User != "" {
		if _, err := user.Lookup(hook.User); err != nil {
			return fmt.Errorf("daemon.%s[%d].user %q does not exist: %w", kind, index, hook.User, err)
		}
	}
	for i, kv := range hook.Env {
		if !envAssignmentPattern.MatchString(kv) {
			return fmt.Errorf("daemon.%s[%d].env[%d] must be KEY=VALUE with an identifier key", kind, index, i)
		}
	}
	if hook.Timeout < 0 {
		return fmt.Errorf("daemon.%s[%d].timeout must not be negative", kind, index)
	}
	return nil
}

var webhookNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// NativeOpNames lists the built-in operations the daemon executes natively
// (container lifecycle, process kill, systemd unit actions, compose project
// actions). These slugs are reserved: webhooks and actions may not reuse
// them, so the cloud relay — which dispatches by name — stays unambiguous
// and credential action-name scopes mean the same thing for both kinds.
var NativeOpNames = []string{
	"container.start", "container.stop", "container.restart",
	"container.pause", "container.unpause", "container.kill", "container.remove",
	"process.kill",
	"systemd.start", "systemd.stop", "systemd.restart", "systemd.reload",
	"systemd.enable", "systemd.disable",
	"compose.up", "compose.stop", "compose.restart", "compose.pull", "compose.recreate",
}

func Load(configPath string) (*Config, error) {
	viper.Reset()
	viper.SetConfigType("toml")
	if configPath == "" {
		configPath = os.Getenv("CONFIG_PATH")
	}
	if configPath != "" {
		viper.SetConfigFile(configPath)
	}
	viper.SetDefault("app.name", "MaidCafe")
	viper.SetDefault("http.port", "8080")
	viper.SetDefault("database.dsn", "")
	viper.SetDefault("auth.target", "")
	viper.SetDefault("auth.useTLS", true)
	viper.SetDefault("auth.tlsSkipVerify", false)
	viper.SetDefault("workspace.target", "")
	viper.SetDefault("workspace.useTLS", true)
	viper.SetDefault("workspace.tlsSkipVerify", false)
	viper.SetDefault("eventbus.url", "")
	viper.SetDefault("eventbus.subjectPrefix", "")
	viper.SetDefault("ring.target", "")
	viper.SetDefault("ring.useTLS", false)
	viper.SetDefault("daemon.transport", "http")
	viper.SetDefault("daemon.listen", "127.0.0.1:8747")
	viper.SetDefault("daemon.metricsSecret", "")
	viper.SetDefault("daemon.metricsHistoryPath", "/var/lib/maidcafe/metrics")
	viper.SetDefault("daemon.metricsRetentionDays", 7)
	viper.SetDefault("daemon.auditPath", "/var/lib/maidcafe/audit.jsonl")
	viper.SetDefault("daemon.actionsDir", "/etc/maidcafe/actions")
	viper.SetDefault("daemon.alarmsDir", "/etc/maidcafe/alarms")
	viper.SetDefault("daemon.jobsDir", "/etc/maidcafe/jobs")
	viper.SetDefault("daemon.logsDir", "/var/lib/maidcafe/logs")
	viper.SetDefault("daemon.logsInterval", 10*time.Second)
	viper.SetDefault("daemon.cloudUrl", "https://mk.solsynth.dev")
	viper.SetDefault("daemon.cloudSecret", "")
	viper.SetDefault("daemon.metricsInterval", time.Minute)
	viper.SetDefault("daemon.streamInterval", time.Second)
	viper.SetDefault("daemon.containersInterval", 5*time.Second)
	viper.SetDefault("daemon.imagesInterval", time.Minute)
	viper.SetDefault("daemon.processesInterval", 10*time.Second)
	viper.SetDefault("daemon.systemdInterval", 30*time.Second)
	viper.SetDefault("daemon.runtimesInterval", 10*time.Second)
	viper.SetDefault("cloud.daemonDisconnectAfter", 5*time.Minute)
	viper.SetDefault("cloud.daemonDisconnectNotificationCooldown", 30*time.Minute)
	viper.SetDefault("cloud.alarmCheckInterval", time.Minute)
	viper.SetDefault("daemon.databaseMetricsInterval", 10*time.Second)
	viper.SetDefault("daemon.runtimes", []string{"java", "dotnet", "python", "node", "deno", "go", "ruby", "php"})
	viper.SetDefault("daemon.watchedProcesses", []string{})
	viper.SetDefault("daemon.watchedProcessesFile", "/var/lib/maidcafe/watched-processes.json")
	viper.SetDefault("daemon.processesLimit", 50)
	viper.SetDefault("daemon.requestTimeout", 10*time.Second)
	viper.SetDefault("daemon.scriptTimeout", 30*time.Second)
	viper.SetDefault("daemon.maxBodyBytes", int64(65536))
	viper.SetDefault("daemon.maxConcurrentRuns", 4)
	applyEnvAliases()
	if configPath != "" {
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}
	applyEnvAliases()
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := cfg.loadActionFragments(); err != nil {
		return nil, err
	}
	if err := cfg.loadAlarmFragments(); err != nil {
		return nil, err
	}
	if err := cfg.loadJobFragments(); err != nil {
		return nil, err
	}
	// The stable host identity is written by the install flow, not derived
	// from the config file; a missing file (hand-rolled installs) simply
	// leaves the daemon unlinked in the cloud.
	if raw, err := os.ReadFile(hostIDPath); err == nil {
		cfg.Daemon.HostID = strings.TrimSpace(string(raw))
	}
	return &cfg, nil
}

// loadActionFragments merges every `<slug>.toml` under daemon.actionsDir into
// Daemon.Actions, sorted by file name for deterministic ordering. A missing
// or unreadable directory is not an error (a fresh host has no actions yet);
// a fragment that fails to parse or validate is. Each fragment is a flat
// TOML file holding one action's fields — the same shape as an entry of
// [[daemon.actions]] — so action changes are isolated from the main config
// file and never rewrite it.
func (c *Config) loadActionFragments() error {
	dir := strings.TrimSpace(c.Daemon.ActionsDir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read actions dir %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}
	fragments := make([]WebhookConfig, 0, len(paths))
	names := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		hook, err := loadActionFragment(path)
		if err != nil {
			return err
		}
		names[hook.Name] = struct{}{}
		fragments = append(fragments, hook)
	}
	// MaidKit migrates actions out of the main config into per-file
	// fragments: a leftover inline [[daemon.actions]] entry and a fragment
	// with the same name describe the same action, so the fragment wins
	// (mirroring MaidKit's own read-back) instead of failing duplicate
	// validation and taking the daemon down.
	inline := c.Daemon.Actions
	c.Daemon.Actions = make([]WebhookConfig, 0, len(inline)+len(fragments))
	for _, hook := range inline {
		if _, covered := names[hook.Name]; covered {
			continue
		}
		c.Daemon.Actions = append(c.Daemon.Actions, hook)
	}
	c.Daemon.Actions = append(c.Daemon.Actions, fragments...)
	return nil
}

func loadActionFragment(path string) (WebhookConfig, error) {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return WebhookConfig{}, fmt.Errorf("read action fragment %s: %w", path, err)
	}
	var hook WebhookConfig
	if err := v.Unmarshal(&hook); err != nil {
		return WebhookConfig{}, fmt.Errorf("parse action fragment %s: %w", path, err)
	}
	if strings.TrimSpace(hook.Name) == "" {
		return WebhookConfig{}, fmt.Errorf("action fragment %s has no name", path)
	}
	return hook, nil
}

// loadAlarmFragments merges every `<kind>.toml` under daemon.alarmsDir into
// Daemon.Alarms, sorted by file name for deterministic ordering. A missing
// or unreadable directory is not an error (a fresh host has no alarms yet);
// a fragment that fails to parse or validate is. Each fragment is a flat
// TOML file holding one alarm's fields — the same shape as an entry of
// [[daemon.alarms]] — so alarm changes are isolated from the main config
// file and never rewrite it. A fragment covering an inline alarm wins,
// mirroring the action-fragment behavior.
func (c *Config) loadAlarmFragments() error {
	dir := strings.TrimSpace(c.Daemon.AlarmsDir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read alarms dir %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}
	fragments := make([]AlarmConfig, 0, len(paths))
	kinds := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		alarm, err := loadAlarmFragment(path)
		if err != nil {
			return err
		}
		kinds[alarm.Kind] = struct{}{}
		fragments = append(fragments, alarm)
	}
	inline := c.Daemon.Alarms
	c.Daemon.Alarms = make([]AlarmConfig, 0, len(inline)+len(fragments))
	for _, alarm := range inline {
		if _, covered := kinds[alarm.Kind]; covered {
			continue
		}
		c.Daemon.Alarms = append(c.Daemon.Alarms, alarm)
	}
	c.Daemon.Alarms = append(c.Daemon.Alarms, fragments...)
	return nil
}

func loadAlarmFragment(path string) (AlarmConfig, error) {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return AlarmConfig{}, fmt.Errorf("read alarm fragment %s: %w", path, err)
	}
	var alarm AlarmConfig
	if err := v.Unmarshal(&alarm); err != nil {
		return AlarmConfig{}, fmt.Errorf("parse alarm fragment %s: %w", path, err)
	}
	if strings.TrimSpace(alarm.Kind) == "" {
		return AlarmConfig{}, fmt.Errorf("alarm fragment %s has no kind", path)
	}
	return alarm, nil
}

// loadJobFragments merges every `<slug>.toml` under daemon.jobsDir into
// Daemon.Jobs, sorted by file name for deterministic ordering. A missing or
// unreadable directory is not an error (a fresh host has no jobs yet); a
// fragment that fails to parse or validate is. Each fragment is a flat TOML
// file holding one job's fields — the same shape as an entry of
// [[daemon.jobs]] — and a fragment covering an inline job wins, mirroring the
// action and alarm fragment behavior.
func (c *Config) loadJobFragments() error {
	dir := strings.TrimSpace(c.Daemon.JobsDir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read jobs dir %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}
	fragments := make([]JobConfig, 0, len(paths))
	names := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		job, err := loadJobFragment(path)
		if err != nil {
			return err
		}
		names[job.Name] = struct{}{}
		fragments = append(fragments, job)
	}
	inline := c.Daemon.Jobs
	c.Daemon.Jobs = make([]JobConfig, 0, len(inline)+len(fragments))
	for _, job := range inline {
		if _, covered := names[job.Name]; covered {
			continue
		}
		c.Daemon.Jobs = append(c.Daemon.Jobs, job)
	}
	c.Daemon.Jobs = append(c.Daemon.Jobs, fragments...)
	return nil
}

func loadJobFragment(path string) (JobConfig, error) {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return JobConfig{}, fmt.Errorf("read job fragment %s: %w", path, err)
	}
	var job JobConfig
	if err := v.Unmarshal(&job); err != nil {
		return JobConfig{}, fmt.Errorf("parse job fragment %s: %w", path, err)
	}
	if strings.TrimSpace(job.Name) == "" {
		return JobConfig{}, fmt.Errorf("job fragment %s has no name", path)
	}
	return job, nil
}

func applyEnvAliases() {
	aliases := map[string]string{
		"AUTH_TARGET": "auth.target", "AUTH_USE_TLS": "auth.useTLS", "AUTH_TLS_SKIP_VERIFY": "auth.tlsSkipVerify",
		"WORKSPACE_TARGET": "workspace.target", "WORKSPACE_USE_TLS": "workspace.useTLS", "WORKSPACE_TLS_SKIP_VERIFY": "workspace.tlsSkipVerify",
		"RING_TARGET": "ring.target", "RING_USE_TLS": "ring.useTLS", "RING_TLS_SKIP_VERIFY": "ring.tlsSkipVerify",
		"EVENTBUS_URL": "eventbus.url", "EVENTBUS_SUBJECT_PREFIX": "eventbus.subjectPrefix",
		"CLOUD_DAEMON_DISCONNECT_AFTER": "cloud.daemonDisconnectAfter", "CLOUD_DAEMON_DISCONNECT_NOTIFICATION_COOLDOWN": "cloud.daemonDisconnectNotificationCooldown", "CLOUD_ALARM_CHECK_INTERVAL": "cloud.alarmCheckInterval",
		"DAEMON_ID": "daemon.id", "DAEMON_TRANSPORT": "daemon.transport", "DAEMON_LISTEN": "daemon.listen",
		"DAEMON_METRICS_SECRET":         "daemon.metricsSecret",
		"DAEMON_METRICS_HISTORY_PATH":   "daemon.metricsHistoryPath",
		"DAEMON_METRICS_RETENTION_DAYS": "daemon.metricsRetentionDays",
		"DAEMON_AUDIT_PATH":             "daemon.auditPath",
		"DAEMON_ACTIONS_DIR":            "daemon.actionsDir",
		"DAEMON_ALARMS_DIR":             "daemon.alarmsDir",
		"DAEMON_JOBS_DIR":               "daemon.jobsDir",
		"DAEMON_LOGS_DIR":               "daemon.logsDir",
		"DAEMON_LOGS_INTERVAL":          "daemon.logsInterval",
		"DAEMON_CLOUD_URL":              "daemon.cloudUrl", "DAEMON_CLOUD_SECRET": "daemon.cloudSecret",
		"DAEMON_METRICS_INTERVAL": "daemon.metricsInterval", "DAEMON_STREAM_INTERVAL": "daemon.streamInterval",
		"DAEMON_CONTAINERS_INTERVAL": "daemon.containersInterval", "DAEMON_IMAGES_INTERVAL": "daemon.imagesInterval", "DAEMON_PROCESSES_INTERVAL": "daemon.processesInterval",
		"DAEMON_SYSTEMD_INTERVAL": "daemon.systemdInterval", "DAEMON_DATABASE_METRICS_INTERVAL": "daemon.databaseMetricsInterval", "DAEMON_PROCESSES_LIMIT": "daemon.processesLimit",
		"DAEMON_REQUEST_TIMEOUT": "daemon.requestTimeout",
		"DAEMON_SCRIPT_TIMEOUT":  "daemon.scriptTimeout", "DAEMON_MAX_BODY_BYTES": "daemon.maxBodyBytes",
		"DAEMON_MAX_CONCURRENT_RUNS": "daemon.maxConcurrentRuns",
	}
	for env, key := range aliases {
		if value, ok := os.LookupEnv(env); ok {
			viper.Set(key, value)
		}
	}
}

func (c *Config) ValidateCloud() error {
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn is required in cloud mode")
	}
	if strings.TrimSpace(c.Auth.Target) == "" {
		return fmt.Errorf("auth.target is required in cloud mode")
	}
	if strings.TrimSpace(c.Workspace.Target) == "" {
		return fmt.Errorf("workspace.target is required in cloud mode")
	}
	if err := validatePort(c.HTTP.Port); err != nil {
		return fmt.Errorf("http.port: %w", err)
	}
	if c.Cloud.DaemonDisconnectAfter < 0 {
		return fmt.Errorf("cloud.daemonDisconnectAfter must not be negative")
	}
	if c.Cloud.DaemonDisconnectNotificationCooldown < 0 {
		return fmt.Errorf("cloud.daemonDisconnectNotificationCooldown must not be negative")
	}
	if c.Cloud.AlarmCheckInterval < 0 {
		return fmt.Errorf("cloud.alarmCheckInterval must not be negative")
	}
	return nil
}

func (c *Config) ValidateDaemon() error {
	if strings.TrimSpace(c.Daemon.ID) == "" {
		return fmt.Errorf("daemon.id is required in daemon mode")
	}
	transport := strings.ToLower(strings.TrimSpace(c.Daemon.Transport))
	if transport == "" {
		transport = "http"
	}
	if transport != "http" && transport != "stdio" {
		return fmt.Errorf("daemon.transport must be http or stdio")
	}
	if transport == "http" {
		if err := validateListen(c.Daemon.Listen); err != nil {
			return fmt.Errorf("daemon.listen: %w", err)
		}
		if strings.TrimSpace(c.Daemon.MetricsSecret) == "" {
			return fmt.Errorf("daemon.metricsSecret is required for http transport")
		}
	}
	if c.Daemon.MetricsInterval <= 0 {
		return fmt.Errorf("daemon.metricsInterval must be positive")
	}
	if c.Daemon.StreamInterval <= 0 {
		return fmt.Errorf("daemon.streamInterval must be positive")
	}
	if c.Daemon.ContainersInterval < 0 {
		return fmt.Errorf("daemon.containersInterval must not be negative")
	}
	if c.Daemon.ImagesInterval < 0 {
		return fmt.Errorf("daemon.imagesInterval must not be negative")
	}
	if c.Daemon.ProcessesInterval < 0 {
		return fmt.Errorf("daemon.processesInterval must not be negative")
	}
	if c.Daemon.SystemdInterval < 0 {
		return fmt.Errorf("daemon.systemdInterval must not be negative")
	}
	if c.Daemon.LogsInterval < 0 {
		return fmt.Errorf("daemon.logsInterval must not be negative")
	}
	if c.Daemon.RuntimesInterval < 0 {
		return fmt.Errorf("daemon.runtimesInterval must not be negative")
	}
	if len(c.Daemon.Runtimes) == 0 {
		return fmt.Errorf("daemon.runtimes must not be empty")
	}
	runtimeNames := make(map[string]struct{}, len(c.Daemon.Runtimes))
	for i, name := range c.Daemon.Runtimes {
		if !runtimeNamePattern.MatchString(name) {
			return fmt.Errorf("daemon.runtimes[%d] must match [a-z][a-z0-9_-]*", i)
		}
		if _, ok := runtimeNames[name]; ok {
			return fmt.Errorf("daemon.runtimes[%d] %q is duplicated", i, name)
		}
		runtimeNames[name] = struct{}{}
	}
	watchedNames := make(map[string]struct{}, len(c.Daemon.WatchedProcesses))
	for i, name := range c.Daemon.WatchedProcesses {
		if !watchedProcessNamePattern.MatchString(name) {
			return fmt.Errorf("daemon.watchedProcesses[%d] must match [A-Za-z0-9][A-Za-z0-9._-]*", i)
		}
		if _, ok := watchedNames[name]; ok {
			return fmt.Errorf("daemon.watchedProcesses[%d] %q is duplicated", i, name)
		}
		watchedNames[name] = struct{}{}
	}
	if c.Daemon.ProcessesLimit < 1 || c.Daemon.ProcessesLimit > 500 {
		return fmt.Errorf("daemon.processesLimit must be between 1 and 500")
	}
	if c.Daemon.MetricsRetentionDays < 0 || c.Daemon.MetricsRetentionDays > 30 {
		return fmt.Errorf("daemon.metricsRetentionDays must be 0 (default) or between 1 and 30")
	}
	if c.Daemon.RequestTimeout <= 0 {
		return fmt.Errorf("daemon.requestTimeout must be positive")
	}
	if c.Daemon.ScriptTimeout <= 0 {
		return fmt.Errorf("daemon.scriptTimeout must be positive")
	}
	if c.Daemon.MaxBodyBytes <= 0 {
		return fmt.Errorf("daemon.maxBodyBytes must be positive")
	}
	if c.Daemon.MaxConcurrentRuns <= 0 {
		return fmt.Errorf("daemon.maxConcurrentRuns must be positive")
	}
	hookNames := make(map[string]struct{}, len(c.Daemon.Webhooks))
	for i, hook := range c.Daemon.Webhooks {
		if strings.TrimSpace(hook.Name) == "" || !webhookNamePattern.MatchString(hook.Name) {
			return fmt.Errorf("daemon.webhooks[%d].name must match [A-Za-z0-9._-]+", i)
		}
		if _, ok := hookNames[hook.Name]; ok {
			return fmt.Errorf("daemon.webhooks[%d].name %q is duplicated", i, hook.Name)
		}
		hookNames[hook.Name] = struct{}{}
		if IsNativeOpName(hook.Name) {
			return fmt.Errorf("daemon.webhooks[%d].name %q is reserved for a built-in operation", i, hook.Name)
		}
		if strings.TrimSpace(hook.Secret) == "" {
			return fmt.Errorf("daemon.webhooks[%d].secret is required", i)
		}
		if !filepath.IsAbs(hook.Command) {
			return fmt.Errorf("daemon.webhooks[%d].command must be an absolute path", i)
		}
		if err := validateHookExecution(hook, "webhooks", i); err != nil {
			return err
		}
	}
	// Webhooks and actions share the runtime namespace (the executor
	// registers both in one hook table), so a cross-kind name collision is
	// rejected just like a duplicate — with a message that names the other
	// kind, since a config showing one entry of each is easy to misread.
	actionNames := make(map[string]struct{}, len(c.Daemon.Actions))
	for i, action := range c.Daemon.Actions {
		if strings.TrimSpace(action.Name) == "" || !webhookNamePattern.MatchString(action.Name) {
			return fmt.Errorf("daemon.actions[%d].name must match [A-Za-z0-9._-]+", i)
		}
		if _, ok := hookNames[action.Name]; ok {
			return fmt.Errorf("daemon.actions[%d].name %q collides with a webhook of the same name", i, action.Name)
		}
		if _, ok := actionNames[action.Name]; ok {
			return fmt.Errorf("daemon.actions[%d].name %q is duplicated", i, action.Name)
		}
		actionNames[action.Name] = struct{}{}
		if IsNativeOpName(action.Name) {
			return fmt.Errorf("daemon.actions[%d].name %q is reserved for a built-in operation", i, action.Name)
		}
		if !filepath.IsAbs(action.Command) {
			return fmt.Errorf("daemon.actions[%d].command must be an absolute path", i)
		}
		if err := validateHookExecution(action, "actions", i); err != nil {
			return err
		}
	}
	jobNames := make(map[string]struct{}, len(c.Daemon.Jobs))
	for i, job := range c.Daemon.Jobs {
		job.Name = strings.TrimSpace(job.Name)
		if job.Name == "" || !webhookNamePattern.MatchString(job.Name) {
			return fmt.Errorf("daemon.jobs[%d].name must match [A-Za-z0-9._-]+", i)
		}
		if _, ok := jobNames[job.Name]; ok {
			return fmt.Errorf("daemon.jobs[%d].name %q is duplicated", i, job.Name)
		}
		jobNames[job.Name] = struct{}{}
		if strings.TrimSpace(job.Schedule) == "" {
			return fmt.Errorf("daemon.jobs[%d].schedule is required", i)
		}
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			return fmt.Errorf("daemon.jobs[%d].schedule %q is not a valid cron expression or descriptor: %w", i, job.Schedule, err)
		}
		if strings.TrimSpace(job.Action) == "" {
			return fmt.Errorf("daemon.jobs[%d].action is required", i)
		}
		if job.Timeout < 0 {
			return fmt.Errorf("daemon.jobs[%d].timeout must not be negative", i)
		}
		if job.Enabled == nil {
			enabled := true
			c.Daemon.Jobs[i].Enabled = &enabled
		}
	}
	alarmKinds := make(map[string]struct{}, len(c.Daemon.Alarms))
	for i := range c.Daemon.Alarms {
		alarm := &c.Daemon.Alarms[i]
		alarm.Kind = strings.TrimSpace(alarm.Kind)
		alarm.Target = strings.TrimSpace(alarm.Target)
		switch alarm.Kind {
		case "cpu_percent", "memory_used_percent", "disk_used_percent":
			if alarm.Threshold <= 0 || alarm.Threshold > 100 {
				return fmt.Errorf("daemon.alarms[%d].threshold must be between 0 and 100", i)
			}
			if alarm.Target != "" {
				return fmt.Errorf("daemon.alarms[%d].target is only supported for container_down", i)
			}
		case "container_down":
			// Empty targets intentionally match any down container.
		default:
			return fmt.Errorf("daemon.alarms[%d].kind must be cpu_percent, memory_used_percent, disk_used_percent, or container_down", i)
		}
		key := alarm.Kind + "\x00" + alarm.Target
		if _, ok := alarmKinds[key]; ok {
			return fmt.Errorf("daemon.alarms[%d].kind %q with target %q is duplicated", i, alarm.Kind, alarm.Target)
		}
		alarmKinds[key] = struct{}{}
		if alarm.CooldownSeconds <= 0 {
			alarm.CooldownSeconds = 300
		}
		// A fragment omitting `enabled` would otherwise silently disable the
		// alarm; default it on so a minimal fragment does what it looks like.
		if alarm.Enabled == nil {
			enabled := true
			alarm.Enabled = &enabled
		}
	}
	if err := ValidateCloudURL(c.Daemon.CloudURL); err != nil {
		return fmt.Errorf("daemon.cloudUrl %w", err)
	}
	return nil
}

// ValidateCloudURL checks a cloud endpoint: HTTPS, or HTTP to a loopback
// host (development). Empty URLs are allowed (publishing disabled).
func ValidateCloudURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
		return fmt.Errorf("must be HTTPS, or HTTP to localhost")
	}
	return nil
}

func validateListen(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("must not be empty")
	}
	address, err := net.ResolveTCPAddr("tcp", value)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}
	if address.Port != 0 && address.Port < 1024 {
		return fmt.Errorf("port %d requires root or CAP_NET_BIND_SERVICE; use a port >= 1024", address.Port)
	}
	return nil
}
func validatePort(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("must not be empty")
	}
	if _, err := net.ResolveTCPAddr("tcp", ":"+strings.TrimPrefix(value, ":")); err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}
	return nil
}
