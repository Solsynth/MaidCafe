package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type App struct {
	cfg             config.DaemonConfig
	executor        *WebhookExecutor
	ops             *nativeOpRunner
	metrics         *MetricsCollector
	publisher       *atomic.Pointer[CloudPublisher]
	relay           *WebhookRelay
	hub             *StreamHub
	alarms          *alarmEvaluator
	containers      *ContainersCollector
	images          *ImagesCollector
	processes       *ProcessesCollector
	systemd         *SystemdCollector
	runtimes        *RuntimesCollector
	databaseMetrics *DatabaseMetricsCollector
	logs            *LogsCollector
	logAlerts       *logAlertEvaluator
	logUpload       *logUploadBuffer
	watched         *watchedProcessStore
	jobs            *jobRunner
	server          *http.Server
	listenerMu      sync.RWMutex
	listener        net.Listener
	logger          *slog.Logger
	// configPath is the TOML file the daemon was started with; empty means
	// environment-only configuration (hot reload and the config API are
	// disabled then).
	configPath string
	// rt holds the reloadable configuration slice; readers load it once per
	// request or tick, so a swap never tears a run in half.
	rt atomic.Pointer[reloadableConfig]
}

// publish returns the current cloud publisher, or nil when cloud publishing
// is not configured.
func (a *App) publish() *CloudPublisher {
	if a.publisher == nil {
		return nil
	}
	return a.publisher.Load()
}

