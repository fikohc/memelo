package ocr

import (
	"context"

	"github.com/fikohc/memelo/common/temp"
	"github.com/fikohc/memelo/storage-service/entity"
)

type LlmEmbeddingExtractor interface {
	GetImageEmbedding(ctx context.Context, image temp.Data) (*entity.EmbeddingItem, error)
	GetTextEmbedding(ctx context.Context, text string) (*entity.EmbeddingItem, error)
	GetVideoEmbedding(ctx context.Context, video temp.Data) ([]entity.EmbeddingItem, error)
}
