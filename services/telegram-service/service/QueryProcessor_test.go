package service

import (
	"context"
	"errors"
	"testing"

	"github.com/fikohc/memelo/telegram-service/entity"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
)

func TestTypeTagRe_MatchesImageTag(t *testing.T) {
	query := "some query -type:image extra"
	match := typeTagRe.FindStringSubmatch(query)
	if match == nil {
		t.Fatal("expected match for -type:image, got nil")
	}
	if match[1] != "image" {
		t.Errorf("expected capture 'image', got %q", match[1])
	}
	cleaned := typeTagRe.ReplaceAllString(query, " ")
	if typeTagRe.MatchString(cleaned) {
		t.Errorf("expected tag stripped from %q, but still present", cleaned)
	}
}

func TestTypeTagRe_MatchesVideoTag(t *testing.T) {
	query := "-type:video"
	match := typeTagRe.FindStringSubmatch(query)
	if match == nil {
		t.Fatal("expected match for -type:video, got nil")
	}
	if match[1] != "video" {
		t.Errorf("expected capture 'video', got %q", match[1])
	}
}

func TestTypeTagRe_CaseInsensitive(t *testing.T) {
	for _, q := range []string{"-type:IMAGE", "-type:Video", "-type:IMAGE"} {
		if typeTagRe.FindStringSubmatch(q) == nil {
			t.Errorf("expected match for %q", q)
		}
	}
}

func TestTypeTagRe_NoMatchOnUnknownType(t *testing.T) {
	for _, q := range []string{"-type:gif", "-type:", "type:image"} {
		if typeTagRe.FindStringSubmatch(q) != nil {
			t.Errorf("expected no match for %q, but got one", q)
		}
	}
}

func TestTypeTagRe_QueryCleanedAfterStrip(t *testing.T) {
	cases := []struct {
		input    string
		wantType string
		wantRest string
	}{
		{"-type:image cat meme", "image", "cat meme"},
		{"cat meme -type:video", "video", "cat meme"},
		{"-type:image", "image", ""},
	}
	for _, tc := range cases {
		match := typeTagRe.FindStringSubmatch(tc.input)
		if match == nil {
			t.Errorf("input %q: expected match", tc.input)
			continue
		}
		got := typeTagRe.ReplaceAllString(tc.input, " ")
		gotTrimmed := got
		for len(gotTrimmed) > 0 && (gotTrimmed[0] == ' ' || gotTrimmed[len(gotTrimmed)-1] == ' ') {
			gotTrimmed = gotTrimmed[:len(gotTrimmed)-1]
			if len(gotTrimmed) > 0 && gotTrimmed[0] == ' ' {
				gotTrimmed = gotTrimmed[1:]
			}
		}
		if match[1] != tc.wantType {
			t.Errorf("input %q: type = %q, want %q", tc.input, match[1], tc.wantType)
		}
	}
}

type fakeMediaFileCache struct {
	entries map[uuid.UUID]entity.MediaFileCache
	err     error
}

func (f *fakeMediaFileCache) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]entity.MediaFileCache, error) {
	if f.err != nil {
		return nil, f.err
	}
	result := make(map[uuid.UUID]entity.MediaFileCache)
	for _, id := range ids {
		if entry, ok := f.entries[id]; ok {
			result[id] = entry
		}
	}
	return result, nil
}

func (f *fakeMediaFileCache) Save(context.Context, *entity.MediaFileCache) error { return nil }

func (f *fakeMediaFileCache) Delete(context.Context, uuid.UUID) error { return nil }

type fakeServiceChatUploader struct {
	enqueued []string
}

func (f *fakeServiceChatUploader) Enqueue(item entity.MemeSearchResult) {
	f.enqueued = append(f.enqueued, item.Id)
}

