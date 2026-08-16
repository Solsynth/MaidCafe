package database

import (
	"time"

	"gorm.io/datatypes"
)

type Daemon struct {
	ID          string `gorm:"type:char(36);primaryKey"`
	AccountID   string `gorm:"size:191;index;not null"`
	WorkspaceID string `gorm:"size:191;index;not null"`
	Name        string `gorm:"size:191;not null"`
	SecretHash  string `gorm:"size:255;not null" json:"-"`
	Enabled     bool   `gorm:"not null;index"`
	LastSeenAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type DaemonMetric struct {
	ID                 string `gorm:"type:char(36);primaryKey"`
	DaemonID           string `gorm:"size:191;index;not null"`
	SentAt             time.Time
	ReceivedAt         time.Time `gorm:"index;not null"`
	UptimeSeconds      int64
	ProcessMemoryBytes int64
	CPUPercent         float64
	CPUCount           int
	Load1              float64
	Load5              float64
	Load15             float64
	MemoryUsedPercent  float64
	MemoryUsedBytes    uint64
	MemoryTotalBytes   uint64
	SwapTotalKb        int64
	SwapFreeKb         int64
	DiskTotalKb        int64
	DiskAvailableKb    int64
	NetRxBytes         uint64
	NetTxBytes         uint64
	WebhookExecutions  uint64
	WebhookFailures    uint64
}

type Notification struct {
	ID          string         `gorm:"type:char(36);primaryKey"`
	AccountID   string         `gorm:"size:191;index;not null"`
	WorkspaceID string         `gorm:"size:191;index;not null"`
	DaemonID    string         `gorm:"size:191;index;not null"`
	Kind      string         `gorm:"size:128;not null"`
	Title     string         `gorm:"size:128;not null"`
	Body      string         `gorm:"size:4096;not null"`
	Metadata  datatypes.JSON `gorm:"type:json"`
	ReadAt    *time.Time
	CreatedAt time.Time
}

// WebhookRequest is a webhook invocation queued through the MaidKit cloud
// relay. The daemon polls for pending requests, verifies the HMAC signature
// against its own config, executes the webhook and stores the result here.
type WebhookRequest struct {
	ID          string `gorm:"type:char(36);primaryKey"`
	DaemonID    string `gorm:"size:191;index;not null"`
	Name        string `gorm:"size:128;not null"`
	Body        []byte `gorm:"type:bytea;not null"`
	Signature   string `gorm:"size:128;not null"`
	Status      string `gorm:"size:16;not null;index"`
	LeasedAt    *time.Time
	ResultCode  int
	ResultBody  []byte `gorm:"type:bytea"`
	ResultError string `gorm:"size:512"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
