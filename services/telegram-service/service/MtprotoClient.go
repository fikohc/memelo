package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"go.uber.org/fx"
)

const mtprotoStartTimeout = 30 * time.Second

// MtprotoClient is an MTProto connection logged in as the same bot as the
// Bot API client. It is used only to move files: the Bot API limits
// downloads and URL uploads to 20 MB, MTProto allows up to 2 GB.
// Updates are still received through the Bot API webhook.
type MtprotoClient interface {
	Client() *telegram.Client
	API() *tg.Client
}

type MtprotoClientImpl struct {
	client *telegram.Client
	log    *slog.Logger
}

func (m *MtprotoClientImpl) Client() *telegram.Client { return m.client }

func (m *MtprotoClientImpl) API() *tg.Client { return m.client.API() }

func NewMtprotoClient(
	lc fx.Lifecycle,
	cfg *conf.Config,
	sessionStorage storage.MtprotoSessionStorage,
) MtprotoClient {
	client := telegram.NewClient(cfg.Mtproto.ApiId, cfg.Mtproto.ApiHash, telegram.Options{
		SessionStorage: sessionStorage,
		NoUpdates:      true,
	})
	m := &MtprotoClientImpl{
		client: client,
		log:    slog.With("service", "MtprotoClient"),
	}

	var cancel context.CancelFunc
	runDone := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			if cfg.Mtproto.ApiId == 0 || cfg.Mtproto.ApiHash == "" {
				return errors.New("MtprotoClient: mtproto.ApiId and mtproto.ApiHash must be set")
			}
			var runCtx context.Context
			runCtx, cancel = context.WithCancel(context.Background())
			ready := make(chan error, 1)

			go func() {
				defer close(runDone)
				err := client.Run(runCtx, func(ctx context.Context) error {
					if err := m.authorize(ctx, cfg.Telegram.Token); err != nil {
						return err
					}
					ready <- nil
					<-ctx.Done()
					return ctx.Err()
				})
				if err != nil && !errors.Is(err, context.Canceled) {
					m.log.Error("MTProto client stopped", "error", err)
					select {
					case ready <- err:
					default:
					}
				}
			}()

			select {
			case err := <-ready:
				if err != nil {
					return fmt.Errorf("MtprotoClient: start failed: %w", err)
				}
				return nil
			case <-time.After(mtprotoStartTimeout):
				return errors.New("MtprotoClient: start timed out")
			case <-startCtx.Done():
				return startCtx.Err()
			}
		},
		OnStop: func(ctx context.Context) error {
			if cancel != nil {
				cancel()
			}
			select {
			case <-runDone:
			case <-ctx.Done():
			}
			return nil
		},
	})
	return m
}

func (m *MtprotoClientImpl) authorize(ctx context.Context, token string) error {
	status, err := m.client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("MtprotoClient: auth status failed: %w", err)
	}
	if status.Authorized {
		m.log.InfoContext(ctx, "MTProto session restored", "bot", status.User.Username)
		return nil
	}
	if _, err := m.client.Auth().Bot(ctx, token); err != nil {
		return fmt.Errorf("MtprotoClient: bot login failed: %w", err)
	}
	m.log.InfoContext(ctx, "MTProto bot authorized")
	return nil
}
