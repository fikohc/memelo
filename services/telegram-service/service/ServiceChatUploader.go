package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fikohc/memelo/common/helper"
	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/entity"
	"github.com/fikohc/memelo/telegram-service/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/fx"
)

const (
	serviceChatQueueSize   = 64
	serviceChatWorkers     = 2
	serviceChatMaxAttempts = 3
	// Bot API channel ids are -100<channel id>.
	botApiChannelIdOffset = -1000000000000
)

// ServiceChatUploader uploads meme media (videos and images) to the private
// service chat over MTProto and caches the resulting Bot API file_id, so
// inline results can send cached media instead of URLs (Telegram fetches
// URLs only up to 5 MB for photos and 20 MB for videos).
type ServiceChatUploader interface {
	// Enqueue schedules a background upload. It never blocks; duplicates of
	// an in-flight upload and overflow of the queue are dropped.
	Enqueue(item entity.MemeSearchResult)
}

type ServiceChatUploaderImpl struct {
	mtproto MtprotoClient
	bot     *tgbotapi.BotAPI
	cache   storage.MediaFileCacheStorage
	chatId  int64
	log     *slog.Logger

	queue    chan entity.MemeSearchResult
	inflight sync.Map

	peerMu sync.Mutex
	peer   tg.InputPeerClass
}

func (u *ServiceChatUploaderImpl) Enqueue(item entity.MemeSearchResult) {
	if u.chatId == 0 {
		return
	}
	if _, loaded := u.inflight.LoadOrStore(item.Id, struct{}{}); loaded {
		return
	}
	select {
	case u.queue <- item:
	default:
		u.inflight.Delete(item.Id)
		u.log.Warn("Service chat upload queue is full, dropping", "memeId", item.Id)
	}
}

func (u *ServiceChatUploaderImpl) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-u.queue:
			if err := u.uploadWithRetry(ctx, item); err != nil {
				u.log.ErrorContext(ctx, "Service chat upload failed", "memeId", item.Id, "error", err)
			}
			u.inflight.Delete(item.Id)
		}
	}
}