func TestInlineResultBuilder_UsesCachedMediaAndQueuesMisses(t *testing.T) {
	cachedVideo := uuid.New()
	cachedImage := uuid.New()
	missingVideo := uuid.New()
	missingImage := uuid.New()
	uploader := &fakeServiceChatUploader{}
	builder := NewInlineResultBuilder(
		&fakeMediaFileCache{entries: map[uuid.UUID]entity.MediaFileCache{
			cachedVideo: {MemeId: cachedVideo, Type: entity.ResultTypeVideo, FileId: "video-file-id"},
			cachedImage: {MemeId: cachedImage, Type: entity.ResultTypeImage, FileId: "photo-file-id"},
		}},
		uploader,
	)

	results, err := builder.Build(context.Background(), []entity.MemeSearchResult{
		{Id: cachedVideo.String(), Type: entity.ResultTypeVideo, MediaUrl: "https://example/cached.mp4"},
		{Id: cachedImage.String(), Type: entity.ResultTypeImage, MediaUrl: "https://example/cached.jpg"},
		{Id: missingVideo.String(), Type: entity.ResultTypeVideo, MediaUrl: "https://example/missing.mp4"},
		{Id: missingImage.String(), Type: entity.ResultTypeImage, MediaUrl: "https://example/missing.jpg"},
	}, "mark")
	if err != nil {
		t.Fatal(err)
	}

	video, ok := results[0].(tgbotapi.InlineQueryResultCachedVideo)
	if !ok {
		t.Fatalf("cached video: got %T, want InlineQueryResultCachedVideo", results[0])
	}
	if video.VideoID != "video-file-id" || video.Caption != "mark" {
		t.Errorf("cached video: got file id %q caption %q", video.VideoID, video.Caption)
	}
	photo, ok := results[1].(tgbotapi.InlineQueryResultCachedPhoto)
	if !ok {
		t.Fatalf("cached image: got %T, want InlineQueryResultCachedPhoto", results[1])
	}
	if photo.PhotoID != "photo-file-id" || photo.Caption != "mark" {
		t.Errorf("cached image: got file id %q caption %q", photo.PhotoID, photo.Caption)
	}
	if _, ok := results[2].(tgbotapi.InlineQueryResultVideo); !ok {
		t.Errorf("missing video: got %T, want InlineQueryResultVideo", results[2])
	}
	if _, ok := results[3].(tgbotapi.InlineQueryResultPhoto); !ok {
		t.Errorf("missing image: got %T, want InlineQueryResultPhoto", results[3])
	}
	want := []string{missingVideo.String(), missingImage.String()}
	if len(uploader.enqueued) != 2 || uploader.enqueued[0] != want[0] || uploader.enqueued[1] != want[1] {
		t.Errorf("enqueued = %v, want %v", uploader.enqueued, want)
	}
}

func TestInlineResultBuilder_IgnoresCachedEntryOfOtherType(t *testing.T) {
	id := uuid.New()
	builder := NewInlineResultBuilder(
		&fakeMediaFileCache{entries: map[uuid.UUID]entity.MediaFileCache{
			id: {MemeId: id, Type: entity.ResultTypeImage, FileId: "photo-file-id"},
		}},
		&fakeServiceChatUploader{},
	)

	results, err := builder.Build(context.Background(), []entity.MemeSearchResult{
		{Id: id.String(), Type: entity.ResultTypeVideo, MediaUrl: "https://example/video.mp4"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := results[0].(tgbotapi.InlineQueryResultVideo); !ok {
		t.Errorf("got %T, want InlineQueryResultVideo", results[0])
	}
}

func TestInlineResultBuilder_FallsBackToUrlWhenCacheFails(t *testing.T) {
	builder := NewInlineResultBuilder(
		&fakeMediaFileCache{err: errors.New("db down")},
		&fakeServiceChatUploader{},
	)

	results, err := builder.Build(context.Background(), []entity.MemeSearchResult{
		{Id: uuid.NewString(), Type: entity.ResultTypeVideo, MediaUrl: "https://example/video.mp4"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := results[0].(tgbotapi.InlineQueryResultVideo); !ok {
		t.Errorf("got %T, want InlineQueryResultVideo", results[0])
	}
}
