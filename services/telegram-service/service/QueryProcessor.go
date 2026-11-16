package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/entity"
	tgstorage "github.com/fikohc/memelo/telegram-service/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
)

var typeTagRe = regexp.MustCompile(`(?i)\s*-type:(image|video)\s*`)

type QueryProcessorResult struct {
	Results    []interface{}
	Pagination *entity.PaginationOffset
	CacheTime  int
}

type QueryProcessor interface {
	Process(ctx context.Context, accountId uuid.UUID, query string, pagination *entity.PaginationOffset) (*QueryProcessorResult, error)
	ProcessChosenQuery(ctx context.Context, accountId uuid.UUID, resultId string) error
	HelpText() string
}

type QueryProcessorEntry struct {
	Permission string
	Processor  QueryProcessor
}

type QueryProcessorFactory interface {
	GetProcessor(rawQuery string) QueryProcessor
	Entries() []QueryProcessorEntry
}

const (
	// largeMediaThreshold is the largest media Telegram fetches by URL for
	// inline results; bigger media can only be sent by cached file_id.
	largeMediaThreshold = 20 << 20
	// largeMediaWaitTimeout bounds how long an inline query waits for
	// priority uploads of large media; Telegram expects an answer within ~10 s.
	largeMediaWaitTimeout = 5 * time.Second
	mediaSizeProbeTimeout = 2 * time.Second
)

// InlineResultBuilder converts search results into Telegram inline results.
// Media with a cached Bot API file_id is sent as a cached photo/video. Other
// media up to 20 MB is sent by URL and queued for a background upload to the
// service chat. Larger media can't be sent by URL at all: it is uploaded
// with priority while the query waits, and left out if that takes too long.
type InlineResultBuilder interface {
	Build(ctx context.Context, results []entity.MemeSearchResult, mark string) ([]any, error)
}

type InlineResultBuilderImpl struct {
	cache              tgstorage.MediaFileCacheStorage
	backgroundUploader BackgroundServiceChatUploader
	sizeProber         MediaSizeProber
	largeMediaWait     time.Duration
	log                *slog.Logger
}

type priorityUploadResult struct {
	memeId string
	fileId string
	err    error
}

func (b *InlineResultBuilderImpl) Build(ctx context.Context, results []entity.MemeSearchResult, mark string) ([]any, error) {
	fileIds := b.cachedFileIds(ctx, results)

	var misses []entity.MemeSearchResult
	for _, item := range results {
		if _, cached := fileIds[item.Id]; !cached && isMedia(item) {
			misses = append(misses, item)
		}
	}
	large := b.largeMedia(ctx, misses)
	for _, item := range misses {
		if !large[item.Id] {
			b.backgroundUploader.EnqueueMaybe(item)
		}
	}
	maps.Copy(fileIds, b.uploadLargeMedia(ctx, misses, large))

	inlineResults := make([]any, 0, len(results))
	excluded := 0
	for _, item := range results {
		b.log.DebugContext(ctx, "SearchResultItem", "id", item.Id, "url", item.MediaUrl)

		fileId, cached := fileIds[item.Id]
		if !cached && large[item.Id] {
			excluded++
			continue
		}
		inlineResultItem, err := inlineResult(item, fileId, cached, mark)
		if err != nil {
			return nil, err
		}
		inlineResults = append(inlineResults, inlineResultItem)
	}

	if excluded > 0 {
		b.log.InfoContext(ctx, "Excluded large media not uploaded in time", "count", excluded)
	}
	return inlineResults, nil
}

func isMedia(item entity.MemeSearchResult) bool {
	return item.Type == entity.ResultTypeImage || item.Type == entity.ResultTypeVideo
}

