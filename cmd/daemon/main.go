package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/daemon"
	"syscall"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "notify" {
		notifyCmd(os.Args[2:])
		return
	}
	configPath := flag.String("config", "", "configuration file path")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.ValidateDaemon(); err != nil {
		slog.Error("validate daemon config", "error", err)
		os.Exit(1)
	}
	app, err := daemon.NewApp(cfg.Daemon, slog.Default())
	if err != nil {
		slog.Error("create daemon", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		slog.Error("daemon stopped", "error", err)
		os.Exit(1)
	}
}
