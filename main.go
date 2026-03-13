package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	log "github.com/yuchanns/bugo/internal/logging"
)

func main() {
	log.Configure()

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("config.load.failed")
	}

	app, err := NewApp(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("app.build.failed")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("app.run.failed")
	}
}
