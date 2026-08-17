package main

import (
	"context"
	"flag"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	"src.solsynth.dev/solsynth/maidcafe/internal/eventbus"
	"src.solsynth.dev/solsynth/maidcafe/internal/ring"
	"src.solsynth.dev/solsynth/maidcafe/internal/server"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
)

func main() {
	configPath := flag.String("config", "", "configuration file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}
	if err := cfg.ValidateCloud(); err != nil {
		log.Fatal().Err(err).Msg("validate cloud config")
	}

	db, err := database.Open(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("open database")
	}
	defer db.Close()
	if err := db.AutoMigrate(); err != nil {
		log.Fatal().Err(err).Msg("migrate database")
	}

	authenticator, err := dyauth.NewGrpcTokenAuthenticator(dyauth.GrpcAuthDialConfig{
		Target:        cfg.Auth.Target,
		UseTLS:        cfg.Auth.UseTLS,
		TLSSkipVerify: cfg.Auth.TLSSkipVerify,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("initialize auth")
	}
	accounts, accountConn, err := cloud.NewAccountClient(cfg.Auth)
	if err != nil {
		log.Fatal().Err(err).Msg("initialize account client")
	}
	defer accountConn.Close()

	workspaces, workspaceConn, err := cloud.NewWorkspaceClient(cfg.Workspace)
	if err != nil {
		log.Fatal().Err(err).Msg("initialize workspace client")
	}
	defer workspaceConn.Close()

	var publishers cloud.FanoutPublisher
	bus, err := eventbus.New(cfg.Eventbus.URL, cfg.App.Name, cfg.Eventbus.SubjectPrefix)
	if err != nil {
		log.Warn().Err(err).Msg("eventbus unavailable; continuing without NATS fan-out")
	} else if bus != nil {
		publishers = append(publishers, bus)
		defer bus.Close()
	}
	if strings.TrimSpace(cfg.Ring.Target) != "" {
		ringClient, err := ring.NewClient(
			cfg.Ring.Target,
			cfg.Ring.UseTLS,
			cfg.Ring.TLSSkipVerify,
		)
		if err != nil {
			log.Fatal().Err(err).Msg("initialize Metoer push client")
		}
		publishers = append(publishers, ringClient)
		defer ringClient.Close()
	}

	var publisher cloud.PushPublisher
	if len(publishers) > 0 {
		publisher = publishers
	}
	svc := cloud.NewService(db, publisher, workspaces)
	svc.SetAccountClient(accounts)
	router := server.NewRouter(cfg, svc, authenticator)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Metric retention: prune rows older than each workspace's
	// metrics_retention_days quota, once at startup then hourly.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		prune := func() {
			if err := svc.PruneMetrics(ctx); err != nil {
				log.Warn().Err(err).Msg("metric retention prune")
			}
		}
		prune()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
	disconnectAfter := cfg.Cloud.DaemonDisconnectAfter
	if disconnectAfter <= 0 {
		disconnectAfter = cloud.DefaultDaemonDisconnectAfter
	}
	disconnectCooldown := cfg.Cloud.DaemonDisconnectNotificationCooldown
	if disconnectCooldown <= 0 {
		disconnectCooldown = cloud.DefaultDaemonDisconnectNotificationCooldown
	}
	alarmCheckInterval := cfg.Cloud.AlarmCheckInterval
	if alarmCheckInterval <= 0 {
		alarmCheckInterval = cloud.DefaultAlarmCheckInterval
	}
	go func() {
		ticker := time.NewTicker(alarmCheckInterval)
		defer ticker.Stop()
		check := func() {
			if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, disconnectAfter, disconnectCooldown, time.Now().UTC()); err != nil {
				log.Warn().Err(err).Msg("daemon disconnect alarm evaluation")
			}
		}
		check()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				checkAt := now.UTC()
				if err := svc.EvaluateDisconnectedDaemonsWithCooldown(ctx, disconnectAfter, disconnectCooldown, checkAt); err != nil {
					log.Warn().Err(err).Msg("daemon disconnect alarm evaluation")
				}
			}
		}
	}()

	httpServer := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("cloud server")
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
