package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Config API: GET returns the merged configuration with secrets redacted;
// PATCH patches a safe subset of [daemon] keys into the config file
// (preserving every other line, section and comment verbatim) and hot-
// reloads. Both authenticate like the actions route: metrics secret plus a
// body signature.
//
// Restart-required settings (transport, listen, paths, the metrics secret)
// are read-only here: the components that own them are constructed once.

// patchableDaemonKeys is the whitelist of [daemon] keys PATCH accepts,
// mapped to a validation + TOML serialization function.
type patchRule struct {
	// validate returns an error when the decoded value is unusable.
	validate func(value any) error
	// toml serializes the decoded value as a TOML literal.
	toml func(value any) (string, error)
}

var patchDurationRule = patchRule{
	validate: func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a duration string")
		}
		_, err := time.ParseDuration(text)
		return err
	},
	toml: func(value any) (string, error) {
		return tomlString(value.(string)), nil
	},
}

func patchIntRule(min, max int64) patchRule {
	return patchRule{
		validate: func(value any) error {
			number, ok := value.(float64)
			if !ok {
				return fmt.Errorf("must be an integer")
			}
			n := int64(number)
			if number != float64(n) || n < min || n > max {
				return fmt.Errorf("must be between %d and %d", min, max)
			}
			return nil
		},
		toml: func(value any) (string, error) {
			return strconv.FormatInt(int64(value.(float64)), 10), nil
		},
	}
}

var patchStringListRule = patchRule{
	validate: func(value any) error {
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("must be an array of strings")
		}
		for _, entry := range list {
			if _, ok := entry.(string); !ok {
				return fmt.Errorf("must be an array of strings")
			}
		}
		return nil
	},
	toml: func(value any) (string, error) {
		list := value.([]any)
		quoted := make([]string, 0, len(list))
		for _, entry := range list {
			quoted = append(quoted, tomlString(entry.(string)))
		}
		return "[" + strings.Join(quoted, ", ") + "]", nil
	},
}

var patchCloudURLRule = patchRule{
	validate: func(value any) error {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("must be a URL string")
		}
		return config.ValidateCloudURL(text)
	},
	toml: func(value any) (string, error) {
		return tomlString(strings.TrimSpace(value.(string))), nil
	},
}

var patchSecretRule = patchRule{
	validate: func(value any) error {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string")
		}
		return nil
	},
	toml: func(value any) (string, error) {
		return tomlString(value.(string)), nil
	},
}

// patchableDaemonKeys maps a PATCH key to its rule.
var patchableDaemonKeys = map[string]patchRule{
	"metricsInterval":         patchDurationRule,
	"streamInterval":          patchDurationRule,
	"containersInterval":      patchDurationRule,
	"imagesInterval":          patchDurationRule,
	"processesInterval":       patchDurationRule,
	"systemdInterval":         patchDurationRule,
	"runtimesInterval":        patchDurationRule,
	"databaseMetricsInterval": patchDurationRule,
	"logsInterval":            patchDurationRule,
	"scriptTimeout":           patchDurationRule,
	"processesLimit":          patchIntRule(1, 500),
	"maxBodyBytes":            patchIntRule(1, 1<<30),
	"maxConcurrentRuns":       patchIntRule(1, 256),
	"runtimes":                patchStringListRule,
	"watchedProcesses":        patchStringListRule,
	"cloudUrl":                patchCloudURLRule,
	"cloudSecret":             patchSecretRule,
}

func tomlString(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "\n", "\\n"))
}

// handleGetConfig serves the merged configuration with secrets redacted.
func (a *App) handleGetConfig(c *gin.Context) {
	if strings.TrimSpace(a.configPath) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "no config file is configured"})
		return
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":     a.configPath,
		"config":   newRedactedConfigView(cfg.Daemon),
		"webhooks": redactedWebhooks(cfg.Daemon.Webhooks),
		"actions":  cfg.Daemon.Actions,
		"alarms":   cfg.Daemon.Alarms,
		"jobs":     cfg.Daemon.Jobs,
	})
}

