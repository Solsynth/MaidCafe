package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	app := &App{cfg: cfg, executor: executor, metrics: NewMetricsCollector(cfg, executor), publisher: publisher, logger: logger}
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
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "mode": "daemon", "id": cfg.ID})
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
	var ticker *time.Ticker
	if a.publisher != nil {
		ticker = time.NewTicker(a.cfg.MetricsInterval)
		defer ticker.Stop()
	}
	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.Shutdown(shutdownCtx)
	}
	for {
		if ticker == nil {
			<-ctx.Done()
			return shutdown()
		}
		select {
		case <-ctx.Done():
			return shutdown()
		case <-ticker.C:
			a.publisher.PublishMetrics(context.Background(), a.metrics.Collect())
		}
	}
}

func (a *App) Shutdown(ctx context.Context) error {
	if a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}
