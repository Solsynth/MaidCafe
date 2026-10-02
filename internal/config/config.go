package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path"
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
	// AllowedOrigins lists the browser origins allowed to open the cloud's
	// WebSocket terminal socket, matched with path.Match against the Origin
	// host (or `scheme://host` when the pattern contains "://"). Empty allows
	// only same-host origins, so a browser client served from another origin
	// (e.g. a MaidKit web build) must be listed here. A request without an
	// Origin header is always accepted.
	AllowedOrigins []string `mapstructure:"allowedOrigins"`
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
	LogsDir string `mapstructure:"logsDir"`
	// LogAlertsDir holds one `<slug>.toml` fragment per regex log alert.
	LogAlertsDir string `mapstructure:"logAlertsDir"`
	// Terminal is the opt-in interactive shell served over WebSocket at
	// GET /api/v1/terminal. It exists so browser-based MaidKit builds, which
	// cannot open raw SSH sockets, can attach a terminal. It is unrelated to
	// the cloud relay: nothing about a terminal session is reported to the
	// cloud. Disabled by default.
	Terminal TerminalConfig `mapstructure:"terminal"`
	// Files is the opt-in file-management API served over HTTP
	// (/api/v1/files/...) and the stdio transport. It exists for the same
	// reason as Terminal: browser-based MaidKit builds cannot open an SFTP
	// channel, so the daemon serves the operations the file manager and the
	// editor need. Disabled by default.
	Files       FilesConfig `mapstructure:"files"`
	CloudURL    string      `mapstructure:"cloudUrl"`
	CloudSecret string      `mapstructure:"cloudSecret"`
	// LogsUploadEnabled opts the daemon into outbound log upload. It is
	// deliberately false by default because logs may contain secrets.
	LogsUploadEnabled    bool          `mapstructure:"logsUploadEnabled"`
	LogsUploadInterval   time.Duration `mapstructure:"logsUploadInterval"`
	LogsUploadBatchLines int           `mapstructure:"logsUploadBatchLines"`
	// ManagedContainers and ManagedComposes scope cloud uploads (logs and
	// container status) to a curated set. An empty managed list uploads every
	// container (the default, matching the pre-scoping behavior). Matching is
	// by exact container Name or ID prefix (ManagedContainers) or compose
	// project label (ManagedComposes, from com.docker.compose.project /
	// io.podman.compose.project).
	ManagedContainers []string `mapstructure:"managedContainers"`
	ManagedComposes   []string `mapstructure:"managedComposes"`
	// StatusUploadEnabled opts the daemon into publishing container status
	// (state, image, compose project) to the cloud so managed hosts can be
	// inspected centrally. Status carries no secrets, but the upload stays
	// opt-in and is paced on the metrics tick.
	StatusUploadEnabled     bool          `mapstructure:"statusUploadEnabled"`
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
	WatchedProcesses     []string `mapstructure:"watchedProcesses"`
	WatchedProcessesFile string   `mapstructure:"watchedProcessesFile"`
	ProcessesLimit       int      `mapstructure:"processesLimit"`
	// ScriptTimeout bounds each run that has no per-hook timeout. 0 disables
	// the deadline entirely: commands run until they complete (slow deploys,
	// long pulls). A run with no deadline still holds a concurrency slot and
	// blocks later relayed invocations while it runs.
	RequestTimeout    time.Duration    `mapstructure:"requestTimeout"`
	ScriptTimeout     time.Duration    `mapstructure:"scriptTimeout"`
	MaxBodyBytes      int64            `mapstructure:"maxBodyBytes"`
	MaxConcurrentRuns int              `mapstructure:"maxConcurrentRuns"`
	Webhooks          []WebhookConfig  `mapstructure:"webhooks"`
	Actions           []WebhookConfig  `mapstructure:"actions"`
	Alarms            []AlarmConfig    `mapstructure:"alarms"`
	Jobs              []JobConfig      `mapstructure:"jobs"`
	LogAlerts         []LogAlertConfig `mapstructure:"logAlerts"`
}

