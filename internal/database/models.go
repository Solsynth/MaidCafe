package database

import (
	"time"

	"gorm.io/datatypes"
)

type Daemon struct {
	ID         string `gorm:"type:char(36);primaryKey"`
	AccountID  string `gorm:"size:191;index;not null"`
	Name       string `gorm:"size:191;not null"`
	SecretHash string `gorm:"size:255;not null" json:"-"`
	Enabled    bool   `gorm:"not null;index"`
	LastSeenAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type DaemonMetric struct {
	ID                 string `gorm:"type:char(36);primaryKey"`
	DaemonID           string `gorm:"size:191;index;not null"`
	SentAt             time.Time
	ReceivedAt         time.Time `gorm:"index;not null"`
	UptimeSeconds      int64
	ProcessMemoryBytes int64
	CPUPercent         float64
	MemoryUsedPercent  float64
	MemoryUsedBytes    uint64
	MemoryTotalBytes   uint64
	WebhookExecutions  uint64
	WebhookFailures    uint64
}

type DaemonAlarm struct {
	ID              string  `gorm:"type:char(36);primaryKey"`
	DaemonID        string  `gorm:"size:191;index;not null"`
	Kind            string  `gorm:"size:64;not null"`
	Threshold       float64 `gorm:"not null"`
	Enabled         bool    `gorm:"not null;index"`
	CooldownSeconds int     `gorm:"not null"`
	LastTriggeredAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Notification struct {
	ID        string         `gorm:"type:char(36);primaryKey"`
	AccountID string         `gorm:"size:191;index;not null"`
	DaemonID  string         `gorm:"size:191;index;not null"`
	Kind      string         `gorm:"size:128;not null"`
	Title     string         `gorm:"size:128;not null"`
	Body      string         `gorm:"size:4096;not null"`
	Metadata  datatypes.JSON `gorm:"type:json"`
	ReadAt    *time.Time
	CreatedAt time.Time
}
