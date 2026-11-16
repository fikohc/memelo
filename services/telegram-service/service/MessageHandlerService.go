package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/fikohc/memelo/common/helper"
	commonservice "github.com/fikohc/memelo/common/service"
	"github.com/fikohc/memelo/common/temp"
	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/entity"
	tgstorage "github.com/fikohc/memelo/telegram-service/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
)

type MessageHandlerService interface {
	ProcessImageMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error)
	ProcessVideoMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error)
	ProcessDocumentMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error)
	ProcessYouTubeMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error)
}

const (
	parseMode    = "Markdown"
	msgForbidden = "You are not allowed to perform this action."

	// telegramMaxMessageLength is the Bot API limit for a message text,
	// counted in UTF-16 code units.
	telegramMaxMessageLength = 4096
	truncationSuffix         = "..."

	memeCreatedFormat = " Text: ```\n%s\n```\n" +
		" Caption: `%s`\n" +
		" ID: `%s` \n" +
		" Status: `%s`"

	// duplicateStatus is the CreateMemeStatus name of an already stored meme.
	duplicateStatus = "STATUS_DUPLICATE"
)

type MessageHandlerResponse struct {
	Message   string
	ParseMode string
}

type MessageHandlerServiceImpl struct {
	storage           StorageConnector
	fileDownloader    TelegramFileDownloader
	mediaCache        tgstorage.MediaFileCacheStorage
	sizeProber        MediaSizeProber
	permissionService PermissionService
	tmpDataService    commonservice.TmpDataService
	youtubeConnector  YouTubeConnector
	slogger           *slog.Logger
	staticAccountId   uuid.UUID
}

func (m MessageHandlerServiceImpl) ProcessImageMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error) {
	if message.From == nil {
		return nil, errors.New("messageHandlerService: message has no sender")
	}
	return InvokeWithPermission(ctx, m.permissionService, message.From.ID, PermissionCreate, func() (*MessageHandlerResponse, error) {
		if len(message.Photo) == 0 || message.Photo[len(message.Photo)-1].FileID == "" {
			return nil, errors.New("messageHandlerService: message dont contain image")
		}
		photo := message.Photo[len(message.Photo)-1]

		result, err := m.createMediaMeme(ctx, entity.ResultTypeImage, photo.FileID, "image/jpeg", m.staticAccountId)
		if err != nil {
			return nil, err
		}
		m.cacheFileId(ctx, result, entity.ResultTypeImage, photo.FileID, photo.FileUniqueID)

		m.slogger.InfoContext(ctx, "meme created",
			"imageId", result.Id,
			"duplicate", result.DuplicateStatus)

		return memeCreatedResponse(result), nil
	})
}

func (m MessageHandlerServiceImpl) ProcessVideoMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error) {
	if message.From == nil {
		return nil, errors.New("messageHandlerService: message has no sender")
	}
	return InvokeWithPermission(ctx, m.permissionService, message.From.ID, PermissionCreate, func() (*MessageHandlerResponse, error) {
		if message.Video == nil {
			return nil, errors.New("messageHandlerService: message does not contain a video")
		}

		result, err := m.createMediaMeme(ctx, entity.ResultTypeVideo, message.Video.FileID, message.Video.MimeType, m.staticAccountId)
		if err != nil {
			return nil, err
		}
		m.cacheFileId(ctx, result, entity.ResultTypeVideo, message.Video.FileID, message.Video.FileUniqueID)

		m.slogger.InfoContext(ctx, "video meme created",
			"memeId", result.Id,
			"duplicate", result.DuplicateStatus)

		return memeCreatedResponse(result), nil
	})
}

