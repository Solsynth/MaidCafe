package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type App struct {
	cfg       config.DaemonConfig
	executor  *WebhookExecutor
	metrics   *MetricsCollector
	publisher *CloudPublisher
	server    *http.Server
	listener  net.Listener
	logger    *slog.Logger
}

func NewApp(cfg config.DaemonConfig, logger *slog.Logger) (*App, error) {
	if err := (&config.Config{Daemon: cfg}).ValidateDaemon(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	executor := NewWebhookExecutor(cfg)
	publisher, err := NewCloudPublisher(cfg, logger)
	if err != nil {
		return nil, err
	}
	metrics, err := NewMetricsCollector(cfg, executor)
	if err != nil {
		return nil, fmt.Errorf("open metrics history: %w", err)
	}
	app := &App{cfg: cfg, executor: executor, metrics: metrics, publisher: publisher, logger: logger}
	executor.SetCompletionHandler(func(hook config.WebhookConfig, ok bool, exitCode int, stderr string, duration time.Duration) {
		if publisher == nil || (!ok && !hook.NotifyOnFailure) || (ok && !hook.NotifyOnSuccess) {
			return
		}
		kind, title := "webhook.failure", "Webhook "+hook.Name+" failed"
		if ok {
			kind, title = "webhook.success", "Webhook "+hook.Name+" completed"
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
	router.POST("/api/v1/actions/:name", authorizeMetrics, func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, cfg.MaxBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		response, requestErr := executor.RunAction(
			c.Request.Context(),
			c.Param("name"),
			body,
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
	router.POST("/api/v1/webhooks/:name", executor.GinHandler())
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
	a.listener = listener
	a.server.Addr = listener.Addr().String()
	go func() {
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("daemon server stopped", "error", err)
		}
	}()
	return nil
}

func (a *App) ListenAddr() string {
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
