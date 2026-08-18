package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// reloadableConfig is the slice of the daemon's configuration that file
// watchers, the config API and SIGHUP can swap without restarting. It lives
// behind an atomic pointer so a reader always sees one consistent snapshot.
// Everything else (transport, listen address, audit/history paths, the
// metrics secret, collector storage directories) is restart-required: the
// components that own them are constructed once.
type reloadableConfig struct {
	actions              []config.WebhookConfig
	alarms               []config.AlarmConfig
	jobs                 []config.JobConfig
	logAlerts            []config.LogAlertConfig
	runtimes             []string
	watchedProcesses     []string
	processesLimit       int
	scriptTimeout        time.Duration
	maxBodyBytes         int64
	maxConcurrentRuns    int
	cloudURL             string
	cloudSecret          string
	logsUploadEnabled    bool
	logsUploadInterval   time.Duration
	logsUploadBatchLines int
	version              string
	intervals            reloadableIntervals
}

type reloadableIntervals struct {
	metrics    time.Duration
	stream     time.Duration
	containers time.Duration
	images     time.Duration
	processes  time.Duration
	systemd    time.Duration
	runtimes   time.Duration
	database   time.Duration
	logs       time.Duration
}

func newReloadableConfig(cfg config.DaemonConfig) *reloadableConfig {
	batchLines := cfg.LogsUploadBatchLines
	if batchLines <= 0 {
		batchLines = 100
	}
	return &reloadableConfig{
		actions:              cfg.Actions,
		alarms:               cfg.Alarms,
		jobs:                 cfg.Jobs,
		logAlerts:            cfg.LogAlerts,
		runtimes:             cfg.Runtimes,
		watchedProcesses:     cfg.WatchedProcesses,
		processesLimit:       cfg.ProcessesLimit,
		scriptTimeout:        cfg.ScriptTimeout,
		maxBodyBytes:         cfg.MaxBodyBytes,
		maxConcurrentRuns:    cfg.MaxConcurrentRuns,
		cloudURL:             cfg.CloudURL,
		cloudSecret:          cfg.CloudSecret,
		logsUploadEnabled:    cfg.LogsUploadEnabled,
		logsUploadInterval:   cfg.LogsUploadInterval,
		logsUploadBatchLines: batchLines,
		version:              cfg.Version,
		intervals: reloadableIntervals{
			metrics:    cfg.MetricsInterval,
			stream:     cfg.StreamInterval,
			containers: cfg.ContainersInterval,
			images:     cfg.ImagesInterval,
			processes:  cfg.ProcessesInterval,
			systemd:    cfg.SystemdInterval,
			runtimes:   cfg.RuntimesInterval,
			database:   cfg.DatabaseMetricsInterval,
			logs:       cfg.LogsInterval,
		},
	}
}

// SetConfigPath records the TOML file the daemon was started with. Hot
// reload and the config API need it; an empty path disables both.
func (a *App) SetConfigPath(path string) {
	a.configPath = path
}

// Reload re-reads the configuration file and fragments, validates the result,
// and swaps every reloadable component. A failed load or validation never
// takes the daemon down: the previous state stays active and the error is
// returned (and logged by callers that care). Safe to call concurrently;
// reloads are serialized.
func (a *App) Reload() error {
	if strings.TrimSpace(a.configPath) == "" {
		return fmt.Errorf("config reload requested but no config path is configured")
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		a.logger.Error("config reload failed to load", "error", err)
		return err
	}
	if err := (&config.Config{Daemon: cfg.Daemon}).ValidateDaemon(); err != nil {
		a.logger.Error("config reload failed validation", "error", err)
		return err
	}
	a.applyReload(cfg.Daemon)
	return nil
}

// applyReload swaps the reloadable components. Callers hold the reload
// serialization (either this method via Reload, or a PATCH that already
// wrote the file).
func (a *App) applyReload(cfg config.DaemonConfig) {
	a.executor.SetHooks(cfg.Webhooks, cfg.Actions)
	a.executor.SetScriptTimeout(cfg.ScriptTimeout)
	a.executor.SetMaxBodyBytes(cfg.MaxBodyBytes)
	a.executor.SetMaxConcurrentRuns(cfg.MaxConcurrentRuns)
	a.ops.SetScriptTimeout(cfg.ScriptTimeout)
	a.runtimes.SetRuntimes(cfg.Runtimes)
	a.runtimes.SetLimit(cfg.ProcessesLimit)
	a.watched.Reseed(cfg.WatchedProcesses)
	a.jobs.SetJobs(cfg.Jobs)
	if a.logAlerts != nil {
		a.logAlerts.SetAlerts(cfg.LogAlerts)
	}
	// one up when cloud configuration appeared on a host that had none.
	rt := newReloadableConfig(cfg)
	if pub := a.publish(); pub != nil {
		pub.Reload(cfg.CloudURL, cfg.CloudSecret)
	} else if strings.TrimSpace(cfg.CloudURL) != "" && strings.TrimSpace(cfg.CloudSecret) != "" {
		if created, err := NewCloudPublisher(cfg, a.logger); err == nil && created != nil {
			a.publisher.Store(created)
		}
	}
	a.rt.Store(rt)
	a.logger.Info("configuration reloaded",
		"actions", len(cfg.Actions), "alarms", len(cfg.Alarms), "jobs", len(cfg.Jobs),
		"runtimes", len(cfg.Runtimes), "cloud_url", cfg.CloudURL,
	)
}

// watchConfig watches the config file and the fragment directories and calls
// Reload on changes, debounced so an atomic save (temp file + rename) or a
// burst of writes coalesces into one reload. Returns after the watcher is
// torn down (on ctx cancellation or a watch error).
func (a *App) watchConfig(ctx context.Context) {
	dirs := make([]string, 0, 5)
	if p := filepath.Dir(a.configPath); p != "" {
		dirs = append(dirs, p)
	}
	for _, dir := range []string{a.cfg.ActionsDir, a.cfg.AlarmsDir, a.cfg.JobsDir, a.cfg.LogAlertsDir} {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		a.logger.Warn("config hot reload disabled", "error", err)
		return
	}
	defer watcher.Close()
	added := make(map[string]bool)
	for _, dir := range dirs {
		if added[dir] {
			continue
		}
		added[dir] = true
		if err := watcher.Add(dir); err != nil {
			// A missing fragment dir is normal on fresh hosts; the config
			// dir must be watchable for reload to work at all.
			a.logger.Debug("config watch add failed", "dir", dir, "error", err)
		}
	}
	var reloadMu sync.Mutex
	var pending bool
	var timer *time.Timer
	timer = time.AfterFunc(reloadDebounce, func() {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		if !pending {
			return
		}
		pending = false
		if err := a.Reload(); err != nil {
			a.logger.Warn("config hot reload failed; previous configuration stays active", "error", err)
		}
	})
	defer timer.Stop()
	schedule := func() {
		reloadMu.Lock()
		pending = true
		reloadMu.Unlock()
		timer.Reset(reloadDebounce)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Config and fragment edits land as writes; renames (atomic
			// saves) and chmods accompany them. Anything else is noise.
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				schedule()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			a.logger.Debug("config watch error", "error", err)
		}
	}
}

// reloadDebounce coalesces bursts of file writes into one reload.
const reloadDebounce = 400 * time.Millisecond
