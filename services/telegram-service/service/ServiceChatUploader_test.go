package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fikohc/memelo/telegram-service/entity"
	"github.com/gotd/td/fileid"
	"github.com/gotd/td/tg"
)

func TestBotApiFileId_Video(t *testing.T) {
	doc := &tg.Document{
		ID:            123,
		AccessHash:    456,
		FileReference: []byte{1, 2, 3},
		DCID:          2,
		Attributes:    []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: 720, H: 1280}},
	}
	posted := &tg.Message{ID: 1, Media: &tg.MessageMediaDocument{Document: doc}}

	encoded, err := botApiFileId(posted, entity.ResultTypeVideo)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := fileid.DecodeFileID(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != fileid.Video || decoded.ID != 123 || decoded.AccessHash != 456 || decoded.DC != 2 ||
		!bytes.Equal(decoded.FileReference, doc.FileReference) {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestBotApiFileId_PhotoUsesLargestSize(t *testing.T) {
	photo := &tg.Photo{
		ID:            789,
		AccessHash:    1011,
		FileReference: []byte{4, 5},
		DCID:          4,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoStrippedSize{Type: "i"},
			&tg.PhotoSize{Type: "m", W: 320, H: 320},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 1280},
			&tg.PhotoSize{Type: "x", W: 800, H: 800},
		},
	}
	posted := &tg.Message{ID: 1, Media: &tg.MessageMediaPhoto{Photo: photo}}

	encoded, err := botApiFileId(posted, entity.ResultTypeImage)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := fileid.DecodeFileID(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != fileid.Photo || decoded.ID != 789 || decoded.DC != 4 {
		t.Errorf("decoded = %+v", decoded)
	}
	if got := decoded.PhotoSizeSource.ThumbnailType; got != 'y' {
		t.Errorf("thumbnail type = %q, want 'y'", got)
	}
}

func TestBotApiFileId_RejectsMismatchedType(t *testing.T) {
	doc := &tg.Document{ID: 1, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}}}
	posted := &tg.Message{ID: 1, Media: &tg.MessageMediaDocument{Document: doc}}

	if _, err := botApiFileId(posted, entity.ResultTypeImage); err == nil {
		t.Error("expected error for a video posted as image")
	}
}

type fakeServiceChatMediaUploader struct {
	upload func(context.Context, entity.MemeSearchResult) (string, error)
}

func (f *fakeServiceChatMediaUploader) Upload(ctx context.Context, item entity.MemeSearchResult) (string, error) {
	return f.upload(ctx, item)
}

func newTestUploader(upload func(context.Context, entity.MemeSearchResult) (string, error)) *BackgroundServiceChatUploaderImpl {
	return &BackgroundServiceChatUploaderImpl{
		uploader:      &fakeServiceChatMediaUploader{upload: upload},
		chatId:        -1001,
		log:           slog.Default(),
		queue:         make(chan *uploadJob, serviceChatQueueSize),
		priorityQueue: make(chan *uploadJob, serviceChatPriorityQueueSize),
		jobs:          make(map[string]*uploadJob),
	}
}

func TestUploaderWorker_TakesPriorityJobsFirst(t *testing.T) {
	var mu sync.Mutex
	var order []string
	u := newTestUploader(func(_ context.Context, item entity.MemeSearchResult) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, item.Id)
		return "file-" + item.Id, nil
	})

	u.EnqueueMaybe(entity.MemeSearchResult{Id: "ordinary-1"})
	u.EnqueueMaybe(entity.MemeSearchResult{Id: "ordinary-2"})
	done := make(chan struct{}, 2)
	notify := func(string, error) { done <- struct{}{} }
	u.EnqueuePriority(entity.MemeSearchResult{Id: "priority-1"}, notify)
	u.EnqueuePriority(entity.MemeSearchResult{Id: "priority-2"}, notify)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.worker(ctx)
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 4
	})

	want := []string{"priority-1", "priority-2", "ordinary-1", "ordinary-2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("upload order = %v, want %v", order, want)
		}
	}
}

func TestUploader_PriorityMovesUpQueuedJobAndUploadsOnce(t *testing.T) {
	var mu sync.Mutex
	uploads := 0
	u := newTestUploader(func(_ context.Context, item entity.MemeSearchResult) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		uploads++
		return "file-" + item.Id, nil
	})

	item := entity.MemeSearchResult{Id: "meme"}
	u.EnqueueMaybe(item)
	got := make(chan string, 1)
	u.EnqueuePriority(item, func(fileId string, err error) {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		got <- fileId
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.worker(ctx)

	select {
	case fileId := <-got:
		if fileId != "file-meme" {
			t.Errorf("fileId = %q, want file-meme", fileId)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not called")
	}
	// Let the worker drain the stale ordinary copy of the job.
	waitUntil(t, func() bool { return len(u.queue) == 0 })
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if uploads != 1 {
		t.Errorf("uploads = %d, want 1", uploads)
	}
}

func TestUploader_PriorityWithoutServiceChatFailsImmediately(t *testing.T) {
	u := newTestUploader(nil)
	u.chatId = 0

	var gotErr error
	u.EnqueuePriority(entity.MemeSearchResult{Id: "meme"}, func(_ string, err error) { gotErr = err })

	if !errors.Is(gotErr, errServiceChatDisabled) {
		t.Errorf("err = %v, want errServiceChatDisabled", gotErr)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