func (u *ServiceChatUploaderImpl) uploadWithRetry(ctx context.Context, item entity.MemeSearchResult) error {
	var err error
	for attempt := 1; attempt <= serviceChatMaxAttempts; attempt++ {
		err = u.upload(ctx, item)
		if err == nil {
			return nil
		}
		wait, ok := telegram.AsFloodWait(err)
		if !ok {
			return err
		}
		u.log.WarnContext(ctx, "Service chat upload hit flood wait", "memeId", item.Id, "wait", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (u *ServiceChatUploaderImpl) upload(ctx context.Context, item entity.MemeSearchResult) error {
	memeId, err := uuid.Parse(item.Id)
	if err != nil {
		return fmt.Errorf("parse meme id: %w", err)
	}

	peer, err := u.resolvePeer(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.MediaUrl, nil)
	if err != nil {
		return fmt.Errorf("create media request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download media: %w", err)
	}
	defer helper.QuietClose(resp.Body, u.log)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download media: non-2xx status: %d", resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return errors.New("download media: unknown content length")
	}

	media, filename, err := u.mediaOption(item)
	if err != nil {
		return err
	}

	api := u.mtproto.API()
	file, err := uploader.NewUploader(api).Upload(ctx, uploader.NewUpload(filename, resp.Body, resp.ContentLength))
	if err != nil {
		return fmt.Errorf("mtproto upload: %w", err)
	}

	posted, err := unpack.Message(message.NewSender(api).To(peer).Media(ctx, media(file)))
	if err != nil {
		return fmt.Errorf("post to service chat: %w", err)
	}

	fileId, fileUniqueId, err := u.botApiFileId(posted.ID, item.Type)
	if err != nil {
		return err
	}

	err = u.cache.Save(ctx, &entity.MediaFileCache{
		MemeId:       memeId,
		Type:         item.Type,
		FileId:       fileId,
		FileUniqueId: fileUniqueId,
		Size:         resp.ContentLength,
	})
	if err != nil {
		return err
	}

	u.log.InfoContext(ctx, "Media uploaded to service chat",
		"memeId", item.Id,
		"type", item.Type,
		"messageId", posted.ID,
		"size", resp.ContentLength)
	return nil
}

// mediaOption returns how an uploaded file is posted: videos as streamable
// video documents, images as photos.
func (u *ServiceChatUploaderImpl) mediaOption(item entity.MemeSearchResult) (func(tg.InputFileClass) message.MediaOption, string, error) {
	caption := styling.Plain(item.Id)
	switch item.Type {
	case entity.ResultTypeVideo:
		return func(file tg.InputFileClass) message.MediaOption {
			return message.UploadedDocument(file, caption).
				MIME("video/mp4").
				Filename("video.mp4").
				Attributes(&tg.DocumentAttributeVideo{
					SupportsStreaming: true,
					W:                 item.MediaWidth,
					H:                 item.MediaHeight,
				})
		}, "video.mp4", nil
	case entity.ResultTypeImage:
		return func(file tg.InputFileClass) message.MediaOption {
			return message.UploadedPhoto(file, caption)
		}, "image.jpg", nil
	}
	return nil, "", fmt.Errorf("unsupported media type %q", item.Type)
}

// botApiFileId forwards the posted message within the service chat through
// the Bot API, so Telegram itself issues the Bot API file_id, then deletes
// the forwarded copy. The original post is kept: the file reference stays
// valid while it exists.
func (u *ServiceChatUploaderImpl) botApiFileId(messageId int, mediaType string) (string, string, error) {
	forwarded, err := u.bot.Send(tgbotapi.NewForward(u.chatId, u.chatId, messageId))
	if err != nil {
		return "", "", fmt.Errorf("forward service chat message: %w", err)
	}
	if _, err := u.bot.Request(tgbotapi.NewDeleteMessage(u.chatId, forwarded.MessageID)); err != nil {
		u.log.Warn("Failed to delete forwarded service chat message", "messageId", forwarded.MessageID, "error", err)
	}

	switch {
	case mediaType == entity.ResultTypeVideo && forwarded.Video != nil:
		return forwarded.Video.FileID, forwarded.Video.FileUniqueID, nil
	case mediaType == entity.ResultTypeImage && len(forwarded.Photo) > 0:
		largest := forwarded.Photo[len(forwarded.Photo)-1]
		return largest.FileID, largest.FileUniqueID, nil
	}
	return "", "", fmt.Errorf("forwarded service chat message %d has no %s", messageId, mediaType)
}

func (u *ServiceChatUploaderImpl) resolvePeer(ctx context.Context) (tg.InputPeerClass, error) {
	u.peerMu.Lock()
	defer u.peerMu.Unlock()
	if u.peer != nil {
		return u.peer, nil
	}

	channelId := botApiChannelIdOffset - u.chatId
	// Bots may pass access_hash 0 for channels they are a member of.
	chats, err := u.mtproto.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: channelId},
	})
	if err != nil {
		return nil, fmt.Errorf("resolve service chat %d: %w", u.chatId, err)
	}
	for _, chat := range chats.GetChats() {
		if channel, ok := chat.(*tg.Channel); ok && channel.ID == channelId {
			u.peer = channel.AsInputPeer()
			return u.peer, nil
		}
	}
	return nil, fmt.Errorf("resolve service chat %d: channel not found", u.chatId)
}

func NewServiceChatUploader(
	lc fx.Lifecycle,
	mtproto MtprotoClient,
	bot *tgbotapi.BotAPI,
	cache storage.MediaFileCacheStorage,
	cfg *conf.Config,
) ServiceChatUploader {
	u := &ServiceChatUploaderImpl{
		mtproto: mtproto,
		bot:     bot,
		cache:   cache,
		chatId:  cfg.Telegram.ServiceChatId,
		log:     slog.With("service", "ServiceChatUploader"),
		queue:   make(chan entity.MemeSearchResult, serviceChatQueueSize),
	}
	if u.chatId == 0 {
		u.log.Warn("telegram.ServiceChatId is not set, media won't be cached in the service chat")
	}

	var cancel context.CancelFunc
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			for range serviceChatWorkers {
				go u.worker(ctx)
			}
			return nil
		},
		OnStop: func(context.Context) error {
			if cancel != nil {
				cancel()
			}
			return nil
		},
	})
	return u
}