func (m MessageHandlerServiceImpl) ProcessDocumentMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error) {
	if message.From == nil {
		return nil, errors.New("messageHandlerService: message has no sender")
	}
	return InvokeWithPermission(ctx, m.permissionService, message.From.ID, PermissionCreate, func() (*MessageHandlerResponse, error) {
		if message.Document == nil {
			return nil, errors.New("messageHandlerService: message does not contain a document")
		}

		mimeType := message.Document.MimeType
		if mimeType == "" {
			return nil, errors.New("messageHandlerService: message has no mime type")
		}

		var typ string
		if strings.HasPrefix(mimeType, "video/") {
			typ = "video"
		} else if strings.HasPrefix(mimeType, "image/") {
			typ = "image"
		} else {
			return nil, errors.New("messageHandlerService: invalid mime type")
		}

		result, err := m.createMediaMeme(ctx, typ, message.Document.FileID, mimeType, m.staticAccountId)
		if err != nil {
			return nil, err
		}

		m.slogger.InfoContext(ctx, "document meme created",
			"memeId", result.Id,
			"duplicate", result.DuplicateStatus)

		return memeCreatedResponse(result), nil
	})
}

func (m MessageHandlerServiceImpl) ProcessYouTubeMessage(ctx context.Context, message *tgbotapi.Message) (*MessageHandlerResponse, error) {
	if message.From == nil {
		return nil, errors.New("ProcessYouTubeMessage: message has no sender")
	}
	return InvokeWithPermission(ctx, m.permissionService, message.From.ID, PermissionCreate, func() (*MessageHandlerResponse, error) {
		jobId, err := m.youtubeConnector.DownloadVideoAsync(ctx, message.Text)
		if err != nil {
			return nil, fmt.Errorf("ProcessYouTubeMessage: YouTube async job creation failed: %w", err)
		}

		var jobStatus *entity.YouTubeJobStatus
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
	poll:
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
				jobStatus, err = m.youtubeConnector.GetDownloadJobStatus(ctx, jobId)
				if err != nil {
					return nil, fmt.Errorf("messageHandlerService: youtube GetDownloadJobStatus failed: %w", err)
				}
				switch jobStatus.State {
				case "done":
					break poll
				case "failed":
					return nil, fmt.Errorf("messageHandlerService: YouTube download failed: %s", jobStatus.Error)
				}
			}
		}
		if jobStatus.MimeType != "video/mp4" {
			return nil, fmt.Errorf("messageHandlerService: YouTube download failed: unexpected mime type: %s", jobStatus.MimeType)
		}

		s3Media, err := m.tmpDataService.WrapInternalS3Path(ctx, jobStatus.S3Path)
		if err != nil {
			return nil, fmt.Errorf("messageHandlerService: youtube WrapInternalS3Path failed: %w", err)
		}

		result, err := m.storage.CreateMeme(ctx, s3Media, "video", m.staticAccountId)
		if err != nil {
			return nil, fmt.Errorf("messageHandlerService: youtube CreateMeme failed: %w", err)
		}

		m.slogger.InfoContext(ctx, "youtube meme created",
			"memeId", result.Id,
			"duplicate", result.DuplicateStatus)

		return memeCreatedResponse(result), nil
	})
}

func (m MessageHandlerServiceImpl) createMediaMeme(ctx context.Context, reqType string, fileId string, mimeType string, accountId uuid.UUID) (*entity.MemeCreateResult, error) {
	s3data, err := m.downloadToS3(ctx, fileId, mimeType)
	if err != nil {
		return nil, fmt.Errorf("messageHandlerService: downloadToS3 failed, fileId: %s : %w", fileId, err)
	}
	defer helper.QuietClose(s3data, m.slogger)

	result, err := m.storage.CreateMeme(ctx, s3data, reqType, accountId)
	if err != nil {
		return nil, fmt.Errorf("messageHandlerService: CreateMeme failed : %w", err)
	}

	return result, nil
}