// TerminalConfig is the WebSocket terminal policy. The endpoint only exists in
// the http transport (stdio has no listener) and cannot start a session on
// Windows, where the daemon has no PTY.
type TerminalConfig struct {
	// Enabled turns the endpoint on. Off by default: enabling it grants an
	// interactive shell on this host to whoever holds the credential.
	Enabled bool `mapstructure:"enabled"`
	// Secret is an optional dedicated terminal credential. Empty (default)
	// accepts metricsSecret; setting it keeps a leaked metrics secret from
	// granting a shell.
	Secret string `mapstructure:"secret"`
	// Shells is the allowlist of absolute shell paths a session may request.
	// The first entry is the default. Required when Enabled.
	Shells []string `mapstructure:"shells"`
	// Users is the optional allowlist for `sudo -H -u <user>` sessions.
	// Empty means the daemon's own account only.
	Users []string `mapstructure:"users"`
	// Cwd is an optional absolute working directory. Empty starts in the
	// target account's home directory.
	Cwd string `mapstructure:"cwd"`
	// Env are optional KEY=VALUE entries added to the shell environment.
	Env []string `mapstructure:"env"`
	// AllowedOrigins lists the browser origins allowed to open the socket,
	// matched with path.Match against the Origin host (or `scheme://host`
	// when the pattern contains "://"). Empty allows only same-host origins,
	// so a browser client must be listed here. A request without an Origin
	// header (native client, tunnel) is always accepted.
	AllowedOrigins []string `mapstructure:"allowedOrigins"`
	// AllowRemote accepts sessions from addresses outside loopback, RFC1918,
	// link-local and Tailscale CGNAT (100.64.0.0/10). Off by default.
	AllowRemote bool `mapstructure:"allowRemote"`
	// MaxSessions caps concurrent sessions (1..8); 0 means the default (2).
	MaxSessions int `mapstructure:"maxSessions"`
	// IdleTimeout closes a session with no traffic in either direction; 0
	// disables the idle check.
	IdleTimeout time.Duration `mapstructure:"idleTimeout"`
	// MaxLifetime caps a session's absolute age; 0 disables the cap.
	MaxLifetime time.Duration `mapstructure:"maxLifetime"`
	// Relay serves sessions that arrive through the MaidCafe cloud instead of
	// this host's own listener, so a browser client can attach to a daemon
	// behind NAT. Off by default.
	Relay TerminalRelayConfig `mapstructure:"relay"`
}

// TerminalRelayConfig is the cloud-relayed terminal policy. A relayed session
// is one the cloud handed to this daemon: the daemon polls the cloud for
// pending sessions, dials out to the cloud for each accepted one, and both
// legs of the browser connection ride that outbound socket. Relayed sessions
// obey the same shell/user/idle policy as the direct endpoint and share its
// MaxSessions budget.
type TerminalRelayConfig struct {
	// Enabled turns relayed sessions on. Requires cloudUrl and cloudSecret;
	// the relay is outbound, so it also works in the stdio transport.
	Enabled bool `mapstructure:"enabled"`
	// Users is an optional allowlist of cloud identities allowed to open a
	// relayed session. Entries match the identity the cloud forwards (the
	// Solarpass handle, or the account id when no handle is known) exactly.
	// Empty accepts any identity, which means any account the cloud
	// authorizes — the cloud only authorizes members of the daemon's
	// workspace, but a workspace is not the same trust boundary as a shell.
	Users []string `mapstructure:"users"`
	// PollWait is how long one pickup long-poll holds before it returns empty,
	// which bounds session start latency and idle cloud traffic. 0 uses
	// TerminalRelayDefaultPollWait.
	PollWait time.Duration `mapstructure:"pollWait"`
}