func inlineResult(item entity.MemeSearchResult, fileId string, cached bool, mark string) (any, error) {
	caption := item.Caption
	if caption == "" {
		caption = "media"
	}

	switch item.Type {
	case entity.ResultTypeImage:
		if cached {
			inlineChoice := tgbotapi.NewInlineQueryResultCachedPhoto(item.Id, fileId)
			inlineChoice.Caption = mark
			return inlineChoice, nil
		}
		inlineChoice := tgbotapi.NewInlineQueryResultPhotoWithThumb(item.Id, item.MediaUrl, item.ThumbUrl)
		inlineChoice.MimeType = "image/jpeg"
		inlineChoice.Height = item.ThumbHeight
		inlineChoice.Width = item.ThumbWidth
		inlineChoice.Caption = mark
		return inlineChoice, nil

	case entity.ResultTypeVideo:
		if cached {
			inlineChoice := tgbotapi.NewInlineQueryResultCachedVideo(item.Id, fileId, caption)
			inlineChoice.Caption = mark
			return inlineChoice, nil
		}
		inlineChoice := tgbotapi.NewInlineQueryResultVideo(item.Id, item.MediaUrl)
		inlineChoice.MimeType = "video/mp4"
		inlineChoice.ThumbURL = item.ThumbUrl
		inlineChoice.Width = item.ThumbWidth
		inlineChoice.Height = item.ThumbHeight
		inlineChoice.Title = caption
		inlineChoice.Caption = mark
		return inlineChoice, nil
	}
	return nil, fmt.Errorf("InlineResultBuilder: unknown result type %q", item.Type)
}

// largeMedia probes the sizes of the given media in parallel and returns the
// ids of those over largeMediaThreshold. Media whose size can't be probed is
// treated as small, so it still gets a URL result.
func (b *InlineResultBuilderImpl) largeMedia(ctx context.Context, items []entity.MemeSearchResult) map[string]bool {
	sizes := make([]int64, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, mediaSizeProbeTimeout)
			defer cancel()
			size, err := b.sizeProber.Size(probeCtx, item.MediaUrl)
			if err != nil {
				b.log.WarnContext(ctx, "Media size probe failed", "memeId", item.Id, "error", err)
				return
			}
			sizes[i] = size
		}()
	}
	wg.Wait()

	large := make(map[string]bool)
	for i, item := range items {
		if sizes[i] > largeMediaThreshold {
			large[item.Id] = true
		}
	}
	return large
}

// uploadLargeMedia schedules priority uploads of the large misses and waits
// for them until largeMediaWait passes. It returns memeId -> file_id of the
// uploads that finished in time.
func (b *InlineResultBuilderImpl) uploadLargeMedia(ctx context.Context, misses []entity.MemeSearchResult, large map[string]bool) map[string]string {
	uploaded := make(map[string]string)
	if len(large) == 0 {
		return uploaded
	}

	// Buffered for every callback, so a late one never blocks an upload worker.
	done := make(chan priorityUploadResult, len(large))
	for _, item := range misses {
		if !large[item.Id] {
			continue
		}
		memeId := item.Id
		b.backgroundUploader.EnqueuePriority(item, func(fileId string, err error) {
			done <- priorityUploadResult{memeId: memeId, fileId: fileId, err: err}
		})
	}

	timeout := time.NewTimer(b.largeMediaWait)
	defer timeout.Stop()
	for range len(large) {
		select {
		case result := <-done:
			if result.err != nil {
				b.log.WarnContext(ctx, "Priority upload failed", "memeId", result.memeId, "error", result.err)
				continue
			}
			uploaded[result.memeId] = result.fileId
		case <-timeout.C:
			return uploaded
		case <-ctx.Done():
			return uploaded
		}
	}
	return uploaded
}

// cachedFileIds looks up file_ids for the whole page in one query. Entries
// whose type differs from the result's are ignored, since a photo file_id
// can't be sent as a video and vice versa. A lookup failure degrades to URL
// results instead of failing the query.
func (b *InlineResultBuilderImpl) cachedFileIds(ctx context.Context, results []entity.MemeSearchResult) map[string]string {
	fileIds := make(map[string]string)

	ids := make([]uuid.UUID, 0, len(results))
	for _, item := range results {
		if id, err := uuid.Parse(item.Id); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return fileIds
	}

	cached, err := b.cache.GetMany(ctx, ids)
	if err != nil {
		b.log.ErrorContext(ctx, "Media file cache lookup failed", "error", err)
		return fileIds
	}
	for _, item := range results {
		id, err := uuid.Parse(item.Id)
		if err != nil {
			continue
		}
		if entry, ok := cached[id]; ok && entry.Type == item.Type {
			fileIds[item.Id] = entry.FileId
		}
	}
	return fileIds
}

