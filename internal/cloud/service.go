package cloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrNotFound = errors.New("not found")
var ErrForbidden = errors.New("forbidden")

// PushPublisher is deliberately small so event fan-out can be disabled or faked.
type PushPublisher interface {
	Publish(context.Context, NotificationEvent) error
}

type FanoutPublisher []PushPublisher

func (p FanoutPublisher) Publish(ctx context.Context, event NotificationEvent) error {
	var firstErr error
	for _, publisher := range p {
		if publisher == nil {
			continue
		}
		if err := publisher.Publish(ctx, event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type NotificationEvent struct {
	EventID        string          `json:"event_id"`
	Timestamp      time.Time       `json:"timestamp"`
	AccountID      string          `json:"account_id"`
	DaemonID       string          `json:"daemon_id"`
	NotificationID string          `json:"notification_id"`
	Kind           string          `json:"kind"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

func (s *Service) daemonForAccount(ctx context.Context, accountID, id string) (database.Daemon, error) {
	var d database.Daemon
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return d, ErrNotFound
		}
		return d, err
	}
	if d.AccountID != accountID {
		return database.Daemon{}, ErrForbidden
	}
	return d, nil
}

type Service struct {
	db        *database.DB
	publisher PushPublisher
}

func NewService(db *database.DB, publisher PushPublisher) *Service {
	return &Service{db: db, publisher: publisher}
}

type DaemonView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}
type Credential struct {
	DaemonView
	Secret string `json:"secret"`
}
type NotificationView struct {
	ID        string         `json:"id"`
	AccountID string         `json:"account_id"`
	DaemonID  string         `json:"daemon_id"`
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Metadata  datatypes.JSON `json:"metadata"`
	ReadAt    *time.Time     `json:"read_at"`
	CreatedAt time.Time      `json:"created_at"`
}
type MetricInput struct {
	SentAt             time.Time `json:"sent_at"`
	UptimeSeconds      int64     `json:"uptime_seconds"`
	ProcessMemoryBytes int64     `json:"process_memory_bytes"`
	CPUPercent         float64   `json:"cpu_percent"`
	MemoryUsedPercent  float64   `json:"memory_used_percent"`
	MemoryUsedBytes    uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64    `json:"memory_total_bytes"`
	WebhookExecutions  uint64    `json:"webhook_executions"`
	WebhookFailures    uint64    `json:"webhook_failures"`
}
type MetricView struct {
	ID                 string    `json:"id"`
	DaemonID           string    `json:"daemon_id"`
	SentAt             time.Time `json:"sent_at"`
	ReceivedAt         time.Time `json:"received_at"`
	UptimeSeconds      int64     `json:"uptime_seconds"`
	ProcessMemoryBytes int64     `json:"process_memory_bytes"`
	CPUPercent         float64   `json:"cpu_percent"`
	MemoryUsedPercent  float64   `json:"memory_used_percent"`
	MemoryUsedBytes    uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64    `json:"memory_total_bytes"`
	WebhookExecutions  uint64    `json:"webhook_executions"`
	WebhookFailures    uint64    `json:"webhook_failures"`
}
type AlarmView struct {
	ID              string     `json:"id"`
	DaemonID        string     `json:"daemon_id"`
	Kind            string     `json:"kind"`
	Threshold       float64    `json:"threshold"`
	Enabled         bool       `json:"enabled"`
	CooldownSeconds int        `json:"cooldown_seconds"`
	LastTriggeredAt *time.Time `json:"last_triggered_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}
type AlarmInput struct {
	Kind            string  `json:"kind"`
	Threshold       float64 `json:"threshold"`
	Enabled         bool    `json:"enabled"`
	CooldownSeconds int     `json:"cooldown_seconds"`
}
type NotificationInput struct {
	Kind     string          `json:"kind"`
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Metadata json.RawMessage `json:"metadata"`
}

func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
func viewDaemon(d database.Daemon) DaemonView {
	return DaemonView{ID: d.ID, Name: d.Name, Enabled: d.Enabled, LastSeenAt: d.LastSeenAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}
func (s *Service) CreateDaemon(ctx context.Context, accountID, name string) (Credential, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Credential{}, fmt.Errorf("name is required")
	}
	secret, err := generateSecret()
	if err != nil {
		return Credential{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return Credential{}, err
	}
	d := database.Daemon{ID: uuid.NewString(), AccountID: accountID, Name: name, SecretHash: string(hash), Enabled: true}
	if err := s.db.WithContext(ctx).Create(&d).Error; err != nil {
		return Credential{}, err
	}
	return Credential{DaemonView: viewDaemon(d), Secret: secret}, nil
}
func (s *Service) ListDaemons(ctx context.Context, accountID string) ([]DaemonView, error) {
	var rows []database.Daemon
	if err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("created_at desc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]DaemonView, len(rows))
	for i := range rows {
		out[i] = viewDaemon(rows[i])
	}
	return out, nil
}
func (s *Service) ListMetrics(ctx context.Context, accountID, daemonID string, limit int, before *time.Time) ([]MetricView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	query := s.db.WithContext(ctx).Where("daemon_id = ?", daemonID)
	if before != nil {
		query = query.Where("sent_at < ?", before.UTC())
	}
	var rows []database.DaemonMetric
	if err := query.Order("sent_at desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MetricView, len(rows))
	for i, row := range rows {
		out[i] = MetricView{ID: row.ID, DaemonID: row.DaemonID, SentAt: row.SentAt, ReceivedAt: row.ReceivedAt, UptimeSeconds: row.UptimeSeconds, ProcessMemoryBytes: row.ProcessMemoryBytes, CPUPercent: row.CPUPercent, MemoryUsedPercent: row.MemoryUsedPercent, MemoryUsedBytes: row.MemoryUsedBytes, MemoryTotalBytes: row.MemoryTotalBytes, WebhookExecutions: row.WebhookExecutions, WebhookFailures: row.WebhookFailures}
	}
	return out, nil
}
func viewAlarm(row database.DaemonAlarm) AlarmView {
	return AlarmView{ID: row.ID, DaemonID: row.DaemonID, Kind: row.Kind, Threshold: row.Threshold, Enabled: row.Enabled, CooldownSeconds: row.CooldownSeconds, LastTriggeredAt: row.LastTriggeredAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
func (s *Service) ListAlarms(ctx context.Context, accountID, daemonID string) ([]AlarmView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return nil, err
	}
	var rows []database.DaemonAlarm
	if err := s.db.WithContext(ctx).Where("daemon_id = ?", daemonID).Order("created_at asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]AlarmView, len(rows))
	for i := range rows {
		out[i] = viewAlarm(rows[i])
	}
	return out, nil
}
func (s *Service) SetAlarm(ctx context.Context, accountID, daemonID string, input AlarmInput) (AlarmView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return AlarmView{}, err
	}
	input.Kind = strings.TrimSpace(input.Kind)
	if input.Kind != "cpu_percent" && input.Kind != "memory_used_percent" {
		return AlarmView{}, fmt.Errorf("kind must be cpu_percent or memory_used_percent")
	}
	if input.Threshold <= 0 || input.Threshold > 100 {
		return AlarmView{}, fmt.Errorf("threshold must be between 0 and 100")
	}
	if input.CooldownSeconds <= 0 {
		input.CooldownSeconds = 300
	}
	var row database.DaemonAlarm
	err := s.db.WithContext(ctx).Where("daemon_id = ? AND kind = ?", daemonID, input.Kind).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = database.DaemonAlarm{ID: uuid.NewString(), DaemonID: daemonID, Kind: input.Kind}
	} else if err != nil {
		return AlarmView{}, err
	}
	row.Threshold, row.Enabled, row.CooldownSeconds = input.Threshold, input.Enabled, input.CooldownSeconds
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return AlarmView{}, err
	}
	return viewAlarm(row), nil
}
func (s *Service) DeleteAlarm(ctx context.Context, accountID, daemonID, alarmID string) error {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return err
	}
	result := s.db.WithContext(ctx).Where("id = ? AND daemon_id = ?", alarmID, daemonID).Delete(&database.DaemonAlarm{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) GetDaemon(ctx context.Context, accountID, id string) (DaemonView, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil {
		return DaemonView{}, err
	}
	return viewDaemon(d), nil
}
func (s *Service) UpdateDaemon(ctx context.Context, accountID, id string, name *string, enabled *bool) (DaemonView, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil {
		return DaemonView{}, err
	}
	if name != nil {
		d.Name = strings.TrimSpace(*name)
		if d.Name == "" {
			return DaemonView{}, fmt.Errorf("name is required")
		}
	}
	if enabled != nil {
		d.Enabled = *enabled
	}
	if err := s.db.WithContext(ctx).Save(&d).Error; err != nil {
		return DaemonView{}, err
	}
	return viewDaemon(d), nil
}
func (s *Service) RotateSecret(ctx context.Context, accountID, id string) (string, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil {
		return "", err
	}
	secret, err := generateSecret()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	d.SecretHash = string(hash)
	if err := s.db.WithContext(ctx).Save(&d).Error; err != nil {
		return "", err
	}
	return secret, nil
}
func (s *Service) DisableDaemon(ctx context.Context, accountID, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var d database.Daemon
		if err := tx.Where("id = ?", id).First(&d).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if d.AccountID != accountID {
			return ErrForbidden
		}
		d.Enabled = false
		return tx.Save(&d).Error
	})
}
func (s *Service) authenticateDaemon(ctx context.Context, id, secret string) (database.Daemon, error) {
	var d database.Daemon
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&d).Error; err != nil {
		return d, ErrUnauthorized
	}
	if !d.Enabled || bcrypt.CompareHashAndPassword([]byte(d.SecretHash), []byte(secret)) != nil {
		return database.Daemon{}, ErrUnauthorized
	}
	return d, nil
}
func (s *Service) IngestMetric(ctx context.Context, id, secret string, input MetricInput) error {
	d, err := s.authenticateDaemon(ctx, id, secret)
	if err != nil {
		return ErrUnauthorized
	}
	if input.SentAt.IsZero() || input.UptimeSeconds < 0 || input.ProcessMemoryBytes < 0 ||
		input.CPUPercent < 0 || input.CPUPercent > 100 ||
		input.MemoryUsedPercent < 0 || input.MemoryUsedPercent > 100 {
		return fmt.Errorf("invalid metric")
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Create(&database.DaemonMetric{
		ID: uuid.NewString(), DaemonID: d.ID, SentAt: input.SentAt.UTC(), ReceivedAt: now,
		UptimeSeconds: input.UptimeSeconds, ProcessMemoryBytes: input.ProcessMemoryBytes,
		CPUPercent: input.CPUPercent, MemoryUsedPercent: input.MemoryUsedPercent,
		MemoryUsedBytes: input.MemoryUsedBytes, MemoryTotalBytes: input.MemoryTotalBytes,
		WebhookExecutions: input.WebhookExecutions, WebhookFailures: input.WebhookFailures,
	}).Error; err != nil {
		return err
	}
	if err := s.evaluateAlarms(ctx, d, input, now); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", d.ID).Updates(map[string]any{"last_seen_at": now, "updated_at": now}).Error
}
func (s *Service) evaluateAlarms(ctx context.Context, daemon database.Daemon, input MetricInput, now time.Time) error {
	var alarms []database.DaemonAlarm
	if err := s.db.WithContext(ctx).Where("daemon_id = ? AND enabled = ?", daemon.ID, true).Find(&alarms).Error; err != nil {
		return err
	}
	for i := range alarms {
		alarm := &alarms[i]
		value := input.CPUPercent
		if alarm.Kind == "memory_used_percent" {
			value = input.MemoryUsedPercent
		}
		if value < alarm.Threshold ||
			(alarm.LastTriggeredAt != nil && now.Sub(*alarm.LastTriggeredAt) < time.Duration(alarm.CooldownSeconds)*time.Second) {
			continue
		}
		notification := database.Notification{
			ID: uuid.NewString(), AccountID: daemon.AccountID, DaemonID: daemon.ID,
			Kind:      "daemon.alarm." + alarm.Kind,
			Title:     fmt.Sprintf("%s threshold exceeded", alarm.Kind),
			Body:      fmt.Sprintf("%s reached %.2f%% (threshold %.2f%%)", alarm.Kind, value, alarm.Threshold),
			Metadata:  datatypes.JSON(fmt.Sprintf(`{"value":%.4f,"threshold":%.4f}`, value, alarm.Threshold)),
			CreatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&notification).Error; err != nil {
			return err
		}
		alarm.LastTriggeredAt = &now
		if err := s.db.WithContext(ctx).Save(alarm).Error; err != nil {
			return err
		}
		s.publishNotification(ctx, notification)
	}
	return nil
}
func (s *Service) publishNotification(ctx context.Context, notification database.Notification) {
	if s.publisher == nil {
		return
	}
	event := NotificationEvent{
		EventID:        uuid.NewString(),
		Timestamp:      notification.CreatedAt,
		AccountID:      notification.AccountID,
		DaemonID:       notification.DaemonID,
		NotificationID: notification.ID,
		Kind:           notification.Kind,
		Title:          notification.Title,
		Body:           notification.Body,
		Metadata:       json.RawMessage(notification.Metadata),
	}
	_ = s.publisher.Publish(ctx, event)
}
func (s *Service) CreatePushNotification(ctx context.Context, accountID, daemonID string, input NotificationInput) (NotificationView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return NotificationView{}, err
	}
	kind, err := bound(input.Kind, 128)
	if err != nil {
		return NotificationView{}, fmt.Errorf("kind: %w", err)
	}
	title, err := bound(input.Title, 128)
	if err != nil {
		return NotificationView{}, fmt.Errorf("title: %w", err)
	}
	body := strings.TrimSpace(input.Body)
	if body == "" || len(body) > 4096 || !utf8.ValidString(body) {
		return NotificationView{}, fmt.Errorf("body is required and must be at most 4096 bytes")
	}
	metadata := input.Metadata
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	var decoded any
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		return NotificationView{}, fmt.Errorf("metadata: invalid json")
	}
	normalized, err := json.Marshal(decoded)
	if err != nil || len(normalized) > 16*1024 {
		return NotificationView{}, fmt.Errorf("metadata exceeds bounds")
	}
	row := database.Notification{ID: uuid.NewString(), AccountID: accountID, DaemonID: daemonID, Kind: kind, Title: title, Body: body, Metadata: datatypes.JSON(normalized), CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return NotificationView{}, err
	}
	s.publishNotification(ctx, row)
	return notificationView(row), nil
}
func bound(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return "", fmt.Errorf("value exceeds bounds or is empty")
	}
	return value, nil
}
func (s *Service) CreateNotification(ctx context.Context, id, secret string, input NotificationInput) (NotificationView, error) {
	d, err := s.authenticateDaemon(ctx, id, secret)
	if err != nil {
		return NotificationView{}, ErrUnauthorized
	}
	kind, err := bound(input.Kind, 128)
	if err != nil {
		return NotificationView{}, fmt.Errorf("kind: %w", err)
	}
	title, err := bound(input.Title, 128)
	if err != nil {
		return NotificationView{}, fmt.Errorf("title: %w", err)
	}
	if len(input.Body) > 4096 || !utf8.ValidString(input.Body) {
		return NotificationView{}, fmt.Errorf("body exceeds bounds")
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return NotificationView{}, fmt.Errorf("body is required")
	}
	metadata := input.Metadata
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	var decoded any
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		return NotificationView{}, fmt.Errorf("metadata: invalid json")
	}
	metadata, err = json.Marshal(decoded)
	if err != nil || len(metadata) > 16*1024 {
		return NotificationView{}, fmt.Errorf("metadata exceeds bounds")
	}
	n := database.Notification{ID: uuid.NewString(), AccountID: d.AccountID, DaemonID: d.ID, Kind: kind, Title: title, Body: body, Metadata: datatypes.JSON(metadata), CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&n).Error; err != nil {
		return NotificationView{}, err
	}
	s.publishNotification(ctx, n)
	return notificationView(n), nil
}
func notificationView(n database.Notification) NotificationView {
	return NotificationView{ID: n.ID, AccountID: n.AccountID, DaemonID: n.DaemonID, Kind: n.Kind, Title: n.Title, Body: n.Body, Metadata: n.Metadata, ReadAt: n.ReadAt, CreatedAt: n.CreatedAt}
}
func (s *Service) ListNotifications(ctx context.Context, accountID string, unread bool, daemonID string, limit int, before *time.Time) ([]NotificationView, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Where("account_id = ?", accountID)
	if unread {
		q = q.Where("read_at IS NULL")
	}
	if daemonID != "" {
		q = q.Where("daemon_id = ?", daemonID)
	}
	if before != nil {
		q = q.Where("created_at < ?", before.UTC())
	}
	var rows []database.Notification
	if err := q.Order("created_at desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]NotificationView, len(rows))
	for i := range rows {
		out[i] = notificationView(rows[i])
	}
	return out, nil
}
func (s *Service) MarkNotificationRead(ctx context.Context, accountID, id string) error {
	var n database.Notification
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if n.AccountID != accountID {
		return ErrForbidden
	}
	if n.ReadAt == nil {
		now := time.Now().UTC()
		if err := s.db.WithContext(ctx).Model(&n).Update("read_at", now).Error; err != nil {
			return err
		}
	}
	return nil
}

// Webhook relay: clients enqueue signed webhook invocations through the
// cloud; daemons poll for them and post results back. Polling only — no
// long-lived connections or push channels.

const (
	webhookStatusPending = "pending"
	webhookStatusLeased  = "leased"
	webhookStatusDone    = "done"

	webhookLeaseDuration = 2 * time.Minute
	webhookBodyLimit     = 256 * 1024
	webhookPendingLimit  = 50
)

type WebhookRequestView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Signature   string    `json:"signature"`
	Status      string    `json:"status"`
	ResultCode  int       `json:"result_code,omitempty"`
	ResultBody  string    `json:"result_body,omitempty"`
	ResultError string    `json:"result_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func viewWebhookRequest(row database.WebhookRequest) WebhookRequestView {
	return WebhookRequestView{
		ID:          row.ID,
		Name:        row.Name,
		Body:        base64.StdEncoding.EncodeToString(row.Body),
		Signature:   row.Signature,
		Status:      row.Status,
		ResultCode:  row.ResultCode,
		ResultBody:  base64.StdEncoding.EncodeToString(row.ResultBody),
		ResultError: row.ResultError,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

// EnqueueWebhook queues a signed webhook invocation for [daemonID].
func (s *Service) EnqueueWebhook(ctx context.Context, accountID, daemonID, name string, body []byte, signature string) (WebhookRequestView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return WebhookRequestView{}, err
	}
	if strings.TrimSpace(name) == "" || len(body) == 0 || len(body) > webhookBodyLimit || strings.TrimSpace(signature) == "" {
		return WebhookRequestView{}, errors.New("webhook request requires a name, body and signature")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return WebhookRequestView{}, err
	}
	now := time.Now().UTC()
	row := database.WebhookRequest{
		ID:        id.String(),
		DaemonID:  daemonID,
		Name:      strings.TrimSpace(name),
		Body:      body,
		Signature: strings.TrimSpace(signature),
		Status:    webhookStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return WebhookRequestView{}, err
	}
	return viewWebhookRequest(row), nil
}

// ListPendingWebhooks leases and returns up to [limit] pending webhook
// invocations for the daemon. Leases older than webhookLeaseDuration are
// reclaimed first so a daemon that died mid-execution does not lose requests.
func (s *Service) ListPendingWebhooks(ctx context.Context, daemonID, secret string, limit int) ([]WebhookRequestView, error) {
	if _, err := s.authenticateDaemon(ctx, daemonID, secret); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > webhookPendingLimit {
		limit = webhookPendingLimit
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&database.WebhookRequest{}).
		Where("daemon_id = ? AND status = ? AND leased_at < ?", daemonID, webhookStatusLeased, now.Add(-webhookLeaseDuration)).
		Updates(map[string]any{"status": webhookStatusPending, "leased_at": nil, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	var rows []database.WebhookRequest
	if err := s.db.WithContext(ctx).Where("daemon_id = ? AND status = ?", daemonID, webhookStatusPending).
		Order("created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []WebhookRequestView{}, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if err := s.db.WithContext(ctx).Model(&database.WebhookRequest{}).
		Where("daemon_id = ? AND id IN ?", daemonID, ids).
		Updates(map[string]any{"status": webhookStatusLeased, "leased_at": now, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	views := make([]WebhookRequestView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewWebhookRequest(row))
	}
	return views, nil
}

// CompleteWebhook stores the execution result for a leased request.
func (s *Service) CompleteWebhook(ctx context.Context, daemonID, secret, requestID string, code int, body []byte, resultError string) error {
	if _, err := s.authenticateDaemon(ctx, daemonID, secret); err != nil {
		return err
	}
	if len(resultError) > 512 {
		resultError = resultError[:512]
	}
	res := s.db.WithContext(ctx).Model(&database.WebhookRequest{}).
		Where("daemon_id = ? AND id = ? AND status = ?", daemonID, requestID, webhookStatusLeased).
		Updates(map[string]any{
			"status": webhookStatusDone, "result_code": code,
			"result_body": body, "result_error": resultError,
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWebhookResult returns a relayed webhook request (and its result once the
// daemon completed it) to the owning account.
func (s *Service) GetWebhookResult(ctx context.Context, accountID, daemonID, requestID string) (WebhookRequestView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return WebhookRequestView{}, err
	}
	var row database.WebhookRequest
	if err := s.db.WithContext(ctx).Where("daemon_id = ? AND id = ?", daemonID, requestID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return WebhookRequestView{}, ErrNotFound
		}
		return WebhookRequestView{}, err
	}
	return viewWebhookRequest(row), nil
}
