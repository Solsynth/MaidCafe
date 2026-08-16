package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrNotFound = errors.New("not found")
var ErrForbidden = errors.New("forbidden")

// workspaceMemberRole is the minimum DyWorkspace member role level required
// to manage daemons inside a workspace. Role levels follow the workspace
// contract in ../SolarNetwork/Spec/proto/workspace.proto:
// Owner=100, Admin=75, Member=50, Viewer=25.
const workspaceMemberRole int32 = 50

// WorkspaceClient is the small slice of the DyWorkspaceService gRPC surface
// MaidCafe needs. Keeping it an interface lets the service run against a fake
// in tests while the production implementation talks to the workspace service
// through the DysonGo SDK (src.solsynth.dev/sosys/go/proto).
type WorkspaceClient interface {
	IsMemberWithRole(ctx context.Context, workspaceID, accountID string, requiredRoles []int32) (bool, error)
}

// GrpcWorkspaceClient adapts the generated DyWorkspaceServiceClient to
// WorkspaceClient.
type GrpcWorkspaceClient struct {
	client gen.DyWorkspaceServiceClient
}

func (c GrpcWorkspaceClient) IsMemberWithRole(ctx context.Context, workspaceID, accountID string, requiredRoles []int32) (bool, error) {
	if c.client == nil {
		return false, errors.New("workspace client is not configured")
	}
	resp, err := c.client.IsMemberWithRole(ctx, &gen.DyIsWorkspaceMemberWithRoleRequest{
		WorkspaceId:   workspaceID,
		AccountId:     accountID,
		RequiredRoles: requiredRoles,
	})
	if err != nil {
		return false, err
	}
	return resp.GetValue(), nil
}

// NewWorkspaceClient dials the DyWorkspaceService gRPC endpoint hosted by the
// workspace service using the same TLS conventions as the auth client.
func NewWorkspaceClient(cfg config.WorkspaceConfig) (WorkspaceClient, *grpc.ClientConn, error) {
	target, useTLS := dyauth.NormalizeAuthGRPCTarget(cfg.Target, cfg.UseTLS)
	if strings.TrimSpace(target) == "" {
		return nil, nil, errors.New("workspace gRPC target is empty")
	}
	var transportCredentials credentials.TransportCredentials
	if useTLS {
		transportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSSkipVerify})
	} else {
		transportCredentials = insecure.NewCredentials()
	}
	conn, err := grpc.Dial(target, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, nil, fmt.Errorf("dial workspace service: %w", err)
	}
	return GrpcWorkspaceClient{client: gen.NewDyWorkspaceServiceClient(conn)}, conn, nil
}

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
	WorkspaceID    string          `json:"workspace_id"`
	DaemonID       string          `json:"daemon_id"`
	NotificationID string          `json:"notification_id"`
	Kind           string          `json:"kind"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// daemonForAccount loads a daemon and verifies the account may access it.
// Access is granted through workspace membership: the daemon belongs to a
// workspace and every member (role >= Member) of that workspace can manage it.
func (s *Service) daemonForAccount(ctx context.Context, accountID, id string) (database.Daemon, error) {
	var d database.Daemon
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return d, ErrNotFound
		}
		return d, err
	}
	if err := s.authorizeWorkspace(ctx, accountID, d.WorkspaceID); err != nil {
		return database.Daemon{}, err
	}
	return d, nil
}

// authorizeWorkspace fails closed: only a confirmed workspace member with the
// member role level (or higher) passes. An unreachable workspace service is an
// error, never a grant.
func (s *Service) authorizeWorkspace(ctx context.Context, accountID, workspaceID string) error {
	if s.workspaces == nil {
		return ErrForbidden
	}
	member, err := s.workspaces.IsMemberWithRole(ctx, workspaceID, accountID, []int32{workspaceMemberRole})
	if err != nil {
		return fmt.Errorf("check workspace membership: %w", err)
	}
	if !member {
		return ErrForbidden
	}
	return nil
}

type Service struct {
	db         *database.DB
	publisher  PushPublisher
	workspaces WorkspaceClient
}

func NewService(db *database.DB, publisher PushPublisher, workspaces WorkspaceClient) *Service {
	return &Service{db: db, publisher: publisher, workspaces: workspaces}
}

type DaemonView struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	HostID      string     `json:"host_id"`
	Enabled     bool       `json:"enabled"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
