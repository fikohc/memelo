package service

import (
	"strings"
	"testing"

	"github.com/fikohc/memelo/telegram-service/entity"
	"github.com/google/uuid"
)

func TestMemeCreatedResponse_KeepsShortText(t *testing.T) {
	result := &entity.MemeCreateResult{Id: uuid.New(), Text: "short text", Caption: "caption"}

	response := memeCreatedResponse(result)

	if !strings.Contains(response.Message, "short text") {
		t.Errorf("message lost the text: %q", response.Message)
	}
	if strings.Contains(response.Message, truncationSuffix) {
		t.Errorf("short text must not be truncated: %q", response.Message)
	}
}

func TestMemeCreatedResponse_TruncatesLongText(t *testing.T) {
	for _, unit := range []string{"a", "я", "😀"} {
		result := &entity.MemeCreateResult{
			Id:              uuid.New(),
			Text:            strings.Repeat(unit, 5000),
			Caption:         "caption",
			DuplicateStatus: "STATUS_NEW",
		}

		response := memeCreatedResponse(result)

		if got := utf16Len(response.Message); got > telegramMaxMessageLength {
			t.Errorf("unit %q: message length %d exceeds %d", unit, got, telegramMaxMessageLength)
		}
		if !strings.Contains(response.Message, truncationSuffix+"\n```") {
			t.Errorf("unit %q: text must end with %q", unit, truncationSuffix)
		}
		if !strings.Contains(response.Message, result.Id.String()) || !strings.Contains(response.Message, "STATUS_NEW") {
			t.Errorf("unit %q: id and status must be kept", unit)
		}
	}
}
