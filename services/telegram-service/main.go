package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"

	"github.com/fikohc/memelo/common/config"
	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/service"
	tgstorage "github.com/fikohc/memelo/telegram-service/storage"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func Startup(lc fx.Lifecycle, cfg *conf.Config, svc service.TelegramBotService) {
	var srv *http.Server
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := svc.RegisterWebhook(); err != nil {
				return err
			}
			mux := http.NewServeMux()
			mux.Handle(service.WebhookPath, svc.Handler())
			mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			srv = &http.Server{
				Addr:    cfg.Server.ListenAddress,
				Handler: mux,
			}
			ln, err := net.Listen("tcp", cfg.Server.ListenAddress)
			if err != nil {
				return err
			}
			go func() { _ = srv.Serve(ln) }()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			svc.Stop()
			return srv.Shutdown(ctx)
		},
	})
}

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
		fx.Provide(service.NewTelegramBot),
		fx.Provide(tgstorage.NewTmpDataServiceS3Adapter),
		fx.Provide(tgstorage.NewTmpDataService),
		fx.Provide(tgstorage.NewGormDb),
		fx.Provide(tgstorage.NewMtprotoSessionStorage),
		fx.Provide(tgstorage.NewMediaFileCacheStorage),
		fx.Provide(service.NewMtprotoClient),
		fx.Provide(service.NewTelegramFileDownloader),
		fx.Provide(service.NewServiceChatUploader),
		fx.Provide(service.NewInlineResultBuilder),
		fx.Provide(service.NewStorageConnector),
		fx.Provide(service.NewYouTubeConnector),
		fx.Provide(service.NewAuthConnector),
		fx.Provide(service.NewPermissionService),
		fx.Provide(service.NewMessageHandlerService),
		fx.Provide(service.NewQueryProcessorFactory),
		fx.Provide(service.NewInlineService),
		fx.Provide(service.NewTelegramBotService),
		fx.Invoke(func(cfg *conf.Config) error {
			return tgstorage.RunMigrations(cfg, slog.With("service", "Migrator"))
		}),
		fx.Invoke(Startup),
	).Run()
}
