package storage

import (
	"fmt"
	"log/slog"

	"github.com/fikohc/memelo/telegram-service/conf"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func NewGormDb(cfg *conf.Config) (*gorm.DB, error) {
	// A video without a cached file_id is an expected miss, not an error.
	gormLog := gormlogger.NewSlogLogger(slog.With("service", "gorm"), gormlogger.Config{
		LogLevel:                  gormlogger.Warn,
		IgnoreRecordNotFoundError: true,
	})
	db, err := gorm.Open(postgres.Open(cfg.Postgres.Dsn), &gorm.Config{Logger: gormLog})
	if err != nil {
		return nil, fmt.Errorf("connect to postgres failed: %w", err)
	}
	return db, nil
}
