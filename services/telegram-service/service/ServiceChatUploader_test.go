package service

import (
	"bytes"
	"testing"

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
