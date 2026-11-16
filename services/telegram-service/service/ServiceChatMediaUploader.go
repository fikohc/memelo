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
	"github.com/google/uuid"
	"github.com/gotd/td/fileid"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

const (
	serviceChatMaxAttempts = 3
	// Bot API channel ids are -100<channel id>.
	botApiChannelIdOffset = -1000000000000
)

// ServiceChatMediaUploader uploads one meme's media to the private service
// chat over MTProto, caches the resulting Bot API file_id and returns it.
type ServiceChatMediaUploader interface {
	Upload(ctx context.Context, item entity.MemeSearchResult) (string, error)
}

type ServiceChatMediaUploaderImpl struct {
	mtproto MtprotoClient
	cache   storage.MediaFileCacheStorage
	chatId  int64
	log     *slog.Logger

	peerMu sync.Mutex
	peer   tg.InputPeerClass
}

// Upload uploads the item, retrying when Telegram asks to wait (FLOOD_WAIT).
func (u *ServiceChatMediaUploaderImpl) Upload(ctx context.Context, item entity.MemeSearchResult) (string, error) {
	return u.uploadWithRetry(ctx, item)
}

func (u *ServiceChatMediaUploaderImpl) uploadWithRetry(ctx context.Context, item entity.MemeSearchResult) (string, error) {
	var err error
	for attempt := 1; attempt <= serviceChatMaxAttempts; attempt++ {
		var fileId string
		fileId, err = u.upload(ctx, item)
		if err == nil {
			return fileId, nil
		}
		wait, ok := telegram.AsFloodWait(err)
		if !ok {
			return "", err
		}
		u.log.WarnContext(ctx, "Service chat upload hit flood wait", "memeId", item.Id, "wait", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", err
}

func (u *ServiceChatMediaUploaderImpl) upload(ctx context.Context, item entity.MemeSearchResult) (string, error) {
	memeId, err := uuid.Parse(item.Id)
	if err != nil {
		return "", fmt.Errorf("parse meme id: %w", err)
	}

	peer, err := u.resolvePeer(ctx)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.MediaUrl, nil)
	if err != nil {
		return "", fmt.Errorf("create media request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download media: %w", err)
	}
	defer helper.QuietClose(resp.Body, u.log)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("download media: non-2xx status: %d", resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return "", errors.New("download media: unknown content length")
	}

	media, filename, err := u.mediaOption(item)
	if err != nil {
		return "", err
	}

	api := u.mtproto.API()
	file, err := uploader.NewUploader(api).Upload(ctx, uploader.NewUpload(filename, resp.Body, resp.ContentLength))
	if err != nil {
		return "", fmt.Errorf("mtproto upload: %w", err)
	}

	posted, err := unpack.Message(message.NewSender(api).To(peer).Media(ctx, media(file)))
	if err != nil {
		return "", fmt.Errorf("post to service chat: %w", err)
	}

	fileId, err := botApiFileId(posted, item.Type)
	if err != nil {
		return "", err
	}

	err = u.cache.Save(ctx, &entity.MediaFileCache{
		MemeId: memeId,
		Type:   item.Type,
		FileId: fileId,
		Size:   resp.ContentLength,
	})
	if err != nil {
		return "", err
	}

	u.log.InfoContext(ctx, "Media uploaded to service chat",
		"memeId", item.Id,
		"type", item.Type,
		"messageId", posted.ID,
		"size", resp.ContentLength)
	return fileId, nil
}

// mediaOption returns how an uploaded file is posted: videos as streamable
// video documents, images as photos.
func (u *ServiceChatMediaUploaderImpl) mediaOption(item entity.MemeSearchResult) (func(tg.InputFileClass) message.MediaOption, string, error) {
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

// botApiFileId encodes the Bot API file_id of the media in the posted
// service chat message. The post is kept: the file reference inside the
// file_id stays valid while it exists.
func botApiFileId(posted *tg.Message, mediaType string) (string, error) {
	var fid fileid.FileID
	switch media := posted.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := media.Document.AsNotEmpty()
		if !ok || mediaType != entity.ResultTypeVideo {
			return "", fmt.Errorf("service chat message %d: unexpected document for %s", posted.ID, mediaType)
		}
		fid = fileid.FromDocument(doc)
		if fid.Type != fileid.Video {
			return "", fmt.Errorf("service chat message %d: document is %s, not a video", posted.ID, fid.Type)
		}
	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.(*tg.Photo)
		if !ok || mediaType != entity.ResultTypeImage {
			return "", fmt.Errorf("service chat message %d: unexpected photo for %s", posted.ID, mediaType)
		}
		thumbType, ok := largestPhotoSizeType(photo)
		if !ok {
			return "", fmt.Errorf("service chat message %d: photo has no sizes", posted.ID)
		}
		fid = fileid.FromPhoto(photo, thumbType)
	default:
		return "", fmt.Errorf("service chat message %d: unexpected media %T", posted.ID, posted.Media)
	}

	encoded, err := fileid.EncodeFileID(fid)
	if err != nil {
		return "", fmt.Errorf("service chat message %d: encode file id: %w", posted.ID, err)
	}
	return encoded, nil
}

// largestPhotoSizeType returns the size type (e.g. 'y') of the biggest
// variant of a photo; the Bot API file_id of a photo points to one size.
func largestPhotoSizeType(photo *tg.Photo) (rune, bool) {
	var sizeType string
	maxArea := -1
	for _, size := range photo.Sizes {
		var w, h int
		var typ string
		switch s := size.(type) {
		case *tg.PhotoSize:
			w, h, typ = s.W, s.H, s.Type
		case *tg.PhotoSizeProgressive:
			w, h, typ = s.W, s.H, s.Type
		default:
			continue
		}
		if area := w * h; area > maxArea && typ != "" {
			maxArea, sizeType = area, typ
		}
	}
	if sizeType == "" {
		return 0, false
	}
	return []rune(sizeType)[0], true
}

func (u *ServiceChatMediaUploaderImpl) resolvePeer(ctx context.Context) (tg.InputPeerClass, error) {
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

func NewServiceChatMediaUploader(
	mtproto MtprotoClient,
	cache storage.MediaFileCacheStorage,
	cfg *conf.Config,
) ServiceChatMediaUploader {
	return &ServiceChatMediaUploaderImpl{
		mtproto: mtproto,
		cache:   cache,
		chatId:  cfg.Telegram.ServiceChatId,
		log:     slog.With("service", "ServiceChatMediaUploader"),
	}
}
