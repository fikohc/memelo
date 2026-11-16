package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/fikohc/memelo/telegram-service/entity"
	"github.com/gotd/td/session"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// mtprotoSessionRowId is the id of the single session row; one bot per DB.
const mtprotoSessionRowId = 1

// MtprotoSessionStorage keeps the gotd session in Postgres so the bot
// authorization survives pod restarts.
type MtprotoSessionStorage session.Storage

type MtprotoSessionStorageImpl struct {
	db *gorm.DB
}

func NewMtprotoSessionStorage(db *gorm.DB) MtprotoSessionStorage {
	return &MtprotoSessionStorageImpl{db: db}
}

func (s *MtprotoSessionStorageImpl) LoadSession(ctx context.Context) ([]byte, error) {
	var row entity.MtprotoSession
	err := s.db.WithContext(ctx).First(&row, "id = ?", mtprotoSessionRowId).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, session.ErrNotFound
		}
		return nil, fmt.Errorf("mtproto session: load failed: %w", err)
	}
	return row.Data, nil
}

func (s *MtprotoSessionStorageImpl) StoreSession(ctx context.Context, data []byte) error {
	row := entity.MtprotoSession{Id: mtprotoSessionRowId, Data: data}
	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"data", "updated_at"}),
		}).
		Create(&row).Error
	if err != nil {
		return fmt.Errorf("mtproto session: store failed: %w", err)
	}
	return nil
}