func NewInlineResultBuilder(
	cache tgstorage.MediaFileCacheStorage,
	uploader BackgroundServiceChatUploader,
	sizeProber MediaSizeProber,
) InlineResultBuilder {
	return &InlineResultBuilderImpl{
		cache:              cache,
		backgroundUploader: uploader,
		sizeProber:         sizeProber,
		largeMediaWait:     largeMediaWaitTimeout,
		log:                slog.With("service", "InlineResultBuilder"),
	}
}

// SearchQueryProcessor handles plain search queries.
type SearchQueryProcessor struct {
	prefixSearchProcessor
}

func (p *SearchQueryProcessor) ProcessChosenQuery(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

// prefixSearchProcessor is a shared base for processors that strip a prefix,
// search, and show results with a fixed caption mark.
type prefixSearchProcessor struct {
	storage  StorageConnector
	builder  InlineResultBuilder
	config   *conf.Config
	log      *slog.Logger
	prefix   string
	mark     string
	helpText string
}

func (p *prefixSearchProcessor) HelpText() string { return p.helpText }

func (p *prefixSearchProcessor) Process(ctx context.Context, accountId uuid.UUID, query string, pagination *entity.PaginationOffset) (*QueryProcessorResult, error) {
	queryString := strings.TrimSpace(strings.TrimPrefix(query, p.prefix))

	var metadataType *string
	if match := typeTagRe.FindStringSubmatch(queryString); match != nil {
		t := strings.ToLower(match[1])
		metadataType = &t
		queryString = strings.TrimSpace(typeTagRe.ReplaceAllString(queryString, " "))
	}

	params := entity.SearchParams{Query: queryString, Type: metadataType, Pagination: pagination}
	searchResult, err := p.storage.ProcessSearchQuery(ctx, accountId, params, p.config.Inline.PageSize)
	if err != nil {
		return nil, fmt.Errorf("prefixSearchProcessor: %w", err)
	}

	results, err := p.builder.Build(ctx, searchResult.Results, p.mark)
	if err != nil {
		return nil, err
	}

	// Count search hits, not inline results: large media may be left out.
	var nextPagination *entity.PaginationOffset
	if len(searchResult.Results) == p.config.Inline.PageSize && p.config.Inline.PageSize > 0 {
		nextPagination = searchResult.Pagination
	}
	return &QueryProcessorResult{Results: results, Pagination: nextPagination, CacheTime: 120}, nil
}

// DeleteQueryProcessor handles /delete queries: shows results marked for deletion,
// and performs the actual delete when a result is chosen.
type DeleteQueryProcessor struct {
	prefixSearchProcessor
	mediaCache tgstorage.MediaFileCacheStorage
}

func (p *DeleteQueryProcessor) ProcessChosenQuery(ctx context.Context, accountId uuid.UUID, resultId string) error {
	memeIdUuid, err := uuid.Parse(resultId)
	if err != nil {
		return fmt.Errorf("DeleteQueryProcessor: failed to parse meme uuid %q: %w", resultId, err)
	}
	err = p.storage.DeleteMeme(ctx, accountId, memeIdUuid)
	if err != nil {
		return fmt.Errorf("DeleteQueryProcessor: DeleteMeme failed, accountId=%s, memeId=%s: %w", accountId, memeIdUuid, err)
	}
	if err = p.mediaCache.Delete(ctx, memeIdUuid); err != nil {
		p.log.ErrorContext(ctx, "Failed to delete cached media file id", "memeId", memeIdUuid, "error", err)
	}
	return nil
}

// RandomQueryProcessor handles /random [image|video] queries.
// The query parameter is a media type specifier ("image", "video", or ""), not a search term.
// Pagination is intentionally ignored — random results have no meaningful cursor.
type RandomQueryProcessor struct {
	storage  StorageConnector
	builder  InlineResultBuilder
	log      *slog.Logger
	helpText string
}

func (p *RandomQueryProcessor) HelpText() string { return p.helpText }

func (p *RandomQueryProcessor) Process(ctx context.Context, accountId uuid.UUID, query string, _ *entity.PaginationOffset) (*QueryProcessorResult, error) {
	mediaType := strings.TrimSpace(strings.TrimPrefix(query, randomPrefix))
	mediaType = strings.TrimPrefix(mediaType, "-type:")

	if mediaType != entity.ResultTypeImage && mediaType != entity.ResultTypeVideo {
		mediaType = ""
	}

	meme, err := p.storage.GetRandomMeme(ctx, accountId, mediaType)
	if err != nil {
		return nil, fmt.Errorf("RandomQueryProcessor: %w", err)
	}
	if meme == nil {
		return &QueryProcessorResult{}, nil
	}

	results, err := p.builder.Build(ctx, []entity.MemeSearchResult{*meme}, "")
	if err != nil {
		return nil, err
	}
	return &QueryProcessorResult{Results: results, CacheTime: 1}, nil
}

func (p *RandomQueryProcessor) ProcessChosenQuery(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

// RecomputeQueryProcessor handles /recompute queries: shows search results,
// and triggers a recompute job when a result is chosen.
type RecomputeQueryProcessor struct {
	prefixSearchProcessor
}

func (p *RecomputeQueryProcessor) ProcessChosenQuery(ctx context.Context, accountId uuid.UUID, resultId string) error {
	memeId, err := uuid.Parse(resultId)
	if err != nil {
		return fmt.Errorf("RecomputeQueryProcessor: failed to parse meme uuid %q: %w", resultId, err)
	}
	return p.storage.StartRecomputeById(ctx, accountId, memeId)
}

type QueryProcessorFactoryImpl struct {
	searchProcessor    QueryProcessor
	deleteProcessor    QueryProcessor
	randomProcessor    QueryProcessor
	recomputeProcessor QueryProcessor
}

func (f *QueryProcessorFactoryImpl) GetProcessor(rawQuery string) QueryProcessor {
	switch {
	case strings.HasPrefix(rawQuery, inlineDeletePrefix):
		return f.deleteProcessor
	case strings.HasPrefix(rawQuery, inlineRecomputePrefix):
		return f.recomputeProcessor
	case strings.HasPrefix(rawQuery, randomPrefix):
		return f.randomProcessor
	default:
		return f.searchProcessor
	}
}

func (f *QueryProcessorFactoryImpl) Entries() []QueryProcessorEntry {
	return []QueryProcessorEntry{
		{Permission: PermissionSearch, Processor: f.searchProcessor},
		{Permission: PermissionSearch, Processor: f.randomProcessor},
		{Permission: PermissionDelete, Processor: f.deleteProcessor},
		{Permission: PermissionRecompute, Processor: f.recomputeProcessor},
	}
}

func NewQueryProcessorFactory(
	storage StorageConnector,
	builder InlineResultBuilder,
	mediaCache tgstorage.MediaFileCacheStorage,
	config *conf.Config,
) QueryProcessorFactory {
	return &QueryProcessorFactoryImpl{
		searchProcessor: &SearchQueryProcessor{prefixSearchProcessor{
			storage:  storage,
			builder:  builder,
			config:   config,
			log:      slog.With("service", "SearchQueryProcessor"),
			prefix:   "",
			mark:     "",
			helpText: "<query> — search by text",
		}},
		deleteProcessor: &DeleteQueryProcessor{prefixSearchProcessor{
			storage:  storage,
			builder:  builder,
			config:   config,
			log:      slog.With("service", "DeleteQueryProcessor"),
			prefix:   inlineDeletePrefix,
			mark:     "Deleted",
			helpText: "/delete <query> — delete memes",
		}, mediaCache},
		randomProcessor: &RandomQueryProcessor{
			storage:  storage,
			builder:  builder,
			log:      slog.With("service", "RandomQueryProcessor"),
			helpText: "/random — get a random meme",
		},
		recomputeProcessor: &RecomputeQueryProcessor{prefixSearchProcessor{
			storage:  storage,
			builder:  builder,
			config:   config,
			log:      slog.With("service", "RecomputeQueryProcessor"),
			prefix:   inlineRecomputePrefix,
			mark:     "Recompute",
			helpText: "/recompute <query> — recompute meme metadata",
		}},
	}
}
