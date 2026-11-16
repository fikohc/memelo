package storage

import (
	"context"
	"fmt"

	"github.com/fikohc/memelo/telegram-service/entity"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MediaFileCacheStorage interface {
	// GetMany returns the cached entries of the ids that have one.
	GetMany(ctx context.Context, memeIds []uuid.UUID) (map[uuid.UUID]entity.MediaFileCache, error)
	Save(ctx context.Context, entry *entity.MediaFileCache) error
	Delete(ctx context.Context, memeId uuid.UUID) error
}

type MediaFileCacheStorageImpl struct {
	db *gorm.DB
}

func NewMediaFileCacheStorage(db *gorm.DB) MediaFileCacheStorage {
	return &MediaFileCacheStorageImpl{db: db}
}

func (s *MediaFileCacheStorageImpl) GetMany(ctx context.Context, memeIds []uuid.UUID) (map[uuid.UUID]entity.MediaFileCache, error) {
	result := make(map[uuid.UUID]entity.MediaFileCache, len(memeIds))
	if len(memeIds) == 0 {
		return result, nil
	}

	var rows []entity.MediaFileCache
	err := s.db.WithContext(ctx).
		Select("meme_id", "type", "file_id").
		Where("meme_id IN ?", memeIds).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("media file cache: get many failed: %w", err)
	}
	for _, row := range rows {
		result[row.MemeId] = row
	}
	return result, nil
}

func (s *MediaFileCacheStorageImpl) Save(ctx context.Context, entry *entity.MediaFileCache) error {
	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "meme_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"type", "file_id", "file_unique_id", "size"}),
		}).
		Create(entry).Error
	if err != nil {
		return fmt.Errorf("media file cache: save failed: memeId=%s: %w", entry.MemeId, err)
	}
	return nil
}

func (s *MediaFileCacheStorageImpl) Delete(ctx context.Context, memeId uuid.UUID) error {
	err := s.db.WithContext(ctx).
		Delete(&entity.MediaFileCache{}, "meme_id = ?", memeId).Error
	if err != nil {
		return fmt.Errorf("media file cache: delete failed: memeId=%s: %w", memeId, err)
	}
	return nil
}