func NewApp(cfg config.DaemonConfig, logger *slog.Logger) (*App, error) {
	if err := (&config.Config{Daemon: cfg}).ValidateDaemon(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	executor := NewWebhookExecutor(cfg)
	executor.SetAuditLogger(NewAuditLogger(cfg.AuditPath, logger))
	publisherBox := &atomic.Pointer[CloudPublisher]{}
	publisher, err := NewCloudPublisher(cfg, logger)
	if err != nil {
		return nil, err
	}
	if publisher != nil {
		publisherBox.Store(publisher)
	}
	metrics, err := NewMetricsCollector(cfg, executor)
	if err != nil {
		return nil, fmt.Errorf("open metrics history: %w", err)
	}
	runtimeProbe := &runtimeProbeState{}
	processTable := &processTableCache{}
	ops := &nativeOpRunner{
		executor: executor,
		runtimes: probeContainerRuntimes,
	}
	ops.SetScriptTimeout(cfg.ScriptTimeout)
	watchedStore := newWatchedProcessStore(cfg.WatchedProcessesFile, cfg.WatchedProcesses)
	historyDir := ""
	if strings.TrimSpace(cfg.MetricsHistoryPath) != "" {
		historyDir = filepath.Join(cfg.MetricsHistoryPath, "process-history")
	}
	historyStore := newProcessHistoryStore(historyDir, cfg.MetricsRetentionDays)
	logStore := newContainerLogStore(cfg.LogsDir, cfg.MetricsRetentionDays)
	jobs := newJobRunner(executor, ops, publisherBox, logger)
	app := &App{
		cfg:        cfg,
		executor:   executor,
		ops:        ops,
		metrics:    metrics,
		publisher:  publisherBox,
		hub:        NewStreamHub(),
		alarms:     newAlarmEvaluator(),
		containers: &ContainersCollector{probe: runtimeProbe},
		images:     &ImagesCollector{probe: runtimeProbe},
		processes:  &ProcessesCollector{limit: cfg.ProcessesLimit, table: processTable},
		systemd:    &SystemdCollector{},
		runtimes: &RuntimesCollector{
			limit: cfg.ProcessesLimit, runtimes: cfg.Runtimes,
			watched: watchedStore, history: historyStore, table: processTable,
		},
		databaseMetrics: &DatabaseMetricsCollector{},
		logs:            &LogsCollector{probe: runtimeProbe, store: logStore, logger: logger},
		logAlerts:       newLogAlertEvaluator(),
		logUpload:       newLogUploadBuffer(),
		watched:         watchedStore,
		jobs:            jobs,
		logger:          logger,
	}
	app.logAlerts.SetAlerts(cfg.LogAlerts)
	app.rt.Store(newReloadableConfig(cfg))
	app.relay = NewWebhookRelay(publisherBox, executor, ops, logger)
	executor.SetCompletionHandler(func(hook config.WebhookConfig, ok bool, exitCode int, stderr string, duration time.Duration) {
		p := publisherBox.Load()
		if p == nil || (!ok && !hook.NotifyOnFailure) || (ok && !hook.NotifyOnSuccess) {
			return
		}
		kind, title := "webhook.failure", "Webhook "+hook.Label()+" failed"
		if ok {
			kind, title = "webhook.success", "Webhook "+hook.Label()+" completed"
		}
		body := strings.TrimSpace(stderr)
		if len(body) > 4096 {
			body = body[:4096]
		}
		p.PublishNotification(context.Background(), notificationPayload{Kind: kind, Title: title, Body: body, Metadata: map[string]any{"name": hook.Name, "display_name": hook.Label(), "exit_code": exitCode, "duration_ms": duration.Milliseconds()}})
	})

	if strings.EqualFold(strings.TrimSpace(cfg.Transport), "stdio") {
		app.server = nil
		return app, nil
	}
	router := gin.New()
	router.Use(gin.Recovery())
	// Log every control-plane request so connection problems are visible in
	// journald: 401 distinguishes an auth/secret mismatch from requests that
	// never reach the daemon (missing, or refused by the network).
	router.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.Info("http request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
	authorizeMetrics := func(c *gin.Context) {
		if !authorizedRequest(c.Request, cfg.MetricsSecret) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
			return
		}
		c.Next()
	}
	router.GET("/health", authorizeMetrics, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"mode":    "daemon",
			"id":      cfg.ID,
			"version": cfg.Version,
		})
	})
	router.GET("/api/v1/metrics", authorizeMetrics, func(c *gin.Context) {
		c.JSON(http.StatusOK, app.metrics.Collect())
	})
	router.GET("/api/v1/stream", authorizeMetrics, func(c *gin.Context) {
		handleStream(c, app.hub, app.rt.Load())
	})
	router.GET("/api/v1/metrics/history", authorizeMetrics, func(c *gin.Context) {
		parseTime := func(name string) (*time.Time, error) {
			raw := strings.TrimSpace(c.Query(name))
			if raw == "" {
				return nil, nil
			}
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return nil, fmt.Errorf("%s must be RFC3339", name)
			}
			return &value, nil
		}
		from, err := parseTime("from")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		to, err := parseTime("to")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		before, err := parseTime("before")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		limit := 100
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "limit must be a positive integer"})
				return
			}
			limit = parsed
		}
		c.JSON(http.StatusOK, gin.H{"metrics": app.metrics.History(MetricsHistoryQuery{
			From: from, To: to, Before: before, Limit: limit,
		})})
	})
	// One-shot snapshots of the same data the SSE stream pushes, so clients
	// can paint first data from the daemon instead of an SSH fallback. They
	// reuse the stream collectors' probe cache and rate limits.
	router.GET("/api/v1/containers", authorizeMetrics, func(c *gin.Context) {
		data, err := app.containers.snapshot(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	router.GET("/api/v1/images", authorizeMetrics, func(c *gin.Context) {
		data, err := app.images.snapshot(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	router.GET("/api/v1/processes", authorizeMetrics, func(c *gin.Context) {
		// Per-request cap: absent keeps the daemon's configured
		// processesLimit; 0 requests the complete process table; a positive
		// value keeps the top N CPU consumers.
		limit, err := parseProcessesLimit(c.Query("limit"), cfg.ProcessesLimit)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		entries, err := app.processes.collectEntries(c.Request.Context(), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		data, err := json.Marshal(processesPayload{Processes: entries})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	router.GET("/api/v1/systemd", authorizeMetrics, func(c *gin.Context) {
		data, err := app.systemd.snapshot(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	router.GET("/api/v1/runtimes", authorizeMetrics, func(c *gin.Context) {
		data, err := app.runtimes.collect(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	// One-shot database health snapshot (same payload as the
	// `databaseMetrics` SSE event).
	router.GET("/api/v1/database-metrics", authorizeMetrics, func(c *gin.Context) {
		data, err := app.databaseMetrics.collect(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
	// Watched-process management: the list is stored daemon-side (seeded from
	// config, persisted across restarts) and reported in every runtimes
	// payload under `watched`.
	router.GET("/api/v1/watched-processes", authorizeMetrics, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"processes": app.watched.List()})
	})
	router.POST("/api/v1/watched-processes", authorizeMetrics, func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, app.rt.Load().maxBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if int64(len(body)) > app.rt.Load().maxBodyBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"ok": false, "error": "request body too large"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid JSON body"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if !config.ValidWatchedProcessName(name) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "name must match [A-Za-z0-9][A-Za-z0-9._-]*"})
			return
		}
		processes, addErr := app.watched.Add(name)
		if addErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": addErr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"processes": processes})
	})
	router.DELETE("/api/v1/watched-processes/:name", authorizeMetrics, func(c *gin.Context) {
		processes, removeErr := app.watched.Remove(c.Param("name"))
		if removeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": removeErr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"processes": processes})
	})
	// Per-watched-process usage history recorded by the daemon's ungated
	// history ticker, pruned by metricsRetentionDays.
	router.GET("/api/v1/process-history", authorizeMetrics, func(c *gin.Context) {
		name := strings.TrimSpace(c.Query("name"))
		if !config.ValidWatchedProcessName(name) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "name must match [A-Za-z0-9][A-Za-z0-9._-]*"})
			return
		}
		parseTime := func(raw string) (*time.Time, error) {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				return nil, nil
			}
			value, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Errorf("must be RFC3339")
			}
			return &value, nil
		}
		from, err := parseTime(c.Query("from"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "from " + err.Error()})
			return
		}
		to, err := parseTime(c.Query("to"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "to " + err.Error()})
			return
		}
		limit := 500
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "limit must be a positive integer"})
				return
			}
			limit = parsed
		}
		samples := app.runtimes.history.Query(name, from, to, limit)
		c.JSON(http.StatusOK, gin.H{"name": name, "samples": samples})
	})
	router.POST("/api/v1/actions/:name", authorizeMetrics, func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, app.rt.Load().maxBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if int64(len(body)) > app.rt.Load().maxBodyBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"ok": false, "error": "request body too large"})
			return
		}
		// The Bearer check above authenticates the caller; the signature binds
		// the request to this exact body so it cannot be tampered in transit.
		if !signatureValid(cfg.MetricsSecret, body, c.GetHeader("X-MaidCafe-Signature")) {
			c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
			return
		}
		response, requestErr := executor.RunAction(
			c.Request.Context(),
			c.Param("name"),
			body,
			"http",
			c.GetHeader("X-MaidCafe-Invoked-By"),
		)
		if requestErr != nil {
			c.JSON(
				requestErr.status,
				gin.H{"ok": false, "error": requestErr.message},
			)
			return
		}
		c.JSON(http.StatusOK, response)
	})
	// Native operations: typed container/systemd/compose/process mutations
	// with the same authentication as actions (metrics secret + body
	// signature). Targets are validated daemon-side; runtimes resolve through
	// the shared probe with sudo -n retry, so no shell or scripts are
	// involved.
	router.POST("/api/v1/containers/:id/:action", authorizeMetrics, app.nativeOpHandler(ops, func(c *gin.Context) (string, opParams) {
		return "container." + c.Param("action"), opParams{target: c.Param("id")}
	}, func(values map[string]any, p *opParams) {
		if force, ok := values["force"].(bool); ok {
			p.force = force
		}
	}))
	router.POST("/api/v1/processes/:pid/kill", authorizeMetrics, app.nativeOpHandler(ops, func(c *gin.Context) (string, opParams) {
		pid, _ := strconv.Atoi(c.Param("pid"))
		return "process.kill", opParams{pid: pid}
	}, nil))
	router.POST("/api/v1/systemd/:unit/:action", authorizeMetrics, app.nativeOpHandler(ops, func(c *gin.Context) (string, opParams) {
		return "systemd." + c.Param("action"), opParams{target: c.Param("unit")}
	}, nil))
	router.POST("/api/v1/compose/:project/:action", authorizeMetrics, app.nativeOpHandler(ops, func(c *gin.Context) (string, opParams) {
		return "compose." + c.Param("action"), opParams{target: c.Param("project")}
	}, func(values map[string]any, p *opParams) {
		if directory, ok := values["directory"].(string); ok {
			p.directory = directory
		}
	}))
	// Config introspection and safe-subset patching: the daemon edits its own
	// config.toml (preserving everything it does not model) and hot-reloads,
	// so interval/limit/cloud changes apply without a restart.
	router.GET("/api/v1/config", authorizeMetrics, app.handleGetConfig)
	router.PATCH("/api/v1/config", authorizeMetrics, app.handlePatchConfig)
	// Captured container logs (disk-backed, pruned by retention): the tail
	// window for one container, oldest first.
	router.GET("/api/v1/containers/:id/logs", authorizeMetrics, func(c *gin.Context) {
		lines := 200
		if raw := strings.TrimSpace(c.Query("lines")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed < 1 || parsed > containerLogRingLines {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": fmt.Sprintf("lines must be between 1 and %d", containerLogRingLines)})
				return
			}
			lines = parsed
		}
		c.JSON(http.StatusOK, gin.H{"container": c.Param("id"), "lines": app.logs.store.Snapshot(c.Param("id"), lines)})
	})
	router.GET("/api/v1/audit", authorizeMetrics, func(c *gin.Context) {
		limit := 50
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "limit must be a positive integer"})
				return
			}
			limit = parsed
		}
		c.JSON(http.StatusOK, gin.H{"entries": executor.audit.Recent(limit)})
	})
	router.DELETE("/api/v1/audit", authorizeMetrics, func(c *gin.Context) {
		if err := executor.audit.Clear(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.POST("/api/v1/webhooks/:name", executor.GinHandler())
	// Manual smoke test of the notification pipeline: the daemon pushes a
	// test notification to the cloud (which forwards it to Ring/Metoer), so
	// the whole daemon -> cloud -> feed path can be verified on demand.
	router.POST("/api/v1/notifications/test", authorizeMetrics, func(c *gin.Context) {
		pub := app.publish()
		if pub == nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "cloud relay is not configured"})
			return
		}
		pub.PublishNotification(context.Background(), notificationPayload{
			Kind:  "test.notification",
			Title: "Test notification",
			Body:  "This is a test notification from the MaidCafe daemon.",
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// Custom notification: any local tool (the `notify` CLI included) can
	// send a fully customized title/subtitle/body payload through the daemon
	// to the cloud and on to the user's Metoer feed.
	router.POST("/api/v1/notifications", authorizeMetrics, func(c *gin.Context) {
		if app.publish() == nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "cloud relay is not configured"})
			return
		}
		var in struct {
			Kind     string         `json:"kind"`
			Title    string         `json:"title"`
			Subtitle string         `json:"subtitle"`
			Body     string         `json:"body"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid JSON body"})
			return
		}
		kind := strings.TrimSpace(in.Kind)
		if kind == "" {
			kind = "daemon.notification"
		}
		title := strings.TrimSpace(in.Title)
		body := strings.TrimSpace(in.Body)
		if title == "" || body == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "title and body are required"})
			return
		}
		if len(body) > 4096 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "body must be at most 4096 bytes"})
			return
		}
		app.publish().PublishNotification(context.Background(), notificationPayload{
			Kind:     kind,
			Title:    title,
			Subtitle: strings.TrimSpace(in.Subtitle),
			Body:     body,
			Metadata: in.Metadata,
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	app.server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           router,
		ReadHeaderTimeout: cfg.RequestTimeout,
		ReadTimeout:       cfg.RequestTimeout,
		// No WriteTimeout: responses are small JSON written after the handler
		// finishes, and executions are bounded by the executor's per-run
		// timeout. A write deadline equal to RequestTimeout would kill every
		// execution longer than that — configured actions, and native ops
		// like compose pull — on the loopback HTTP path.
		IdleTimeout: cfg.RequestTimeout,
	}
	return app, nil
}

// nativeOpHandler serves one native operation route. [identity] builds the
// operation slug and path-derived params from the request; [decorate] merges
// JSON body fields (e.g. `force` for container remove, `directory` for
// compose) into the params. Authentication mirrors the actions route: the
// metrics secret bearer plus a body signature.
func (a *App) nativeOpHandler(
	ops *nativeOpRunner,
	identity func(c *gin.Context) (string, opParams),
	decorate func(values map[string]any, p *opParams),
) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, a.rt.Load().maxBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if int64(len(body)) > a.rt.Load().maxBodyBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"ok": false, "error": "request body too large"})
			return
		}
		if !signatureValid(a.cfg.MetricsSecret, body, c.GetHeader("X-MaidCafe-Signature")) {
			c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
			return
		}
		slug, params := identity(c)
		if decorate != nil && len(body) > 0 {
			var values map[string]any
			if err := json.Unmarshal(body, &values); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid JSON body"})
				return
			}
			decorate(values, &params)
		}
		response, status, requestErr := ops.dispatch(
			c.Request.Context(),
			slug,
			params,
			"http",
			c.GetHeader("X-MaidCafe-Invoked-By"),
		)
		if requestErr != nil {
			c.JSON(requestErr.status, gin.H{"ok": false, "error": requestErr.message})
			return
		}
		c.JSON(status, response)
	}
}

func (a *App) Start() error {
	listener, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen daemon: %w", err)
	}
	a.listenerMu.Lock()
	a.listener = listener
	a.listenerMu.Unlock()
	a.server.Addr = listener.Addr().String()
	go func() {
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("daemon server stopped", "error", err)
		}
	}()
	return nil
}

func (a *App) ListenAddr() string {
	a.listenerMu.RLock()
	defer a.listenerMu.RUnlock()
	if a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

func (a *App) Run(ctx context.Context) error {
	if strings.EqualFold(strings.TrimSpace(a.cfg.Transport), "stdio") {
		return a.runStdio(ctx)
	}
	if err := a.Start(); err != nil {
		return err
	}
	a.metrics.Record()
	if a.relay != nil {
		go a.relay.Run(ctx)
	}
	a.startStreamCollectors(ctx)
	go a.jobs.Run(ctx)
	go a.watchConfig(ctx)
	// The metrics cadence is reloadable, so a fixed ticker cannot capture it;
	// a 1s base tick checks the current interval instead.
	base := time.NewTicker(time.Second)
	defer base.Stop()
	var lastMetrics time.Time
	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.Shutdown(shutdownCtx)
	}
	for {
		select {
		case <-ctx.Done():
			return shutdown()
		case now := <-base.C:
			interval := a.rt.Load().intervals.metrics
			if interval <= 0 || now.Sub(lastMetrics) < interval {
				continue
			}
			lastMetrics = now
			metrics := a.metrics.Record()
			rt := a.rt.Load()
			if pub := a.publish(); pub != nil {
				pub.PublishMetrics(context.Background(), metrics)
				pub.PublishActions(context.Background(), append(nativeOpReport(), rt.actions...))
				for _, notification := range a.alarms.evaluate(rt.alarms, metrics, now) {
					pub.PublishNotification(context.Background(), notification)
				}
				for _, alarm := range rt.alarms {
					if alarm.Kind != "container_down" || alarm.Enabled != nil && !*alarm.Enabled {
						continue
					}
					data, err := a.containers.snapshot(context.Background())
					if err != nil {
						a.logger.Warn("container alarm evaluation failed", "error", err)
						break
					}
					var sample containersPayload
					if err := json.Unmarshal(data, &sample); err != nil {
						a.logger.Warn("container alarm payload invalid", "error", err)
						break
					}
					for _, notification := range a.alarms.evaluateContainers(rt.alarms, sample, now) {
						pub.PublishNotification(context.Background(), notification)
					}
					break
				}
			}
		}
	}
}

func (a *App) Shutdown(ctx context.Context) error {
	if a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

// startStreamCollectors launches one ticker goroutine per enabled stream event
// type (HTTP transport only). Each collector runs only while at least one SSE
// subscriber wants its type; the metric collector reuses MetricsCollector.
// Collect (no persistence), never Record. Intervals are read from the
// reloadable state on every tick, so a config reload applies immediately.
func (a *App) startStreamCollectors(ctx context.Context) {
	a.runStreamCollector(ctx, "metric", func(rt *reloadableConfig) time.Duration { return rt.intervals.stream }, func(ctx context.Context) ([]byte, error) {
		return json.Marshal(a.metrics.Collect())
	})
	a.runStreamCollector(ctx, "containers", func(rt *reloadableConfig) time.Duration { return rt.intervals.containers }, a.containers.collect)
	a.runStreamCollector(ctx, "images", func(rt *reloadableConfig) time.Duration { return rt.intervals.images }, a.images.collect)
	a.runProcessesStreamCollector(ctx, func(rt *reloadableConfig) time.Duration { return rt.intervals.processes })
	a.runStreamCollector(ctx, "systemd", func(rt *reloadableConfig) time.Duration { return rt.intervals.systemd }, a.systemd.collect)
	a.runStreamCollector(ctx, "runtimes", func(rt *reloadableConfig) time.Duration { return rt.intervals.runtimes }, a.runtimes.collect)
	a.runStreamCollector(ctx, "databaseMetrics", func(rt *reloadableConfig) time.Duration { return rt.intervals.database }, a.databaseMetrics.collect)
	// Process history accumulates regardless of SSE subscribers so charts
	// show usage even while no client is connected.
	a.runHistoryCollector(ctx, func(rt *reloadableConfig) time.Duration { return rt.intervals.runtimes }, a.runtimes.recordHistory)
	a.runLogsCollector(ctx)
}

// runHistoryCollector ticks a recording callback at interval without the
// subscriber gate stream collectors use. Disabled when interval <= 0.
func (a *App) runHistoryCollector(ctx context.Context, interval func(*reloadableConfig) time.Duration, record func(context.Context)) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rt := a.rt.Load()
				if rt == nil {
					continue
				}
				i := interval(rt)
				if i <= 0 || now.Sub(last) < i {
					continue
				}
				last = now
				record(ctx)
			}
		}
	}()
}

func (a *App) runStreamCollector(ctx context.Context, event string, interval func(*reloadableConfig) time.Duration, collect func(context.Context) ([]byte, error)) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rt := a.rt.Load()
				if rt == nil {
					continue
				}
				i := interval(rt)
				if i <= 0 || now.Sub(last) < i {
					continue
				}
				last = now
				// Per-type gating: zero subscribers means zero collection work.
				if a.hub.Subscribers(event) == 0 {
					continue
				}
				data, err := collect(ctx)
				if err != nil {
					a.logger.Debug("stream collector failed", "event", event, "error", err)
					continue
				}
				if len(data) > 0 {
					a.hub.Broadcast(event, data)
				}
			}
		}
	}()
}

// runProcessesStreamCollector ticks the processes collector on the stream
// cadence while at least one SSE subscriber wants `processes`. The complete
// process table is collected once and sliced per subscriber inside
// StreamHub.BroadcastProcesses, so subscribers with different caps share a
// single ps run.
func (a *App) runProcessesStreamCollector(ctx context.Context, interval func(*reloadableConfig) time.Duration) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rt := a.rt.Load()
				if rt == nil {
					continue
				}
				i := interval(rt)
				if i <= 0 || now.Sub(last) < i {
					continue
				}
				last = now
				if a.hub.Subscribers("processes") == 0 {
					continue
				}
				entries, err := a.processes.collectEntries(ctx, 0)
				if err != nil {
					a.logger.Debug("stream collector failed", "event", "processes", "error", err)
					continue
				}
				a.hub.BroadcastProcesses(entries)
			}
		}
	}()
}

// runLogsCollector captures container logs on the logs cadence regardless of
// subscribers (the disk store is the durable record), broadcasting the delta
// to `logs` subscribers when any are connected. Disabled when logsInterval
// is 0.
func (a *App) runLogsCollector(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rt := a.rt.Load()
				if rt == nil {
					continue
				}
				interval := rt.intervals.logs
				if interval <= 0 || now.Sub(last) < interval {
					continue
				}
				last = now
				data, err := a.containers.snapshot(ctx)
				if err != nil {
					a.logger.Debug("logs collector: container snapshot failed", "error", err)
					continue
				}
				var payload containersPayload
				if err := json.Unmarshal(data, &payload); err != nil {
					continue
				}
				runtimes := make(map[string][]containerEntry, len(payload.Runtimes))
				for _, runtime := range payload.Runtimes {
					runtimes[runtime.Runtime] = runtime.Containers
				}
				collected, err := a.logs.collect(ctx, runtimes)
				if err != nil {
					a.logger.Debug("logs collector failed", "error", err)
					continue
				}
				if len(collected) > 0 {
					pub := a.publish()
					for id, lines := range collected {
						for _, line := range lines {
							for _, match := range a.logAlerts.Match(id, line) {
								if pub == nil {
									continue
								}
								body := strings.TrimSpace(match.Line.Line)
								if len(body) > 4096 {
									body = body[:4096]
								}
								pub.PublishNotification(ctx, notificationPayload{
									Kind:  "daemon.log_alert",
									Title: match.Title,
									Body:  body,
									Metadata: map[string]any{
										"alert": match.Rule, "container": match.Container,
										"timestamp": match.Line.TS,
									},
								})
							}
						}
						a.logUpload.Add(id, lines)
						if a.hub.Subscribers("logs") == 0 {
							continue
						}
						frame, marshalErr := json.Marshal(logsFramePayload{Container: id, Lines: lines})
						if marshalErr == nil {
							a.hub.Broadcast("logs", frame)
						}
					}
				}
				a.logUpload.Flush(ctx, a.publish(), rt.logsUploadEnabled, rt.logsUploadInterval, rt.logsUploadBatchLines)
			}
		}
	}()
}