// FilesConfig is the opt-in file-management API policy. The API serves the
// operations a browser-based MaidKit build cannot perform over SFTP: listing
// a directory, reading and writing file contents, and the create/rename/
// copy/delete mutations the file manager offers.
//
// Every request path must be absolute and lexically inside one of Roots. The
// actual I/O runs through os.Root, so a symlink that resolves outside its
// root is refused by the OS layer even when the requested path is inside it.
// Operations run as the daemon's own account: the API never elevates, so it
// can touch exactly what that account can touch.
type FilesConfig struct {
	// Enabled turns the API on. Off by default: enabling it grants file
	// access on this host to whoever holds the credential.
	Enabled bool `mapstructure:"enabled"`
	// Secret is an optional dedicated credential. Empty (default) accepts
	// metricsSecret; setting it keeps a leaked metrics secret from granting
	// file access.
	Secret string `mapstructure:"secret"`
	// Roots is the allowlist of absolute directories the API may touch.
	// Required when Enabled; paths outside every root are refused. A root
	// itself cannot be deleted through the API.
	Roots []string `mapstructure:"roots"`
	// AllowWrite permits the mutating operations (write, mkdir, move, copy,
	// delete). Off by default, so an operator can publish a read-only view of
	// a directory tree.
	AllowWrite bool `mapstructure:"allowWrite"`
	// MaxReadBytes caps one read request (editor load, download window);
	// 0 uses FilesDefaultMaxReadBytes. A larger file is still reachable by
	// paging with offset/limit.
	MaxReadBytes int64 `mapstructure:"maxReadBytes"`
	// MaxWriteBytes caps one write request body; 0 uses
	// FilesDefaultMaxWriteBytes.
	MaxWriteBytes int64 `mapstructure:"maxWriteBytes"`
	// MaxListEntries caps one directory listing; 0 uses
	// FilesDefaultMaxListEntries. A larger directory is reported truncated.
	MaxListEntries int `mapstructure:"maxListEntries"`
}

// File API bounds shared by config validation and the daemon's file runner.
// A zero value in FilesConfig means "use the default"; the limits exist so an
// operator can raise them deliberately rather than by accident.
const (
	FilesDefaultMaxReadBytes   = 8 << 20 // 8 MiB
	FilesDefaultMaxWriteBytes  = 8 << 20 // 8 MiB
	FilesMaxTransferBytes      = 1 << 30 // 1 GiB per request
	FilesDefaultMaxListEntries = 5000
	FilesMaxListEntriesLimit   = 100000
)

// LogAlertConfig declares one daemon-side regex alert. A matching new log
// line produces a cloud notification through the existing notification
// publisher, subject to the cooldown. An empty Container matches all
// containers.
type LogAlertConfig struct {
	Name            string `mapstructure:"name"`
	Pattern         string `mapstructure:"pattern"`
	Container       string `mapstructure:"container"`
	Title           string `mapstructure:"title"`
	Enabled         *bool  `mapstructure:"enabled"`
	CooldownSeconds int    `mapstructure:"cooldownSeconds"`
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
	// "2m"); zero or absent uses the daemon-wide value (which 0 disables).
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
	// the daemon-wide value (which 0 disables; native compose ops keep their
	// own 5m bound).
	Timeout time.Duration `mapstructure:"timeout"`
	// NotifyOnFailure publishes a job.failure notification when a run exits
	// non-zero or fails to start.
	NotifyOnFailure bool `mapstructure:"notifyOnFailure"`
}

var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Terminal session bounds shared by config validation and the daemon's
// session manager. MaxSessions == 0 means TerminalDefaultMaxSessions: a zero
// value is what a caller that builds DaemonConfig directly (rather than
// through Load) leaves behind.
const (
	TerminalDefaultMaxSessions = 2
	TerminalMaxSessionsLimit   = 8

	// TerminalRelayDefaultPollWait is how long one cloud relay pickup
	// long-poll holds before it returns empty, used when
	// daemon.terminal.relay.pollWait is 0.
	TerminalRelayDefaultPollWait = 20 * time.Second
	// TerminalRelayMinPollWait rejects a wait so short it would hammer the
	// cloud's pickup route; the daemon re-polls immediately, so a tiny wait
	// becomes a busy loop.
	TerminalRelayMinPollWait = 3 * time.Second
	// TerminalRelayMaxPollWait keeps the long-poll inside the cloud's own
	// hold cap, so a stuck poll can never outlive the cloud's response.
	TerminalRelayMaxPollWait = 25 * time.Second
)

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

// validateOriginPattern checks one browser-origin allowlist entry, shared by
// the daemon's terminal endpoint and the cloud's relayed terminal socket. A
// bare "*" is rejected because path.Match would then authorize every origin;
// an operator who wants that writes a wildcard host pattern such as
// "*.example.com".
func validateOriginPattern(origin string) error {
	if origin == "" || strings.TrimSpace(origin) != origin || origin == "*" {
		return fmt.Errorf("must be a host or scheme://host pattern (no bare *)")
	}
	if _, err := path.Match(strings.ToLower(origin), "x"); err != nil {
		return fmt.Errorf("%q is not a valid pattern: %w", origin, err)
	}
	return nil
}

