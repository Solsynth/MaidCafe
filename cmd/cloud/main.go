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

	workspaces, workspaceConn, err := cloud.NewWorkspaceClient(cfg.Workspace)
	if err != nil {
		log.Fatal().Err(err).Msg("initialize workspace client")
	}
	defer workspaceConn.Close()

	var publishers cloud.FanoutPublisher
	bus, err := eventbus.New(cfg.Eventbus.URL, cfg.App.Name)
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
	router := server.NewRouter(cfg, svc, authenticator)
	httpServer := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
