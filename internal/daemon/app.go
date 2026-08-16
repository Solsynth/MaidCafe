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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type App struct {
	cfg        config.DaemonConfig
	executor   *WebhookExecutor
	metrics    *MetricsCollector
	publisher  *CloudPublisher
	relay      *WebhookRelay
	hub        *StreamHub
	alarms     *alarmEvaluator
	containers *ContainersCollector
	images     *ImagesCollector
	processes  *ProcessesCollector
	systemd    *SystemdCollector
	runtimes   *RuntimesCollector
	server     *http.Server
	listenerMu sync.RWMutex
	listener   net.Listener
	logger     *slog.Logger
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
	publisher, err := NewCloudPublisher(cfg, logger)
	if err != nil {
		return nil, err
	}
	metrics, err := NewMetricsCollector(cfg, executor)
	if err != nil {
		return nil, fmt.Errorf("open metrics history: %w", err)
	}
	runtimeProbe := &runtimeProbeState{}
	app := &App{
		cfg:        cfg,
		executor:   executor,
		metrics:    metrics,
		publisher:  publisher,
		hub:        NewStreamHub(),
		alarms:     newAlarmEvaluator(),
		containers: &ContainersCollector{probe: runtimeProbe},
		images:     &ImagesCollector{probe: runtimeProbe},
		processes:  &ProcessesCollector{limit: cfg.ProcessesLimit},
		systemd:    &SystemdCollector{},
		runtimes:   &RuntimesCollector{limit: cfg.ProcessesLimit},
		logger:     logger,
	}
	app.relay = NewWebhookRelay(publisher, executor, logger)
	executor.SetCompletionHandler(func(hook config.WebhookConfig, ok bool, exitCode int, stderr string, duration time.Duration) {
		if publisher == nil || (!ok && !hook.NotifyOnFailure) || (ok && !hook.NotifyOnSuccess) {
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
		publisher.PublishNotification(context.Background(), notificationPayload{Kind: kind, Title: title, Body: body, Metadata: map[string]any{"name": hook.Name, "exit_code": exitCode, "duration_ms": duration.Milliseconds()}})
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
		handleStream(c, app.hub, cfg)
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
		data, err := app.processes.collect(c.Request.Context())
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
	router.POST("/api/v1/actions/:name", authorizeMetrics, func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, cfg.MaxBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if int64(len(body)) > cfg.MaxBodyBytes {
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
		if app.publisher == nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "cloud relay is not configured"})
			return
		}
		app.publisher.PublishNotification(context.Background(), notificationPayload{
			Kind:  "test.notification",
			Title: "Test notification",
			Body:  "This is a test notification from the MaidCafe daemon.",
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	app.server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           router,
		ReadHeaderTimeout: cfg.RequestTimeout,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout,
		IdleTimeout:       cfg.RequestTimeout,
	}
	return app, nil
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
	ticker := time.NewTicker(a.cfg.MetricsInterval)
	defer ticker.Stop()
	a.metrics.Record()
	if a.relay != nil {
		go a.relay.Run(ctx)
	}
	a.startStreamCollectors(ctx)
	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.Shutdown(shutdownCtx)
	}
	for {
		select {
		case <-ctx.Done():
			return shutdown()
		case <-ticker.C:
			metrics := a.metrics.Record()
			if a.publisher != nil {
				a.publisher.PublishMetrics(context.Background(), metrics)
				a.publisher.PublishActions(context.Background(), a.cfg.Actions)
				now := time.Now()
				for _, notification := range a.alarms.evaluate(a.cfg.Alarms, metrics, now) {
					a.publisher.PublishNotification(context.Background(), notification)
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
// Collect (no persistence), never Record.
func (a *App) startStreamCollectors(ctx context.Context) {
	a.runStreamCollector(ctx, "metric", a.cfg.StreamInterval, func(ctx context.Context) ([]byte, error) {
		return json.Marshal(a.metrics.Collect())
	})
	a.runStreamCollector(ctx, "containers", a.cfg.ContainersInterval, a.containers.collect)
	a.runStreamCollector(ctx, "images", a.cfg.ImagesInterval, a.images.collect)
	a.runStreamCollector(ctx, "processes", a.cfg.ProcessesInterval, a.processes.collect)
	a.runStreamCollector(ctx, "systemd", a.cfg.SystemdInterval, a.systemd.collect)
	a.runStreamCollector(ctx, "runtimes", a.cfg.RuntimesInterval, a.runtimes.collect)
}

func (a *App) runStreamCollector(ctx context.Context, event string, interval time.Duration, collect func(context.Context) ([]byte, error)) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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