// validateTerminal checks the WebSocket terminal policy. Numeric zero means
// "use the documented default" (maxSessions 2, no idle/lifetime cap) so a
// DaemonConfig built directly instead of through Load still validates; the
// runtime resolves the same defaults.
func validateTerminal(cfg TerminalConfig) error {
	if (cfg.Enabled || cfg.Relay.Enabled) && len(cfg.Shells) == 0 {
		return fmt.Errorf("daemon.terminal.shells must not be empty when the terminal is enabled")
	}
	shells := make(map[string]struct{}, len(cfg.Shells))
	for i, shell := range cfg.Shells {
		if !filepath.IsAbs(shell) {
			return fmt.Errorf("daemon.terminal.shells[%d] must be an absolute path", i)
		}
		if _, ok := shells[shell]; ok {
			return fmt.Errorf("daemon.terminal.shells[%d] %q is duplicated", i, shell)
		}
		shells[shell] = struct{}{}
	}
	users := make(map[string]struct{}, len(cfg.Users))
	for i, name := range cfg.Users {
		if _, ok := users[name]; ok {
			return fmt.Errorf("daemon.terminal.users[%d] %q is duplicated", i, name)
		}
		users[name] = struct{}{}
		if _, err := user.Lookup(name); err != nil {
			return fmt.Errorf("daemon.terminal.users[%d] %q does not exist: %w", i, name, err)
		}
	}
	if cfg.Cwd != "" && !filepath.IsAbs(cfg.Cwd) {
		return fmt.Errorf("daemon.terminal.cwd must be an absolute path")
	}
	for i, kv := range cfg.Env {
		if !envAssignmentPattern.MatchString(kv) {
			return fmt.Errorf("daemon.terminal.env[%d] must be KEY=VALUE with an identifier key", i)
		}
	}
	if strings.TrimSpace(cfg.Secret) != cfg.Secret {
		return fmt.Errorf("daemon.terminal.secret must not contain whitespace")
	}
	for i, origin := range cfg.AllowedOrigins {
		if err := validateOriginPattern(origin); err != nil {
			return fmt.Errorf("daemon.terminal.allowedOrigins[%d] %w", i, err)
		}
	}
	if cfg.MaxSessions < 0 || cfg.MaxSessions > TerminalMaxSessionsLimit {
		return fmt.Errorf("daemon.terminal.maxSessions must be between 1 and %d", TerminalMaxSessionsLimit)
	}
	if cfg.IdleTimeout < 0 {
		return fmt.Errorf("daemon.terminal.idleTimeout must not be negative")
	}
	if cfg.MaxLifetime < 0 {
		return fmt.Errorf("daemon.terminal.maxLifetime must not be negative")
	}
	if cfg.MaxLifetime > 0 && cfg.IdleTimeout > 0 && cfg.MaxLifetime < cfg.IdleTimeout {
		return fmt.Errorf("daemon.terminal.maxLifetime must not be shorter than idleTimeout")
	}
	relayUsers := make(map[string]struct{}, len(cfg.Relay.Users))
	for i, name := range cfg.Relay.Users {
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("daemon.terminal.relay.users[%d] must be a non-empty cloud identity", i)
		}
		if _, ok := relayUsers[name]; ok {
			return fmt.Errorf("daemon.terminal.relay.users[%d] %q is duplicated", i, name)
		}
		relayUsers[name] = struct{}{}
	}
	if cfg.Relay.PollWait != 0 && (cfg.Relay.PollWait < TerminalRelayMinPollWait || cfg.Relay.PollWait > TerminalRelayMaxPollWait) {
		return fmt.Errorf("daemon.terminal.relay.pollWait must be between %s and %s",
			TerminalRelayMinPollWait, TerminalRelayMaxPollWait)
	}
	return nil
}