// downloadToS3 streams a Telegram file over MTProto straight into temp S3 storage.
func (m MessageHandlerServiceImpl) downloadToS3(ctx context.Context, fileId string, mimeType string) (temp.S3BackedData, error) {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	pr, pw := io.Pipe()
	go func() {
		err := m.fileDownloader.Download(ctx, fileId, pw)
		if err != nil {
			m.slogger.ErrorContext(ctx, "downloadToS3: telegram download failed",
				"fileId", fileId,
				"error", err)
		}
		pw.CloseWithError(err)
	}()

	s3data, err := m.tmpDataService.ByReader(ctx, mimeType, pr)
	// Unblocks the downloader if the upload stopped reading early.
	pr.CloseWithError(err)
	if err != nil {
		m.slogger.ErrorContext(ctx, "downloadToS3: upload to s3 failed",
			"fileId", fileId,
			"mimeType", mimeType,
			"error", err)
		return nil, fmt.Errorf("downloadToS3: upload to s3: %w", err)
	}

	return s3data, nil
}

// cacheFileId stores the file_id of media the user sent, so inline results
// can send it as cached media without a service chat upload.
// Duplicates are skipped: the returned id belongs to another stored meme.
// Documents are not cached: their file_id is not accepted as a cached photo/video.
// The size stored is that of the media in S3, which storage-service may have
// converted, not the size of the file the user sent.
func (m MessageHandlerServiceImpl) cacheFileId(ctx context.Context, result *entity.MemeCreateResult, mediaType string, fileId string, fileUniqueId string) {
	if result.DuplicateStatus == duplicateStatus {
		return
	}

	var size int64
	if result.MediaUrl == "" {
		m.slogger.WarnContext(ctx, "Created meme has no media url, caching file id without size", "memeId", result.Id)
	} else if s3Size, err := m.sizeProber.Size(ctx, result.MediaUrl); err != nil {
		m.slogger.WarnContext(ctx, "Failed to get S3 media size, caching file id without size", "memeId", result.Id, "error", err)
	} else {
		size = s3Size
	}

	err := m.mediaCache.Save(ctx, &entity.MediaFileCache{
		MemeId:       result.Id,
		Type:         mediaType,
		FileId:       fileId,
		FileUniqueId: fileUniqueId,
		Size:         size,
	})
	if err != nil {
		m.slogger.ErrorContext(ctx, "Failed to cache media file id", "memeId", result.Id, "error", err)
	}
}

// memeCreatedResponse formats the reply to a created meme. If it exceeds the
// Telegram message limit, the extracted Text is shortened and ends with "...".
func memeCreatedResponse(result *entity.MemeCreateResult) *MessageHandlerResponse {
	format := func(text string) string {
		return fmt.Sprintf(memeCreatedFormat, text, result.Caption, result.Id, result.DuplicateStatus)
	}

	message := format(result.Text)
	if over := utf16Len(message) - telegramMaxMessageLength; over > 0 {
		keep := utf16Len(result.Text) - over - utf16Len(truncationSuffix)
		message = format(truncateUtf16(result.Text, keep) + truncationSuffix)
	}

	return &MessageHandlerResponse{
		Message:   message,
		ParseMode: parseMode,
	}
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// truncateUtf16 returns the longest prefix of s that fits in limit UTF-16
// code units, never splitting a rune.
func truncateUtf16(s string, limit int) string {
	n := 0
	for i, r := range s {
		n += utf16.RuneLen(r)
		if n > limit {
			return s[:i]
		}
	}
	return s
}

func NewMessageHandlerService(
	storage StorageConnector,
	fileDownloader TelegramFileDownloader,
	mediaCache tgstorage.MediaFileCacheStorage,
	sizeProber MediaSizeProber,
	permissionService PermissionService,
	tmpDataService commonservice.TmpDataService,
	youtubeConnector YouTubeConnector,
	cfg *conf.Config,
) MessageHandlerService {
	staticAccountId := uuid.MustParse(cfg.UserAccount.StaticUuid)
	return &MessageHandlerServiceImpl{
		storage:           storage,
		fileDownloader:    fileDownloader,
		mediaCache:        mediaCache,
		sizeProber:        sizeProber,
		permissionService: permissionService,
		tmpDataService:    tmpDataService,
		youtubeConnector:  youtubeConnector,
		slogger:           slog.With("service", "MessageHandlerService"),
		staticAccountId:   staticAccountId,
	}
}
