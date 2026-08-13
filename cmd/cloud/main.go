package main

import (
	"context"
	"flag"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	"src.solsynth.dev/solsynth/maidcafe/internal/eventbus"
	"src.solsynth.dev/solsynth/maidcafe/internal/server"
)

func main() {
	configPath:=flag.String("config","", "configuration file path")
	flag.Parse()
	cfg,err:=config.Load(*configPath);if err!=nil{log.Fatal().Err(err).Msg("load config")};if err:=cfg.ValidateCloud();err!=nil{log.Fatal().Err(err).Msg("validate cloud config")}
	db,err:=database.Open(cfg);if err!=nil{log.Fatal().Err(err).Msg("open database")};defer db.Close();if err:=db.AutoMigrate();err!=nil{log.Fatal().Err(err).Msg("migrate database")}
	authenticator,err:=dyauth.NewGrpcTokenAuthenticator(dyauth.GrpcAuthDialConfig{Target:cfg.Auth.Target,UseTLS:cfg.Auth.UseTLS,TLSSkipVerify:cfg.Auth.TLSSkipVerify});if err!=nil{log.Fatal().Err(err).Msg("initialize auth")}
	bus,err:=eventbus.New(cfg.Eventbus.URL,cfg.App.Name);if err!=nil{log.Warn().Err(err).Msg("eventbus unavailable; continuing without push fan-out")};if bus!=nil{defer bus.Close()}
	svc:=cloud.NewService(db,bus);router:=server.NewRouter(cfg,svc,authenticator);httpServer:=&http.Server{Addr:":"+cfg.HTTP.Port,Handler:router,ReadHeaderTimeout:10*time.Second,ReadTimeout:30*time.Second,WriteTimeout:30*time.Second,IdleTimeout:60*time.Second}
	ctx,stop:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM);defer stop();go func(){if err:=httpServer.ListenAndServe();err!=nil&&err!=http.ErrServerClosed{log.Fatal().Err(err).Msg("cloud server")}}();<-ctx.Done();shutdownCtx,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel();_ = httpServer.Shutdown(shutdownCtx)
}