// validateFiles checks the opt-in file API policy. Shape and limits are
// checked whenever they are set; the presence rules (roots) only apply when
// the API is enabled, so a disabled-but-present table never blocks startup.
func validateFiles(cfg FilesConfig) error {
	if cfg.Secret != "" && strings.ContainsAny(cfg.Secret, " \t\r\n") {
		return fmt.Errorf("daemon.files.secret must not contain whitespace")
	}
	if cfg.MaxReadBytes < 0 || cfg.MaxWriteBytes < 0 {
		return fmt.Errorf("daemon.files maxReadBytes and maxWriteBytes must not be negative")
	}
	if cfg.MaxReadBytes > FilesMaxTransferBytes || cfg.MaxWriteBytes > FilesMaxTransferBytes {
		return fmt.Errorf("daemon.files maxReadBytes and maxWriteBytes must be at most %d bytes", FilesMaxTransferBytes)
	}
	if cfg.MaxListEntries < 0 || cfg.MaxListEntries > FilesMaxListEntriesLimit {
		return fmt.Errorf("daemon.files.maxListEntries must be between 1 and %d", FilesMaxListEntriesLimit)
	}
	if !cfg.Enabled {
		return nil
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("daemon.files.roots must not be empty when the file API is enabled")
	}
	seen := make(map[string]struct{}, len(cfg.Roots))
	for i, root := range cfg.Roots {
		if strings.TrimSpace(root) != root || root == "" {
			return fmt.Errorf("daemon.files.roots[%d] must be a non-empty absolute path", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("daemon.files.roots[%d] %q must be an absolute path", i, root)
		}
		clean := filepath.Clean(root)
		if _, ok := seen[clean]; ok {
			return fmt.Errorf("daemon.files.roots[%d] %q is duplicated", i, root)
		}
		seen[clean] = struct{}{}
		info, err := os.Stat(clean)
		if err != nil {
			return fmt.Errorf("daemon.files.roots[%d] %q is not usable: %w", i, root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("daemon.files.roots[%d] %q is not a directory", i, root)
		}
	}
	return nil
}

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
	viper.SetDefault("daemon.logAlertsDir", "/etc/maidcafe/log-alerts")
	viper.SetDefault("daemon.logsUploadEnabled", false)
	viper.SetDefault("daemon.logsUploadInterval", 30*time.Second)
	viper.SetDefault("daemon.logsUploadBatchLines", 100)

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
	viper.SetDefault("daemon.logsInterval", 30*time.Second)
	viper.SetDefault("daemon.cloudUrl", "https://mk.solsynth.dev")
	viper.SetDefault("daemon.cloudSecret", "")
	viper.SetDefault("daemon.metricsInterval", time.Minute)
	viper.SetDefault("daemon.streamInterval", time.Second)
	viper.SetDefault("daemon.containersInterval", 5*time.Second)
	viper.SetDefault("daemon.imagesInterval", time.Minute)
	viper.SetDefault("daemon.processesInterval", 10*time.Second)
	viper.SetDefault("daemon.systemdInterval", 30*time.Second)
	viper.SetDefault("daemon.runtimesInterval", 10*time.Second)
	viper.SetDefault("daemon.databaseMetricsInterval", 10*time.Second)
	viper.SetDefault("daemon.runtimes", []string{"java", "dotnet", "python", "node", "deno", "go", "ruby", "php"})
	viper.SetDefault("daemon.watchedProcesses", []string{})
	viper.SetDefault("daemon.watchedProcessesFile", "/var/lib/maidcafe/watched-processes.json")
	viper.SetDefault("daemon.processesLimit", 50)
	viper.SetDefault("daemon.requestTimeout", 10*time.Second)
	viper.SetDefault("cloud.daemonDisconnectAfter", 5*time.Minute)
	viper.SetDefault("cloud.daemonDisconnectNotificationCooldown", 30*time.Minute)
	viper.SetDefault("cloud.alarmCheckInterval", time.Minute)
	viper.SetDefault("daemon.scriptTimeout", 30*time.Second)
	viper.SetDefault("daemon.maxBodyBytes", int64(65536))
	viper.SetDefault("daemon.maxConcurrentRuns", 4)
	viper.SetDefault("daemon.files.enabled", false)
	viper.SetDefault("daemon.files.secret", "")
	viper.SetDefault("daemon.files.roots", []string{})
	viper.SetDefault("daemon.files.allowWrite", false)
	viper.SetDefault("daemon.files.maxReadBytes", int64(FilesDefaultMaxReadBytes))
	viper.SetDefault("daemon.files.maxWriteBytes", int64(FilesDefaultMaxWriteBytes))
	viper.SetDefault("daemon.files.maxListEntries", FilesDefaultMaxListEntries)
	viper.SetDefault("http.allowedOrigins", []string{})
	viper.SetDefault("daemon.terminal.enabled", false)
	viper.SetDefault("daemon.terminal.secret", "")
	viper.SetDefault("daemon.terminal.shells", []string{})
	viper.SetDefault("daemon.terminal.users", []string{})
	viper.SetDefault("daemon.terminal.cwd", "")
	viper.SetDefault("daemon.terminal.env", []string{})
	viper.SetDefault("daemon.terminal.allowedOrigins", []string{})
	viper.SetDefault("daemon.terminal.allowRemote", false)
	viper.SetDefault("daemon.terminal.maxSessions", TerminalDefaultMaxSessions)
	viper.SetDefault("daemon.terminal.idleTimeout", 15*time.Minute)
	viper.SetDefault("daemon.terminal.maxLifetime", 8*time.Hour)
	viper.SetDefault("daemon.terminal.relay.enabled", false)
	viper.SetDefault("daemon.terminal.relay.users", []string{})
	viper.SetDefault("daemon.terminal.relay.pollWait", TerminalRelayDefaultPollWait)
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
	if err := cfg.loadLogAlertFragments(); err != nil {
		return nil, err
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

// loadLogAlertFragments merges one regex alert per `<slug>.toml` under
// daemon.logAlertsDir. Fragment definitions override inline alerts with the
// same name.
func (c *Config) loadLogAlertFragments() error {
	dir := strings.TrimSpace(c.Daemon.LogAlertsDir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read log alerts dir %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	fragments := make([]LogAlertConfig, 0, len(paths))
	names := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		alert, err := loadLogAlertFragment(path)
		if err != nil {
			return err
		}
		names[alert.Name] = struct{}{}
		fragments = append(fragments, alert)
	}
	inline := c.Daemon.LogAlerts
	c.Daemon.LogAlerts = make([]LogAlertConfig, 0, len(inline)+len(fragments))
	for _, alert := range inline {
		if _, covered := names[alert.Name]; !covered {
			c.Daemon.LogAlerts = append(c.Daemon.LogAlerts, alert)
		}
	}
	c.Daemon.LogAlerts = append(c.Daemon.LogAlerts, fragments...)
	return nil
}

func loadLogAlertFragment(path string) (LogAlertConfig, error) {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return LogAlertConfig{}, fmt.Errorf("read log alert fragment %s: %w", path, err)
	}
	var alert LogAlertConfig
	if err := v.Unmarshal(&alert); err != nil {
		return LogAlertConfig{}, fmt.Errorf("parse log alert fragment %s: %w", path, err)
	}
	if strings.TrimSpace(alert.Name) == "" {
		return LogAlertConfig{}, fmt.Errorf("log alert fragment %s has no name", path)
	}
	return alert, nil
}

func applyEnvAliases() {
	aliases := map[string]string{
		"AUTH_TARGET": "auth.target", "AUTH_USE_TLS": "auth.useTLS", "AUTH_TLS_SKIP_VERIFY": "auth.tlsSkipVerify",
		"WORKSPACE_TARGET": "workspace.target", "WORKSPACE_USE_TLS": "workspace.useTLS", "WORKSPACE_TLS_SKIP_VERIFY": "workspace.tlsSkipVerify",
		"RING_TARGET": "ring.target", "RING_USE_TLS": "ring.useTLS", "RING_TLS_SKIP_VERIFY": "ring.tlsSkipVerify",
		"EVENTBUS_URL": "eventbus.url", "EVENTBUS_SUBJECT_PREFIX": "eventbus.subjectPrefix",
		"CLOUD_DAEMON_DISCONNECT_AFTER": "cloud.daemonDisconnectAfter", "CLOUD_DAEMON_DISCONNECT_NOTIFICATION_COOLDOWN": "cloud.daemonDisconnectNotificationCooldown", "CLOUD_ALARM_CHECK_INTERVAL": "cloud.alarmCheckInterval",
		"DAEMON_ID": "daemon.id", "DAEMON_TRANSPORT": "daemon.transport", "DAEMON_LISTEN": "daemon.listen",
		"DAEMON_METRICS_SECRET":          "daemon.metricsSecret",
		"DAEMON_METRICS_HISTORY_PATH":    "daemon.metricsHistoryPath",
		"DAEMON_METRICS_RETENTION_DAYS":  "daemon.metricsRetentionDays",
		"DAEMON_AUDIT_PATH":              "daemon.auditPath",
		"DAEMON_ACTIONS_DIR":             "daemon.actionsDir",
		"DAEMON_ALARMS_DIR":              "daemon.alarmsDir",
		"DAEMON_JOBS_DIR":                "daemon.jobsDir",
		"DAEMON_LOG_ALERTS_DIR":          "daemon.logAlertsDir",
		"DAEMON_LOGS_DIR":                "daemon.logsDir",
		"DAEMON_LOGS_INTERVAL":           "daemon.logsInterval",
		"DAEMON_LOGS_UPLOAD_ENABLED":     "daemon.logsUploadEnabled",
		"DAEMON_LOGS_UPLOAD_INTERVAL":    "daemon.logsUploadInterval",
		"DAEMON_LOGS_UPLOAD_BATCH_LINES": "daemon.logsUploadBatchLines",
		"DAEMON_CLOUD_URL":               "daemon.cloudUrl", "DAEMON_CLOUD_SECRET": "daemon.cloudSecret",
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
	for i, origin := range c.HTTP.AllowedOrigins {
		if err := validateOriginPattern(origin); err != nil {
			return fmt.Errorf("http.allowedOrigins[%d] %w", i, err)
		}
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
	if c.Daemon.ScriptTimeout < 0 {
		return fmt.Errorf("daemon.scriptTimeout must not be negative (0 disables the run deadline)")
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
	if c.Daemon.LogsUploadInterval < 0 {
		return fmt.Errorf("daemon.logsUploadInterval must not be negative")
	}
	if c.Daemon.LogsUploadBatchLines <= 0 {
		c.Daemon.LogsUploadBatchLines = 100
	}
	if c.Daemon.LogsUploadBatchLines > 500 {
		return fmt.Errorf("daemon.logsUploadBatchLines must be between 1 and 500")
	}
	if c.Daemon.LogsUploadEnabled && c.Daemon.LogsUploadInterval <= 0 {
		return fmt.Errorf("daemon.logsUploadInterval must be positive when logsUploadEnabled is true")
	}
	if c.Daemon.MaxBodyBytes <= 0 {
		return fmt.Errorf("daemon.maxBodyBytes must be positive")
	}
	if c.Daemon.MaxConcurrentRuns <= 0 {
		return fmt.Errorf("daemon.maxConcurrentRuns must be positive")
	}
	logAlertNames := make(map[string]struct{}, len(c.Daemon.LogAlerts))
	for i := range c.Daemon.LogAlerts {
		alert := &c.Daemon.LogAlerts[i]
		alert.Name = strings.TrimSpace(alert.Name)
		alert.Pattern = strings.TrimSpace(alert.Pattern)
		alert.Container = strings.TrimSpace(alert.Container)
		alert.Title = strings.TrimSpace(alert.Title)
		if alert.Name == "" || !webhookNamePattern.MatchString(alert.Name) {
			return fmt.Errorf("daemon.logAlerts[%d].name must match [A-Za-z0-9._-]+", i)
		}
		if _, ok := logAlertNames[alert.Name]; ok {
			return fmt.Errorf("daemon.logAlerts[%d].name %q is duplicated", i, alert.Name)
		}
		logAlertNames[alert.Name] = struct{}{}
		if alert.Pattern == "" {
			return fmt.Errorf("daemon.logAlerts[%d].pattern is required", i)
		}
		if _, err := regexp.Compile(alert.Pattern); err != nil {
			return fmt.Errorf("daemon.logAlerts[%d].pattern is invalid: %w", i, err)
		}
		if alert.CooldownSeconds <= 0 {
			alert.CooldownSeconds = 300
		}
		if alert.Enabled == nil {
			enabled := true
			alert.Enabled = &enabled
		}
		if alert.Title == "" {
			alert.Title = "Container log alert: " + alert.Name
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
	if c.Daemon.Terminal.Relay.Enabled {
		// The relay is outbound to the cloud: without a cloud endpoint and
		// secret there is nothing to poll and nowhere to dial back to.
		if strings.TrimSpace(c.Daemon.CloudURL) == "" {
			return fmt.Errorf("daemon.cloudUrl is required when daemon.terminal.relay.enabled")
		}
		if strings.TrimSpace(c.Daemon.CloudSecret) == "" {
			return fmt.Errorf("daemon.cloudSecret is required when daemon.terminal.relay.enabled")
		}
	}
	if err := validateTerminal(c.Daemon.Terminal); err != nil {
		return err
	}
	if err := validateFiles(c.Daemon.Files); err != nil {
		return err
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