// redactedConfigView exposes the daemon configuration without secrets.
type redactedConfigView struct {
	ID                   string   `json:"id"`
	HostID               string   `json:"host_id"`
	Version              string   `json:"version"`
	Transport            string   `json:"transport"`
	Listen               string   `json:"listen"`
	CloudURL             string   `json:"cloud_url"`
	MetricsHistoryPath   string   `json:"metrics_history_path"`
	MetricsRetentionDays int      `json:"metrics_retention_days"`
	AuditPath            string   `json:"audit_path"`
	ActionsDir           string   `json:"actions_dir"`
	AlarmsDir            string   `json:"alarms_dir"`
	JobsDir              string   `json:"jobs_dir"`
	LogsDir              string   `json:"logs_dir"`
	MetricsInterval      string   `json:"metrics_interval"`
	StreamInterval       string   `json:"stream_interval"`
	ContainersInterval   string   `json:"containers_interval"`
	ImagesInterval       string   `json:"images_interval"`
	ProcessesInterval    string   `json:"processes_interval"`
	SystemdInterval      string   `json:"systemd_interval"`
	RuntimesInterval     string   `json:"runtimes_interval"`
	LogsInterval         string   `json:"logs_interval"`
	ProcessesLimit       int      `json:"processes_limit"`
	ScriptTimeout        string   `json:"script_timeout"`
	MaxBodyBytes         int64    `json:"max_body_bytes"`
	MaxConcurrentRuns    int      `json:"max_concurrent_runs"`
	Runtimes             []string `json:"runtimes"`
	WatchedProcesses     []string `json:"watched_processes"`
}

func newRedactedConfigView(cfg config.DaemonConfig) redactedConfigView {
	return redactedConfigView{
		ID:                   cfg.ID,
		HostID:               cfg.HostID,
		Version:              cfg.Version,
		Transport:            cfg.Transport,
		Listen:               cfg.Listen,
		CloudURL:             cfg.CloudURL,
		MetricsHistoryPath:   cfg.MetricsHistoryPath,
		MetricsRetentionDays: cfg.MetricsRetentionDays,
		AuditPath:            cfg.AuditPath,
		ActionsDir:           cfg.ActionsDir,
		AlarmsDir:            cfg.AlarmsDir,
		JobsDir:              cfg.JobsDir,
		LogsDir:              cfg.LogsDir,
		MetricsInterval:      cfg.MetricsInterval.String(),
		StreamInterval:       cfg.StreamInterval.String(),
		ContainersInterval:   cfg.ContainersInterval.String(),
		ImagesInterval:       cfg.ImagesInterval.String(),
		ProcessesInterval:    cfg.ProcessesInterval.String(),
		SystemdInterval:      cfg.SystemdInterval.String(),
		RuntimesInterval:     cfg.RuntimesInterval.String(),
		LogsInterval:         cfg.LogsInterval.String(),
		ProcessesLimit:       cfg.ProcessesLimit,
		ScriptTimeout:        cfg.ScriptTimeout.String(),
		MaxBodyBytes:         cfg.MaxBodyBytes,
		MaxConcurrentRuns:    cfg.MaxConcurrentRuns,
		Runtimes:             cfg.Runtimes,
		WatchedProcesses:     cfg.WatchedProcesses,
	}
}

// redactedWebhooks lists webhooks with their secrets blanked.
func redactedWebhooks(webhooks []config.WebhookConfig) []map[string]any {
	out := make([]map[string]any, 0, len(webhooks))
	for _, hook := range webhooks {
		out = append(out, map[string]any{
			"name":            hook.Name,
			"secret":          "",
			"command":         hook.Command,
			"args":            hook.Args,
			"enabled":         hook.Enabled,
			"notifyOnSuccess": hook.NotifyOnSuccess,
			"notifyOnFailure": hook.NotifyOnFailure,
			"displayName":     hook.DisplayName,
			"script":          hook.Script,
			"cwd":             hook.Cwd,
			"user":            hook.User,
			"env":             hook.Env,
			"timeout":         hook.Timeout.String(),
		})
	}
	return out
}

