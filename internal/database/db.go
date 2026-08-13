package database

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type DB struct {
	*gorm.DB
	sqlDB *sql.DB
}

func Open(cfg *config.Config) (*DB, error) {
	if cfg == nil || strings.TrimSpace(cfg.Database.DSN) == "" {
		return nil, fmt.Errorf("database.dsn is required")
	}
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return openDB(db)
}

func NewSQLite() (*DB, error) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		return nil, err
	}
	return openDB(db)
}

func openDB(db *gorm.DB) (*DB, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	return &DB{DB: db, sqlDB: sqlDB}, nil
}

func (db *DB) AutoMigrate() error {
	if db == nil || db.DB == nil {
		return fmt.Errorf("database is nil")
	}
	return db.DB.AutoMigrate(&Daemon{}, &DaemonMetric{}, &DaemonAlarm{}, &Notification{})
}
func (db *DB) Close() error {
	if db == nil || db.sqlDB == nil {
		return nil
	}
	return db.sqlDB.Close()
}
