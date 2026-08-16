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
	Daemon    DaemonConfig    `mapstructure:"daemon"`
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
	AlarmsDir          string          `mapstructure:"alarmsDir"`
	CloudURL           string          `mapstructure:"cloudUrl"`
	CloudSecret        string          `mapstructure:"cloudSecret"`
	MetricsInterval    time.Duration   `mapstructure:"metricsInterval"`
	StreamInterval     time.Duration   `mapstructure:"streamInterval"`
	ContainersInterval time.Duration   `mapstructure:"containersInterval"`
	ImagesInterval     time.Duration   `mapstructure:"imagesInterval"`
	ProcessesInterval  time.Duration   `mapstructure:"processesInterval"`
	SystemdInterval    time.Duration   `mapstructure:"systemdInterval"`
	ProcessesLimit     int             `mapstructure:"processesLimit"`
	RequestTimeout     time.Duration   `mapstructure:"requestTimeout"`
	ScriptTimeout      time.Duration   `mapstructure:"scriptTimeout"`
	MaxBodyBytes       int64           `mapstructure:"maxBodyBytes"`
	MaxConcurrentRuns  int             `mapstructure:"maxConcurrentRuns"`
	Webhooks           []WebhookConfig `mapstructure:"webhooks"`
	Actions            []WebhookConfig `mapstructure:"actions"`
	Alarms             []AlarmConfig   `mapstructure:"alarms"`
}

// AlarmConfig declares one metric threshold the daemon evaluates against its
// own samples. Evaluation is intentionally daemon-side: the daemon reports a
// `daemon.alarm.<kind>` notification to the cloud when the threshold is
// exceeded, so the cloud only stores and forwards the resulting notification
// and never needs to reach back into the daemon.
type AlarmConfig struct {
	Kind string `mapstructure:"kind"`
	// Threshold is the percentage (0..100] at or above which the alarm fires.
	Threshold float64 `mapstructure:"threshold"`
	// Enabled defaults to true when absent.
	Enabled *bool `mapstructure:"enabled"`
	// CooldownSeconds is the minimum gap between two triggers of the same
	// alarm; it defaults to 300 when absent. It is evaluated in memory, so a
	// daemon restart may re-fire once while the metric stays over threshold.
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

var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

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
	viper.SetDefault("daemon.cloudUrl", "https://mk.solsynth.dev")
	viper.SetDefault("daemon.cloudSecret", "")
	viper.SetDefault("daemon.metricsInterval", time.Minute)
	viper.SetDefault("daemon.streamInterval", time.Second)
	viper.SetDefault("daemon.containersInterval", 5*time.Second)
	viper.SetDefault("daemon.imagesInterval", time.Minute)
	viper.SetDefault("daemon.processesInterval", 10*time.Second)
	viper.SetDefault("daemon.systemdInterval", 30*time.Second)
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

func applyEnvAliases() {
	aliases := map[string]string{
		"AUTH_TARGET": "auth.target", "AUTH_USE_TLS": "auth.useTLS", "AUTH_TLS_SKIP_VERIFY": "auth.tlsSkipVerify",
		"WORKSPACE_TARGET": "workspace.target", "WORKSPACE_USE_TLS": "workspace.useTLS", "WORKSPACE_TLS_SKIP_VERIFY": "workspace.tlsSkipVerify",
		"RING_TARGET": "ring.target", "RING_USE_TLS": "ring.useTLS", "RING_TLS_SKIP_VERIFY": "ring.tlsSkipVerify",
		"EVENTBUS_URL": "eventbus.url", "EVENTBUS_SUBJECT_PREFIX": "eventbus.subjectPrefix", "DAEMON_ID": "daemon.id", "DAEMON_TRANSPORT": "daemon.transport", "DAEMON_LISTEN": "daemon.listen",
		"DAEMON_METRICS_SECRET":         "daemon.metricsSecret",
		"DAEMON_METRICS_HISTORY_PATH":   "daemon.metricsHistoryPath",
		"DAEMON_METRICS_RETENTION_DAYS": "daemon.metricsRetentionDays",
		"DAEMON_AUDIT_PATH":             "daemon.auditPath",
		"DAEMON_ACTIONS_DIR":            "daemon.actionsDir",
		"DAEMON_ALARMS_DIR":             "daemon.alarmsDir",
		"DAEMON_CLOUD_URL":              "daemon.cloudUrl", "DAEMON_CLOUD_SECRET": "daemon.cloudSecret",
		"DAEMON_METRICS_INTERVAL": "daemon.metricsInterval", "DAEMON_STREAM_INTERVAL": "daemon.streamInterval",
		"DAEMON_CONTAINERS_INTERVAL": "daemon.containersInterval", "DAEMON_IMAGES_INTERVAL": "daemon.imagesInterval", "DAEMON_PROCESSES_INTERVAL": "daemon.processesInterval",
		"DAEMON_SYSTEMD_INTERVAL": "daemon.systemdInterval", "DAEMON_PROCESSES_LIMIT": "daemon.processesLimit",
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
		if !filepath.IsAbs(action.Command) {
			return fmt.Errorf("daemon.actions[%d].command must be an absolute path", i)
		}
		if err := validateHookExecution(action, "actions", i); err != nil {
			return err
		}
	}
	alarmKinds := make(map[string]struct{}, len(c.Daemon.Alarms))
	for i := range c.Daemon.Alarms {
		alarm := &c.Daemon.Alarms[i]
		alarm.Kind = strings.TrimSpace(alarm.Kind)
		if alarm.Kind != "cpu_percent" && alarm.Kind != "memory_used_percent" {
			return fmt.Errorf("daemon.alarms[%d].kind must be cpu_percent or memory_used_percent", i)
		}
		if _, ok := alarmKinds[alarm.Kind]; ok {
			return fmt.Errorf("daemon.alarms[%d].kind %q is duplicated", i, alarm.Kind)
		}
		alarmKinds[alarm.Kind] = struct{}{}
		if alarm.Threshold <= 0 || alarm.Threshold > 100 {
			return fmt.Errorf("daemon.alarms[%d].threshold must be between 0 and 100", i)
		}
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
	if strings.TrimSpace(c.Daemon.CloudURL) != "" {
		u, err := url.Parse(c.Daemon.CloudURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
			return fmt.Errorf("daemon.cloudUrl must be HTTPS, or HTTP to localhost")
		}
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