// handlePatchConfig patches a whitelisted subset of [daemon] keys into the
// config file and hot-reloads. The file is written atomically; on a failed
// reload the previous text is restored so a bad PATCH never strands the
// daemon on an invalid on-disk config.
func (a *App) handlePatchConfig(c *gin.Context) {
	if strings.TrimSpace(a.configPath) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "no config file is configured"})
		return
	}
	body, requestErr := a.readSignedBody(c, a.rt.Load().maxBodyBytes)
	if requestErr != nil {
		c.JSON(requestErr.status, gin.H{"ok": false, "error": requestErr.message})
		return
	}
	var patch map[string]any
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "empty patch"})
		return
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid JSON body"})
		return
	}
	values := make(map[string]string, len(patch))
	for key, value := range patch {
		rule, ok := patchableDaemonKeys[key]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": fmt.Sprintf("unsupported config key %q", key)})
			return
		}
		if err := rule.validate(value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": fmt.Sprintf("%s: %v", key, err)})
			return
		}
		serialized, err := rule.toml(value)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": fmt.Sprintf("%s: %v", key, err)})
			return
		}
		values[key] = serialized
	}
	path := a.configPath
	existing, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": fmt.Sprintf("read config file: %v", err)})
		return
	}
	patched := patchDaemonSection(string(existing), values)
	if err := writeFileAtomic(path, []byte(patched)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": fmt.Sprintf("write config file: %v (does the daemon user have write access to %s?)", err, filepath.Dir(path))})
		return
	}
	if err := a.Reload(); err != nil {
		_ = writeFileAtomic(path, existing)
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": fmt.Sprintf("config written but reload failed: %v (previous config restored)", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "path": path, "patched": keysOf(values)})
}

func keysOf(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// readSignedBody reads a bounded request body and verifies the metrics
// secret signature over it, mirroring the actions route.
func (a *App) readSignedBody(c *gin.Context, maxBodyBytes int64) ([]byte, *requestError) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, &requestError{status: http.StatusBadRequest, message: "read request body"}
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, &requestError{status: http.StatusRequestEntityTooLarge, message: "request body too large"}
	}
	if !signatureValid(a.cfg.MetricsSecret, body, c.GetHeader("X-MaidCafe-Signature")) {
		return nil, &requestError{status: http.StatusUnauthorized, message: "unauthorized"}
	}
	return body, nil
}

// patchDaemonSection replaces or inserts `key = value` lines under the
// [daemon] section of [text], preserving every other line (comments, unknown
// keys, other sections, [[daemon.actions]] sub-tables) verbatim. Values are
// pre-serialized TOML literals. Keys not present in the section are appended
// before the next section header (or at the end); a missing [daemon] section
// is created at the end.
func patchDaemonSection(text string, values map[string]string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+len(values))
	written := make(map[string]bool, len(values))
	inDaemon := false
	foundDaemon := false
	flushMissing := func() {
		for key, value := range values {
			if !written[key] {
				out = append(out, key+" = "+value)
			}
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == "[daemon]" {
				foundDaemon = true
				inDaemon = true
				out = append(out, line)
				continue
			}
			if inDaemon {
				flushMissing()
			}
			inDaemon = false
			out = append(out, line)
			continue
		}
		if inDaemon {
			replaced := false
			for key, value := range values {
				if written[key] {
					continue
				}
				prefix := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(key) + `\s*=`)
				if match := prefix.FindStringSubmatch(line); match != nil {
					out = append(out, match[1]+key+" = "+value)
					written[key] = true
					replaced = true
					break
				}
			}
			if replaced {
				continue
			}
		}
		out = append(out, line)
	}
	if inDaemon {
		flushMissing()
	}
	if !foundDaemon {
		out = append(out, "[daemon]")
		flushMissing()
	}
	return strings.Join(out, "\n")
}

// writeFileAtomic writes data via temp file + rename so the config watcher
// never observes a half-written file.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