type Credential struct {
	DaemonView
	Secret string `json:"secret"`
}
type NotificationView struct {
	ID          string         `json:"id"`
	AccountID   string         `json:"account_id"`
	WorkspaceID string         `json:"workspace_id"`
	DaemonID    string         `json:"daemon_id"`
	Kind        string         `json:"kind"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Metadata    datatypes.JSON `json:"metadata"`
	ReadAt      *time.Time     `json:"read_at"`
	CreatedAt   time.Time      `json:"created_at"`
}
type MetricInput struct {
	SentAt             time.Time `json:"sent_at"`
	HostID             string    `json:"host_id,omitempty"`
	UptimeSeconds      int64     `json:"uptime_seconds"`
	ProcessMemoryBytes int64     `json:"process_memory_bytes"`
	CPUPercent         float64   `json:"cpu_percent"`
	CPUCount           int       `json:"cpu_count"`
	Load1              float64   `json:"load1"`
	Load5              float64   `json:"load5"`
	Load15             float64   `json:"load15"`
	MemoryUsedPercent  float64   `json:"memory_used_percent"`
	MemoryUsedBytes    uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64    `json:"memory_total_bytes"`
	SwapTotalKb        int64     `json:"swap_total_kb"`
	SwapFreeKb         int64     `json:"swap_free_kb"`
	DiskTotalKb        int64     `json:"disk_total_kb"`
	DiskAvailableKb    int64     `json:"disk_available_kb"`
	NetRxBytes         uint64    `json:"net_rx_bytes"`
	NetTxBytes         uint64    `json:"net_tx_bytes"`
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
	CPUCount           int       `json:"cpu_count"`
	Load1              float64   `json:"load1"`
	Load5              float64   `json:"load5"`
	Load15             float64   `json:"load15"`
	MemoryUsedPercent  float64   `json:"memory_used_percent"`
	MemoryUsedBytes    uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64    `json:"memory_total_bytes"`
	SwapTotalKb        int64     `json:"swap_total_kb"`
	SwapFreeKb         int64     `json:"swap_free_kb"`
	DiskTotalKb        int64     `json:"disk_total_kb"`
	DiskAvailableKb    int64     `json:"disk_available_kb"`
	NetRxBytes         uint64    `json:"net_rx_bytes"`
	NetTxBytes         uint64    `json:"net_tx_bytes"`
	WebhookExecutions  uint64    `json:"webhook_executions"`
	WebhookFailures    uint64    `json:"webhook_failures"`
}
type NotificationInput struct {
	Kind     string          `json:"kind"`
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Metadata json.RawMessage `json:"metadata"`
}

// ActionInput is one action the daemon reports for cloud-side listing. The
// script body and any secret stay on the host; invocation happens through
// the webhook relay.
type ActionInput struct {
	Name            string `json:"name"`
	DisplayName     string `json:"display_name,omitempty"`
	Enabled         bool   `json:"enabled"`
	NotifyOnSuccess bool   `json:"notify_on_success"`
	NotifyOnFailure bool   `json:"notify_on_failure"`
	Timeout         string `json:"timeout,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	User            string `json:"user,omitempty"`
}

