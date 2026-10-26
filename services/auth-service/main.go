package main

import (
	"log"
	"log/slog"

	"github.com/fikohc/memelo/auth-service/app"
	"github.com/fikohc/memelo/auth-service/conf"
	"github.com/fikohc/memelo/common/config"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func main() {
	config.InitConfig()
	cfg, err := conf.NewConfig()
	if err != nil {
		log.Fatal(err)
	}
	config.InitLogs(cfg.Log)

	fx.New(
		fx.WithLogger(func() fxevent.Logger {
			return &fxevent.SlogLogger{Logger: slog.With()}
		}),
		fx.Supply(cfg),
		app.Module(),
	).Run()
}