type ActionView struct {
	Name            string    `json:"name"`
	DisplayName     string    `json:"display_name"`
	Enabled         bool      `json:"enabled"`
	NotifyOnSuccess bool      `json:"notify_on_success"`
	NotifyOnFailure bool      `json:"notify_on_failure"`
	Timeout         string    `json:"timeout"`
	Cwd             string    `json:"cwd"`
	User            string    `json:"user"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
func viewDaemon(d database.Daemon) DaemonView {
	return DaemonView{ID: d.ID, WorkspaceID: d.WorkspaceID, Name: d.Name, HostID: d.HostID, Enabled: d.Enabled, LastSeenAt: d.LastSeenAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}
func (s *Service) CreateDaemon(ctx context.Context, accountID, workspaceID, name string) (Credential, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return Credential{}, fmt.Errorf("workspace_id is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Credential{}, fmt.Errorf("name is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return Credential{}, err
	}
	secret, err := generateSecret()
	if err != nil {
		return Credential{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return Credential{}, err
	}
	d := database.Daemon{ID: uuid.NewString(), AccountID: accountID, WorkspaceID: workspaceID, Name: name, SecretHash: string(hash), Enabled: true}
	if err := s.db.WithContext(ctx).Create(&d).Error; err != nil {
		return Credential{}, err
	}
	return Credential{DaemonView: viewDaemon(d), Secret: secret}, nil
}
func (s *Service) ListDaemons(ctx context.Context, accountID, workspaceID string) ([]DaemonView, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return nil, err
	}
	var rows []database.Daemon
	if err := s.db.WithContext(ctx).Where("workspace_id = ?", workspaceID).Order("created_at desc").Find(&rows).Error; err != nil {
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
		out[i] = MetricView{ID: row.ID, DaemonID: row.DaemonID, SentAt: row.SentAt, ReceivedAt: row.ReceivedAt, UptimeSeconds: row.UptimeSeconds, ProcessMemoryBytes: row.ProcessMemoryBytes, CPUPercent: row.CPUPercent, CPUCount: row.CPUCount, Load1: row.Load1, Load5: row.Load5, Load15: row.Load15, MemoryUsedPercent: row.MemoryUsedPercent, MemoryUsedBytes: row.MemoryUsedBytes, MemoryTotalBytes: row.MemoryTotalBytes, SwapTotalKb: row.SwapTotalKb, SwapFreeKb: row.SwapFreeKb, DiskTotalKb: row.DiskTotalKb, DiskAvailableKb: row.DiskAvailableKb, NetRxBytes: row.NetRxBytes, NetTxBytes: row.NetTxBytes, WebhookExecutions: row.WebhookExecutions, WebhookFailures: row.WebhookFailures}
	}
	return out, nil
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
		if err := s.authorizeWorkspace(ctx, accountID, d.WorkspaceID); err != nil {
			return err
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
// SyncActions replaces the action list the daemon reported for cloud-side
// listing. Authenticated with the daemon secret, like metric ingest.
func (s *Service) SyncActions(ctx context.Context, id, secret string, input []ActionInput) error {
	d, err := s.authenticateDaemon(ctx, id, secret)
	if err != nil {
		return ErrUnauthorized
	}
	names := make(map[string]struct{}, len(input))
	for i := range input {
		name := strings.TrimSpace(input[i].Name)
		if name == "" || len(name) > 128 || !utf8.ValidString(name) {
			return fmt.Errorf("action name exceeds bounds or is empty")
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("duplicate action name %q", name)
		}
		names[name] = struct{}{}
		input[i].Name = name
		input[i].DisplayName = strings.TrimSpace(input[i].DisplayName)
		if len(input[i].DisplayName) > 128 || len(input[i].Timeout) > 32 ||
			len(input[i].Cwd) > 1024 || len(input[i].User) > 64 {
			return fmt.Errorf("action field exceeds bounds")
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("daemon_id = ?", d.ID).Delete(&database.DaemonAction{}).Error; err != nil {
			return err
		}
		if len(input) == 0 {
			return nil
		}
		now := time.Now().UTC()
		rows := make([]database.DaemonAction, 0, len(input))
		for _, action := range input {
			rows = append(rows, database.DaemonAction{
				DaemonID: d.ID, Name: action.Name, DisplayName: action.DisplayName,
				Enabled: action.Enabled, NotifyOnSuccess: action.NotifyOnSuccess,
				NotifyOnFailure: action.NotifyOnFailure, Timeout: action.Timeout,
				Cwd: action.Cwd, User: action.User, UpdatedAt: now,
			})
		}
		return tx.Create(&rows).Error
	})
}

// ListActions returns the actions the daemon reported, for the cloud page.
func (s *Service) ListActions(ctx context.Context, accountID, daemonID string) ([]ActionView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return nil, err
	}
	var rows []database.DaemonAction
	if err := s.db.WithContext(ctx).Where("daemon_id = ?", daemonID).Order("name asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ActionView, len(rows))
	for i, row := range rows {
		out[i] = ActionView{Name: row.Name, DisplayName: row.DisplayName, Enabled: row.Enabled, NotifyOnSuccess: row.NotifyOnSuccess, NotifyOnFailure: row.NotifyOnFailure, Timeout: row.Timeout, Cwd: row.Cwd, User: row.User, UpdatedAt: row.UpdatedAt}
	}
	return out, nil
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
		CPUPercent: input.CPUPercent, CPUCount: input.CPUCount,
		Load1: input.Load1, Load5: input.Load5, Load15: input.Load15,
		MemoryUsedPercent: input.MemoryUsedPercent,
		MemoryUsedBytes: input.MemoryUsedBytes, MemoryTotalBytes: input.MemoryTotalBytes,
		SwapTotalKb: input.SwapTotalKb, SwapFreeKb: input.SwapFreeKb,
		DiskTotalKb: input.DiskTotalKb, DiskAvailableKb: input.DiskAvailableKb,
		NetRxBytes: input.NetRxBytes, NetTxBytes: input.NetTxBytes,
		WebhookExecutions: input.WebhookExecutions, WebhookFailures: input.WebhookFailures,
	}).Error; err != nil {
		return err
	}
	// Alarms are evaluated daemon-side: the daemon reports `daemon.alarm.*`
	// notifications through CreateNotification when a threshold is exceeded,
	// so the cloud only stores and forwards them.
	updates := map[string]any{"last_seen_at": now, "updated_at": now}
	if hostID := strings.TrimSpace(input.HostID); hostID != "" && hostID != d.HostID {
		updates["host_id"] = hostID
	}
	return s.db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", d.ID).Updates(updates).Error
}
func (s *Service) publishNotification(ctx context.Context, notification database.Notification) {
	if s.publisher == nil {
		return
	}
	event := NotificationEvent{
		EventID:        uuid.NewString(),
		Timestamp:      notification.CreatedAt,
		AccountID:      notification.AccountID,
		WorkspaceID:    notification.WorkspaceID,
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
	daemon, err := s.daemonForAccount(ctx, accountID, daemonID)
	if err != nil {
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
	row := database.Notification{ID: uuid.NewString(), AccountID: accountID, WorkspaceID: daemon.WorkspaceID, DaemonID: daemonID, Kind: kind, Title: title, Body: body, Metadata: datatypes.JSON(normalized), CreatedAt: time.Now().UTC()}
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
	n := database.Notification{ID: uuid.NewString(), AccountID: d.AccountID, WorkspaceID: d.WorkspaceID, DaemonID: d.ID, Kind: kind, Title: title, Body: body, Metadata: datatypes.JSON(metadata), CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&n).Error; err != nil {
		return NotificationView{}, err
	}
	s.publishNotification(ctx, n)
	return notificationView(n), nil
}
func notificationView(n database.Notification) NotificationView {
	return NotificationView{ID: n.ID, AccountID: n.AccountID, WorkspaceID: n.WorkspaceID, DaemonID: n.DaemonID, Kind: n.Kind, Title: n.Title, Body: n.Body, Metadata: n.Metadata, ReadAt: n.ReadAt, CreatedAt: n.CreatedAt}
}
func (s *Service) ListNotifications(ctx context.Context, accountID, workspaceID string, unread bool, daemonID string, limit int, before *time.Time) ([]NotificationView, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Where("workspace_id = ?", workspaceID)
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
	if err := s.authorizeWorkspace(ctx, accountID, n.WorkspaceID); err != nil {
		return err
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
	DaemonID    string    `json:"daemon_id"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Signature   string    `json:"signature"`
	InvokedBy   string    `json:"invoked_by"`
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
		DaemonID:    row.DaemonID,
		Name:        row.Name,
		Body:        base64.StdEncoding.EncodeToString(row.Body),
		Signature:   row.Signature,
		InvokedBy:   row.InvokedBy,
		Status:      row.Status,
		ResultCode:  row.ResultCode,
		ResultBody:  base64.StdEncoding.EncodeToString(row.ResultBody),
		ResultError: row.ResultError,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

// EnqueueWebhook queues a webhook or action invocation for [daemonID]. The
// signature is optional: webhooks carry their own secret that the daemon
// verifies at execution, while actions have no secret (the daemon runs them
// because the request arrived through its own cloud-authenticated poll), so
// the cloud only stores what it is given. [invokedBy] names the caller for
// the daemon's audit log; [credential] (nil for Solarpass users) restricts
// the invocation to the credential's scopes.
func (s *Service) EnqueueWebhook(ctx context.Context, accountID, daemonID, name string, body []byte, signature string, invokedBy string, credential *database.Credential) (WebhookRequestView, error) {
	if credential != nil {
		if err := s.authorizeCredential(ctx, credential, daemonID, name); err != nil {
			return WebhookRequestView{}, err
		}
		if err := s.db.WithContext(ctx).Model(&database.Credential{}).
			Where("id = ?", credential.ID).Update("last_used_at", time.Now().UTC()).Error; err != nil {
			return WebhookRequestView{}, err
		}
	} else if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return WebhookRequestView{}, err
	}
	if strings.TrimSpace(name) == "" || len(body) == 0 || len(body) > webhookBodyLimit {
		return WebhookRequestView{}, errors.New("webhook request requires a name and body")
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
		InvokedBy: invokedBy,
		Status:    webhookStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return WebhookRequestView{}, err
	}
	return viewWebhookRequest(row), nil
}

// authorizeCredential enforces a credential's scopes: each non-empty scope
// list (daemon ids, host ids, action names) is a constraint the request must
// satisfy. An empty list means unrestricted for that dimension.
func (s *Service) authorizeCredential(ctx context.Context, credential *database.Credential, daemonID, actionName string) error {
	if scopes := splitCredentialScopes(credential.DaemonIDs); len(scopes) > 0 && !slices.Contains(scopes, daemonID) {
		return ErrForbidden
	}
	if scopes := splitCredentialScopes(credential.HostIDs); len(scopes) > 0 {
		var daemon database.Daemon
		if err := s.db.WithContext(ctx).Where("id = ?", daemonID).First(&daemon).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if !slices.Contains(scopes, daemon.HostID) {
			return ErrForbidden
		}
	}
	if scopes := splitCredentialScopes(credential.ActionNames); len(scopes) > 0 && !slices.Contains(scopes, actionName) {
		return ErrForbidden
	}
	return nil
}

// CredentialTokenPrefix marks user-level API credential tokens so the auth
// middleware can route them before the Solarpass exchange.
const CredentialTokenPrefix = "mk_"

type CredentialView struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	DaemonIDs   []string   `json:"daemon_ids"`
	HostIDs     []string   `json:"host_ids"`
	ActionNames []string   `json:"action_names"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

// CredentialToken is a created credential plus the one-time plain token.
type CredentialToken struct {
	CredentialView
	Token string `json:"token"`
}

func splitCredentialScopes(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func joinCredentialScopes(parts []string) string {
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" || len(value) > 191 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return strings.Join(out, ",")
}

func hashCredentialToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func viewCredential(row database.Credential) CredentialView {
	return CredentialView{
		ID:          row.ID,
		Label:       row.Label,
		DaemonIDs:   splitCredentialScopes(row.DaemonIDs),
		HostIDs:     splitCredentialScopes(row.HostIDs),
		ActionNames: splitCredentialScopes(row.ActionNames),
		CreatedAt:   row.CreatedAt,
		LastUsedAt:  row.LastUsedAt,
	}
}

func (s *Service) CreateCredential(ctx context.Context, accountID, label string, daemonIDs, hostIDs, actionNames []string) (CredentialToken, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 128 || !utf8.ValidString(label) {
		return CredentialToken{}, fmt.Errorf("label is required and must be at most 128 bytes")
	}
	token, err := generateSecret()
	if err != nil {
		return CredentialToken{}, err
	}
	token = CredentialTokenPrefix + token
	row := database.Credential{
		ID:          uuid.NewString(),
		AccountID:   accountID,
		Label:       label,
		TokenHash:   hashCredentialToken(token),
		DaemonIDs:   joinCredentialScopes(daemonIDs),
		HostIDs:     joinCredentialScopes(hostIDs),
		ActionNames: joinCredentialScopes(actionNames),
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return CredentialToken{}, err
	}
	return CredentialToken{CredentialView: viewCredential(row), Token: token}, nil
}

func (s *Service) ListCredentials(ctx context.Context, accountID string) ([]CredentialView, error) {
	var rows []database.Credential
	if err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("created_at desc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]CredentialView, len(rows))
	for i, row := range rows {
		out[i] = viewCredential(row)
	}
	return out, nil
}

func (s *Service) DeleteCredential(ctx context.Context, accountID, id string) error {
	result := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).Delete(&database.Credential{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// CredentialByToken resolves a credential token (caller already checked the
// prefix) by its hash. Missing or revoked credentials surface as
// ErrUnauthorized.
func (s *Service) CredentialByToken(ctx context.Context, token string) (*database.Credential, error) {
	var row database.Credential
	if err := s.db.WithContext(ctx).Where("token_hash = ?", hashCredentialToken(token)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	return &row, nil
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
