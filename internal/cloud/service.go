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
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrNotFound = errors.New("not found")
var ErrForbidden = errors.New("forbidden")
var ErrPublishFailed = errors.New("notification publish failed")

// ErrRateLimited reports a daemon-initiated request that arrived sooner than
// the workspace's polling_interval_seconds quota allows (HTTP 429).
var ErrRateLimited = errors.New("rate limited")

// ErrMetricOutOfRetention reports a metric whose SentAt predates the
// workspace's metrics_retention_days quota (HTTP 400).
var ErrMetricOutOfRetention = errors.New("metric outside retention window")

// workspaceMemberRole is the minimum DyWorkspace member role level required
// to manage daemons inside a workspace. Role levels follow the workspace
// contract in ../SolarNetwork/Spec/proto/workspace.proto:
// Owner=100, Admin=75, Member=50, Viewer=25.
const workspaceMemberRole int32 = 50

// AccountClient is the account identity subset needed to select notification
// language. The account service shares the auth gRPC endpoint.
type AccountClient interface {
	GetAccount(context.Context, *gen.DyGetAccountRequest, ...grpc.CallOption) (*gen.DyAccount, error)
}

// NewAccountClient dials the account service hosted alongside authentication.
func NewAccountClient(cfg config.AuthConfig) (AccountClient, *grpc.ClientConn, error) {
	target, useTLS := dyauth.NormalizeAuthGRPCTarget(cfg.Target, cfg.UseTLS)
	if strings.TrimSpace(target) == "" {
		return nil, nil, errors.New("account gRPC target is empty")
	}
	var transportCredentials credentials.TransportCredentials
	if useTLS {
		transportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSSkipVerify})
	} else {
		transportCredentials = insecure.NewCredentials()
	}
	conn, err := grpc.Dial(target, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, nil, fmt.Errorf("dial account service: %w", err)
	}
	return gen.NewDyAccountServiceClient(conn), conn, nil
}

// WorkspaceClient is the small slice of the DyWorkspaceService gRPC surface
// MaidCafe needs. Keeping it an interface lets the service run against a fake
// in tests while the production implementation talks to the workspace service
// through the DysonGo SDK (src.solsynth.dev/sosys/go/proto).
type WorkspaceClient interface {
	IsMemberWithRole(ctx context.Context, workspaceID, accountID string, requiredRoles []int32) (bool, error)

	// GetPlanQuota returns the workspace-effective quota map (plan preset +
	// active addon grants) from the workspace service. Dimension keys are
	// configurable; MaidCafe enforces "max_daemons".
	GetPlanQuota(ctx context.Context, workspaceID string) (map[string]int64, error)
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

func (c GrpcWorkspaceClient) GetPlanQuota(ctx context.Context, workspaceID string) (map[string]int64, error) {
	if c.client == nil {
		return nil, errors.New("workspace client is not configured")
	}
	resp, err := c.client.GetPlanQuota(ctx, &gen.DyGetPlanQuotaRequest{WorkspaceId: workspaceID})
	if err != nil {
		return nil, err
	}
	return resp.GetQuotas(), nil
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
	Subtitle       string          `json:"subtitle,omitempty"`
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

// workspaceQuota returns the workspace-effective quota map (plan preset +
// active addon grants). Fails closed when the workspace service is absent.
func (s *Service) workspaceQuota(ctx context.Context, workspaceID string) (map[string]int64, error) {
	if s.workspaces == nil {
		return nil, ErrForbidden
	}
	quotas, err := s.workspaces.GetPlanQuota(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace quota: %w", err)
	}
	return quotas, nil
}

// enforcePollInterval applies the workspace polling_interval_seconds quota to
// daemon-initiated traffic (metric ingest, webhook relay pickup): requests
// arriving sooner than the allowed interval are ErrRateLimited (HTTP 429).
// A missing or non-positive dimension disables throttling.
func (s *Service) enforcePollInterval(daemon database.Daemon, quotas map[string]int64, now time.Time) error {
	intervalSeconds := quotas["polling_interval_seconds"]
	if intervalSeconds <= 0 {
		return nil
	}
	interval := time.Duration(intervalSeconds) * time.Second

	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if last, ok := s.pollHit[daemon.ID]; ok && now.Sub(last) < interval {
		return ErrRateLimited
	}
	s.pollHit[daemon.ID] = now

	// Bound memory: prune entries that have gone quiet.
	if len(s.pollHit) > 10_000 {
		for id, hit := range s.pollHit {
			if now.Sub(hit) > time.Hour {
				delete(s.pollHit, id)
			}
		}
	}
	return nil
}

// clampRetentionDays bounds the metrics_retention_days dimension to a sane
// range (0..10 years) before it is used in date arithmetic.
func clampRetentionDays(days int64) int {
	if days < 0 {
		return 0
	}
	if days > 3650 {
		return 3650
	}
	return int(days)
}

// PruneMetrics deletes metric rows older than each workspace's
// metrics_retention_days quota (measured on received_at, the cloud-side
// storage time). Workspaces without the dimension keep their metrics. Runs on
// an hourly schedule from the cloud main loop.
func (s *Service) PruneMetrics(ctx context.Context) error {
	var workspaceIDs []string
	if err := s.db.WithContext(ctx).Model(&database.Daemon{}).Distinct().Pluck("workspace_id", &workspaceIDs).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, workspaceID := range workspaceIDs {
		quotas, err := s.workspaceQuota(ctx, workspaceID)
		if err != nil {
			s.logger.Warn("metric retention prune skipped: workspace quota unavailable", "workspace_id", workspaceID, "error", err)
			continue
		}
		retentionDays := clampRetentionDays(quotas["metrics_retention_days"])
		if retentionDays <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -retentionDays)
		res := s.db.WithContext(ctx).
			Where("daemon_id IN (SELECT id FROM daemons WHERE workspace_id = ?)", workspaceID).
			Where("received_at < ?", cutoff).
			Delete(&database.DaemonMetric{})
		if res.Error != nil {
			s.logger.Warn("metric retention prune failed", "workspace_id", workspaceID, "error", res.Error)
			continue
		}
		logs := s.db.WithContext(ctx).
			Where("daemon_id IN (SELECT id FROM daemons WHERE workspace_id = ?)", workspaceID).
			Where("received_at < ?", cutoff).
			Delete(&database.DaemonLog{})
		if logs.Error != nil {
			s.logger.Warn("log retention prune failed", "workspace_id", workspaceID, "error", logs.Error)
		}
		if res.RowsAffected > 0 || logs.RowsAffected > 0 {
			s.logger.Info("pruned expired daemon data", "workspace_id", workspaceID, "metrics", res.RowsAffected, "logs", logs.RowsAffected)
		}
	}
	return nil
}

type Service struct {
	db         *database.DB
	publisher  PushPublisher
	workspaces WorkspaceClient
	accounts   AccountClient
	logger     *slog.Logger

	// pollHit tracks the last accepted daemon-initiated request (metric ingest,
	// webhook relay pickup) per daemon for the workspace polling_interval_seconds
	// quota. In-memory only: accurate per cloud instance; multi-replica deploys
	// should move this to shared state.
	pollMu  sync.Mutex
	pollHit map[string]time.Time
}

func NewService(db *database.DB, publisher PushPublisher, workspaces WorkspaceClient) *Service {
	return &Service{
		db:         db,
		publisher:  publisher,
		workspaces: workspaces,
		logger:     slog.Default(),
		pollHit:    make(map[string]time.Time),
	}
}

func (s *Service) SetAccountClient(accounts AccountClient) {
	s.accounts = accounts
}

type DaemonView struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	Name           string     `json:"name"`
	HostID         string     `json:"host_id"`
	Enabled        bool       `json:"enabled"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	DisconnectedAt *time.Time `json:"disconnected_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
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
	Subtitle    string         `json:"subtitle,omitempty"`
	Body        string         `json:"body"`
	Metadata    datatypes.JSON `json:"metadata"`
	ReadAt      *time.Time     `json:"read_at"`
	CreatedAt   time.Time      `json:"created_at"`
}
type NotificationPreferenceLevel int

const (
	NotificationPreferenceNormal NotificationPreferenceLevel = iota
	NotificationPreferenceSilent
	NotificationPreferenceReject
)

type NotificationPreferenceView struct {
	ID          string                      `json:"id"`
	AccountID   string                      `json:"account_id"`
	WorkspaceID string                      `json:"workspace_id"`
	DaemonID    string                      `json:"daemon_id"`
	Topic       string                      `json:"topic"`
	Preference  NotificationPreferenceLevel `json:"preference"`
	CreatedAt   time.Time                   `json:"created_at"`
	UpdatedAt   time.Time                   `json:"updated_at"`
}

type NotificationTopicView struct {
	Topic       string `json:"topic"`
	Description string `json:"description"`
}

type NotificationPreferenceInput struct {
	Preference int `json:"preference"`
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

// LogInput is one container log line uploaded by a daemon. The daemon
// authenticates with its registered secret; the cloud stores only bounded
// lines and timestamps.
type LogInput struct {
	ContainerID string    `json:"container_id"`
	Timestamp   time.Time `json:"timestamp"`
	Line        string    `json:"line"`
}

type LogBatchInput struct {
	Entries []LogInput `json:"entries"`
}

type LogView struct {
	ID          string    `json:"id"`
	DaemonID    string    `json:"daemon_id"`
	ContainerID string    `json:"container_id"`
	Timestamp   time.Time `json:"timestamp"`
	ReceivedAt  time.Time `json:"received_at"`
	Line        string    `json:"line"`
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
	Subtitle string          `json:"subtitle,omitempty"`
	Body     string          `json:"body"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
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
	return DaemonView{ID: d.ID, WorkspaceID: d.WorkspaceID, Name: d.Name, HostID: d.HostID, Enabled: d.Enabled, LastSeenAt: d.LastSeenAt, DisconnectedAt: d.DisconnectedAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
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

	// Enforce the workspace-effective daemon quota (plan preset + addon grants).
	quotas, err := s.workspaces.GetPlanQuota(ctx, workspaceID)
	if err != nil {
		return Credential{}, fmt.Errorf("resolve workspace quota: %w", err)
	}
	maxDaemons := quotas["max_daemons"]
	var count int64
	if err := s.db.WithContext(ctx).Model(&database.Daemon{}).Where("workspace_id = ?", workspaceID).Count(&count).Error; err != nil {
		return Credential{}, err
	}
	if count >= maxDaemons {
		return Credential{}, fmt.Errorf("workspace daemon limit reached (%d of %d)", count, maxDaemons)
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
		if !d.Enabled {
			d.DisconnectedAt = nil
		}
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
		if err := tx.Save(&d).Error; err != nil {
			return err
		}
		return tx.Model(&database.Daemon{}).Where("id = ?", d.ID).
			Update("disconnected_at", gorm.Expr("NULL")).Error
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

// WorkspaceQuotaView is the workspace-effective quota served to a daemon so it
// can self-tune (polling interval, metrics retention, daemon slots, ...).
type WorkspaceQuotaView struct {
	WorkspaceID string           `json:"workspace_id"`
	Quotas      map[string]int64 `json:"quotas"`
}

// GetDaemonQuota returns the connected workspace's effective quota for a
// daemon-authenticated caller (GET /api/daemons/:id/quota).
func (s *Service) GetDaemonQuota(ctx context.Context, id, secret string) (WorkspaceQuotaView, error) {
	d, err := s.authenticateDaemon(ctx, id, secret)
	if err != nil {
		return WorkspaceQuotaView{}, ErrUnauthorized
	}
	quotas, err := s.workspaceQuota(ctx, d.WorkspaceID)
	if err != nil {
		return WorkspaceQuotaView{}, err
	}
	return WorkspaceQuotaView{WorkspaceID: d.WorkspaceID, Quotas: quotas}, nil
}

// GetWorkspaceQuota returns the workspace-effective quota map (plan preset +
// active addon grants) to a confirmed member of the workspace, for user-facing
// display (GET /api/workspaces/:id/quota). A daemon's quota is its
// workspace's effective quota, so this is the same view the daemon fetches.
func (s *Service) GetWorkspaceQuota(ctx context.Context, accountID, workspaceID string) (WorkspaceQuotaView, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return WorkspaceQuotaView{}, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return WorkspaceQuotaView{}, err
	}
	quotas, err := s.workspaceQuota(ctx, workspaceID)
	if err != nil {
		return WorkspaceQuotaView{}, err
	}
	return WorkspaceQuotaView{WorkspaceID: workspaceID, Quotas: quotas}, nil
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

	// Workspace quotas: polling_interval_seconds throttles the report rate;
	// metrics_retention_days rejects data outside the storage window.
	quotas, err := s.workspaceQuota(ctx, d.WorkspaceID)
	if err != nil {
		return err
	}
	if err := s.enforcePollInterval(d, quotas, now); err != nil {
		return err
	}
	if retentionDays := clampRetentionDays(quotas["metrics_retention_days"]); retentionDays > 0 {
		if input.SentAt.Before(now.AddDate(0, 0, -retentionDays)) {
			return fmt.Errorf("%w: sent_at older than %d days", ErrMetricOutOfRetention, retentionDays)
		}
	}

	if err := s.db.WithContext(ctx).Create(&database.DaemonMetric{
		ID: uuid.NewString(), DaemonID: d.ID, SentAt: input.SentAt.UTC(), ReceivedAt: now,
		UptimeSeconds: input.UptimeSeconds, ProcessMemoryBytes: input.ProcessMemoryBytes,
		CPUPercent: input.CPUPercent, CPUCount: input.CPUCount,
		Load1: input.Load1, Load5: input.Load5, Load15: input.Load15,
		MemoryUsedPercent: input.MemoryUsedPercent,
		MemoryUsedBytes:   input.MemoryUsedBytes, MemoryTotalBytes: input.MemoryTotalBytes,
		SwapTotalKb: input.SwapTotalKb, SwapFreeKb: input.SwapFreeKb,
		DiskTotalKb: input.DiskTotalKb, DiskAvailableKb: input.DiskAvailableKb,
		NetRxBytes: input.NetRxBytes, NetTxBytes: input.NetTxBytes,
		WebhookExecutions: input.WebhookExecutions, WebhookFailures: input.WebhookFailures,
	}).Error; err != nil {
		return err
	}
	// Alarms are evaluated daemon-side; this metric records the cloud-side
	// heartbeat and clears a prior disconnect state.
	columns := []string{"last_seen_at", "updated_at"}
	updates := database.Daemon{LastSeenAt: &now, UpdatedAt: now}
	if hostID := strings.TrimSpace(input.HostID); hostID != "" && hostID != d.HostID {
		updates.HostID = hostID
		columns = append(columns, "host_id")
	}
	q := s.db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?", d.ID).Select(columns).Updates(&updates)
	if q.Error != nil {
		return q.Error
	}
	result := s.db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ? AND disconnected_at IS NOT NULL", d.ID).
		Update("disconnected_at", gorm.Expr("NULL"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		metadata := map[string]any{"reconnected_at": now, "last_seen_at": now}
		body := "Metrics received again after the daemon disconnected."
		downtimeText := "an unknown interval"
		if d.DisconnectedAt != nil {
			downtime := now.Sub(d.DisconnectedAt.UTC())
			if downtime < 0 {
				downtime = 0
			}
			downtimeText = downtime.Round(time.Second).String()
			metadata["disconnected_at"] = d.DisconnectedAt.UTC()
			metadata["downtime_seconds"] = int64(downtime / time.Second)
			body = fmt.Sprintf("Metrics resumed after %s.", downtimeText)
		}
		metadata["downtime"] = downtimeText
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		title, body := s.localizedAlarm(ctx, d.AccountID, "daemon.reconnected", "Daemon reconnected", body, encoded)
		notification := database.Notification{
			ID: uuid.NewString(), AccountID: d.AccountID, WorkspaceID: d.WorkspaceID,
			DaemonID: d.ID, Kind: "daemon.reconnected", Title: title,
			Body: body, Metadata: datatypes.JSON(encoded), CreatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&notification).Error; err != nil {
			return err
		}
		if err := s.publishNotification(ctx, d, notification); err != nil {
			return err
		}
	}
	return nil
}

// IngestLogs stores a bounded batch of daemon log lines. Uploads use the
// daemon secret and are intentionally independent of metric poll pacing:
// the daemon batches locally and the workspace retention quota bounds age.
func (s *Service) IngestLogs(ctx context.Context, id, secret string, input LogBatchInput) error {
	d, err := s.authenticateDaemon(ctx, id, secret)
	if err != nil {
		return ErrUnauthorized
	}
	if len(input.Entries) == 0 || len(input.Entries) > 500 {
		return fmt.Errorf("entries must contain between 1 and 500 lines")
	}
	now := time.Now().UTC()
	quotas, err := s.workspaceQuota(ctx, d.WorkspaceID)
	if err != nil {
		return err
	}
	retentionDays := clampRetentionDays(quotas["metrics_retention_days"])
	rows := make([]database.DaemonLog, 0, len(input.Entries))
	for _, entry := range input.Entries {
		entry.ContainerID = strings.TrimSpace(entry.ContainerID)
		if entry.ContainerID == "" || len(entry.ContainerID) > 128 || !utf8.ValidString(entry.ContainerID) {
			return fmt.Errorf("container_id exceeds bounds or is empty")
		}
		if entry.Timestamp.IsZero() {
			return fmt.Errorf("timestamp is required")
		}
		if retentionDays > 0 && entry.Timestamp.Before(now.AddDate(0, 0, -retentionDays)) {
			return fmt.Errorf("log outside retention window")
		}
		if len(entry.Line) == 0 || len(entry.Line) > 4096 || !utf8.ValidString(entry.Line) {
			return fmt.Errorf("line exceeds bounds or is empty")
		}
		rows = append(rows, database.DaemonLog{
			ID: uuid.NewString(), DaemonID: d.ID, ContainerID: entry.ContainerID,
			Timestamp: entry.Timestamp.UTC(), ReceivedAt: now, Line: entry.Line,
		})
	}
	return s.db.WithContext(ctx).Create(&rows).Error
}

// ListLogs returns recent uploaded log lines to workspace members.
func (s *Service) ListLogs(ctx context.Context, accountID, daemonID, containerID string, limit int, before *time.Time) ([]LogView, error) {
	if _, err := s.daemonForAccount(ctx, accountID, daemonID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := s.db.WithContext(ctx).Where("daemon_id = ?", daemonID)
	if containerID = strings.TrimSpace(containerID); containerID != "" {
		query = query.Where("container_id = ?", containerID)
	}
	if before != nil {
		query = query.Where("timestamp < ?", before.UTC())
	}
	var rows []database.DaemonLog
	if err := query.Order("timestamp desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LogView, len(rows))
	for i, row := range rows {
		out[i] = LogView{ID: row.ID, DaemonID: row.DaemonID, ContainerID: row.ContainerID, Timestamp: row.Timestamp, ReceivedAt: row.ReceivedAt, Line: row.Line}
	}
	return out, nil
}

const (
	DefaultDaemonDisconnectAfter                = 5 * time.Minute
	DefaultDaemonDisconnectNotificationCooldown = 30 * time.Minute
	DefaultAlarmCheckInterval                   = time.Minute
)

// EvaluateDisconnectedDaemons marks enabled daemons disconnected when the
// cloud has not accepted a metric within disconnectedAfter. It uses the
// default repeat-notification cooldown.
func (s *Service) EvaluateDisconnectedDaemons(ctx context.Context, disconnectedAfter time.Duration, now time.Time) error {
	return s.EvaluateDisconnectedDaemonsWithCooldown(ctx, disconnectedAfter, DefaultDaemonDisconnectNotificationCooldown, now)
}

// EvaluateDisconnectedDaemonsWithCooldown is the configurable form of
// EvaluateDisconnectedDaemons. A later metric clears the state and publishes
// a reconnected notification, allowing a future outage to alarm again.
func (s *Service) EvaluateDisconnectedDaemonsWithCooldown(ctx context.Context, disconnectedAfter, disconnectNotificationCooldown time.Duration, now time.Time) error {
	if disconnectedAfter <= 0 {
		disconnectedAfter = DefaultDaemonDisconnectAfter
	}
	if disconnectNotificationCooldown <= 0 {
		disconnectNotificationCooldown = DefaultDaemonDisconnectNotificationCooldown
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	var daemons []database.Daemon
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Find(&daemons).Error; err != nil {
		return err
	}
	var firstErr error
	for _, candidate := range daemons {
		preference, preferenceErr := s.notificationPreference(ctx, candidate.AccountID, candidate.WorkspaceID, candidate.ID, "daemon.disconnected")
		if preferenceErr != nil {
			if firstErr == nil {
				firstErr = preferenceErr
			}
			continue
		}
		storeNotification := preference != NotificationPreferenceReject
		publishNotification := preference == NotificationPreferenceNormal
		var notification *database.Notification
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var daemon database.Daemon
			if err := tx.Where("id = ?", candidate.ID).First(&daemon).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}

			stale := daemon.LastSeenAt != nil && !now.Before(daemon.LastSeenAt.Add(disconnectedAfter))
			if stale {
				if daemon.DisconnectedAt != nil {
					return nil
				}
				var previous database.Notification
				err := tx.Where("daemon_id = ? AND kind = ?", daemon.ID, "daemon.disconnected").
					Order("created_at DESC").First(&previous).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil && now.Before(previous.CreatedAt.Add(disconnectNotificationCooldown)) {
					return tx.Model(&database.Daemon{}).Where("id = ? AND disconnected_at IS NULL", daemon.ID).
						Updates(map[string]any{"disconnected_at": now, "updated_at": now}).Error
				}
				disconnectedAt := now
				result := tx.Model(&database.Daemon{}).
					Where("id = ? AND disconnected_at IS NULL", daemon.ID).
					Updates(map[string]any{"disconnected_at": disconnectedAt, "updated_at": now})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return nil
				}
				age := now.Sub(*daemon.LastSeenAt)
				metadata, err := json.Marshal(map[string]any{
					"last_seen_at":      daemon.LastSeenAt.UTC(),
					"disconnected_at":   disconnectedAt,
					"threshold_seconds": int64(disconnectedAfter / time.Second),
					"age_seconds":       int64(age / time.Second),
					"age":               age.Round(time.Second).String(),
					"last_seen":         daemon.LastSeenAt.UTC().Format(time.RFC3339),
				})
				if err != nil {
					return err
				}
				if !storeNotification {
					return nil
				}
				notification = &database.Notification{
					ID: uuid.NewString(), AccountID: daemon.AccountID,
					WorkspaceID: daemon.WorkspaceID, DaemonID: daemon.ID,
					Kind: "daemon.disconnected", Title: "Daemon disconnected",
					Body:     fmt.Sprintf("No metrics received for %s; last seen at %s.", age.Round(time.Second), daemon.LastSeenAt.UTC().Format(time.RFC3339)),
					Metadata: datatypes.JSON(metadata), CreatedAt: now,
				}
				return tx.Create(notification).Error
			}
			if daemon.DisconnectedAt != nil {
				return tx.Model(&database.Daemon{}).Where("id = ?", daemon.ID).
					Update("disconnected_at", gorm.Expr("NULL")).Error
			}
			return nil
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if notification != nil {
			title, body := s.localizedAlarm(ctx, notification.AccountID, notification.Kind, notification.Title, notification.Body, notification.Metadata)
			if title != notification.Title || body != notification.Body {
				notification.Title, notification.Body = title, body
				if err := s.db.WithContext(ctx).Model(&database.Notification{}).
					Where("id = ?", notification.ID).
					Updates(map[string]any{"title": title, "body": body}).Error; err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			}
			if publishNotification {
				if err := s.publishNotification(ctx, candidate, *notification); err != nil {
					if firstErr == nil {
						firstErr = err
					}
				}
			}
		}
	}
	return firstErr
}

func (s *Service) publishNotification(ctx context.Context, daemon database.Daemon, notification database.Notification) error {
	if s.publisher == nil {
		return nil
	}
	// Enrich the payload metadata with the daemon's identity so the feed can
	// show which server sent the notification. Daemon-supplied keys are kept;
	// the identity fields are authoritative and always win.
	meta := map[string]any{}
	if len(notification.Metadata) > 0 {
		if err := json.Unmarshal(notification.Metadata, &meta); err != nil {
			meta = map[string]any{}
		}
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["daemon_id"] = daemon.ID
	meta["daemon_name"] = daemon.Name
	if daemon.HostID != "" {
		meta["host_id"] = daemon.HostID
	}
	merged, err := json.Marshal(meta)
	if err != nil {
		merged = notification.Metadata
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
		Subtitle:       notificationSubtitle(daemon.Name, notification.Subtitle, s.notificationSourcePrefix(ctx, notification.AccountID)),
		Body:           notification.Body,
		Metadata:       merged,
	}
	if err := s.publisher.Publish(ctx, event); err != nil {
		s.logger.Error("publish notification to Metoer failed",
			"notification_id", notification.ID,
			"daemon_id", notification.DaemonID,
			"error", err)
		return fmt.Errorf("%w: %v", ErrPublishFailed, err)
	}
	return nil
}

func (s *Service) notificationSourcePrefix(ctx context.Context, accountID string) string {
	if s.accounts == nil {
		return "From"
	}
	account, err := s.accounts.GetAccount(ctx, &gen.DyGetAccountRequest{Id: accountID})
	if err != nil || account == nil {
		return "From"
	}
	switch normalizeAlarmLocale(account.GetLanguage()) {
	case "zh-cn":
		return "来自"
	case "zh-tw":
		return "來自"
	default:
		return "From"
	}
}

func notificationSubtitle(daemonName, subtitle, sourcePrefix string) string {
	source := strings.TrimSpace(sourcePrefix) + " " + strings.TrimSpace(daemonName)
	subtitle = strings.TrimSpace(subtitle)
	if subtitle == "" {
		return source
	}
	return source + ": " + subtitle
}
func (s *Service) ListNotificationPreferences(ctx context.Context, accountID, workspaceID, daemonID string) ([]NotificationPreferenceView, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ?",
		accountID,
		workspaceID,
	)
	if daemonID != "" {
		query = query.Where("daemon_id = ?", daemonID)
	}
	var rows []database.NotificationPreference
	if err := query.Order("daemon_id asc, topic asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]NotificationPreferenceView, len(rows))
	for i, row := range rows {
		out[i] = notificationPreferenceView(row)
	}
	return out, nil
}

func (s *Service) ListNotificationTopics(ctx context.Context, accountID, workspaceID string) ([]NotificationTopicView, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return nil, err
	}
	topics := map[string]string{
		"daemon.alarm.container_down":      "Container down alarms",
		"daemon.alarm.cpu_percent":         "CPU alarms",
		"daemon.alarm.disk_used_percent":   "Disk alarms",
		"daemon.alarm.memory_used_percent": "Memory alarms",
		"daemon.disconnected":              "Daemon disconnected",
		"daemon.reconnected":               "Daemon reconnected",
		"daemon.notification":              "Daemon notifications",
		"test.notification":                "Test notifications",
		"user.request":                     "User requests",
		"webhook.failure":                  "Webhook failures",
		"webhook.success":                  "Webhook successes",
	}
	var kinds []string
	if err := s.db.WithContext(ctx).Model(&database.Notification{}).
		Where("workspace_id = ?", workspaceID).Distinct("kind").Pluck("kind", &kinds).Error; err != nil {
		return nil, err
	}
	for _, kind := range kinds {
		if _, ok := topics[kind]; !ok {
			topics[kind] = notificationTopicDescription(kind)
		}
	}
	var preferences []database.NotificationPreference
	if err := s.db.WithContext(ctx).
		Where("account_id = ? AND workspace_id = ?", accountID, workspaceID).
		Find(&preferences).Error; err != nil {
		return nil, err
	}
	for _, preference := range preferences {
		if _, ok := topics[preference.Topic]; !ok {
			topics[preference.Topic] = notificationTopicDescription(preference.Topic)
		}
	}
	names := make([]string, 0, len(topics))
	for topic := range topics {
		names = append(names, topic)
	}
	slices.Sort(names)
	out := make([]NotificationTopicView, 0, len(names))
	for _, topic := range names {
		out = append(out, NotificationTopicView{Topic: topic, Description: topics[topic]})
	}
	return out, nil
}

func (s *Service) SetNotificationPreference(ctx context.Context, accountID, workspaceID, daemonID, topic string, preference int) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if err := validateNotificationPreference(preference); err != nil {
		return err
	}
	topic, err := bound(topic, 128)
	if err != nil {
		return fmt.Errorf("topic: %w", err)
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return err
	}
	if daemonID != "" {
		daemon, err := s.daemonForAccount(ctx, accountID, daemonID)
		if err != nil {
			return err
		}
		if daemon.WorkspaceID != workspaceID {
			return ErrForbidden
		}
	}
	var row database.NotificationPreference
	query := s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ? AND daemon_id = ? AND topic = ?",
		accountID,
		workspaceID,
		daemonID,
		topic,
	)
	err = query.First(&row).Error
	now := time.Now().UTC()
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.db.WithContext(ctx).Create(&database.NotificationPreference{
			ID: uuid.NewString(), AccountID: accountID, WorkspaceID: workspaceID,
			DaemonID: daemonID, Topic: topic, Preference: preference,
			CreatedAt: now, UpdatedAt: now,
		}).Error
	case err != nil:
		return err
	default:
		return s.db.WithContext(ctx).Model(&row).Updates(map[string]any{
			"preference": preference,
			"updated_at": now,
		}).Error
	}
}

// SetAllDaemonNotificationPreferences applies one delivery policy to every
// topic currently known in the workspace for a daemon. The operation is
// transactional so a batch never leaves a partially updated daemon scope.
func (s *Service) SetAllDaemonNotificationPreferences(ctx context.Context, accountID, workspaceID, daemonID string, preference int) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if err := validateNotificationPreference(preference); err != nil {
		return err
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return err
	}
	daemon, err := s.daemonForAccount(ctx, accountID, daemonID)
	if err != nil {
		return err
	}
	if daemon.WorkspaceID != workspaceID {
		return ErrForbidden
	}
	topics, err := s.ListNotificationTopics(ctx, accountID, workspaceID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, topic := range topics {
			var row database.NotificationPreference
			query := tx.Where(
				"account_id = ? AND workspace_id = ? AND daemon_id = ? AND topic = ?",
				accountID,
				workspaceID,
				daemonID,
				topic.Topic,
			)
			err := query.First(&row).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				if err := tx.Create(&database.NotificationPreference{
					ID: uuid.NewString(), AccountID: accountID, WorkspaceID: workspaceID,
					DaemonID: daemonID, Topic: topic.Topic, Preference: preference,
					CreatedAt: now, UpdatedAt: now,
				}).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				if err := tx.Model(&row).Updates(map[string]any{
					"preference": preference,
					"updated_at": now,
				}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// DeleteAllDaemonNotificationPreferences removes every daemon-specific
// override, restoring the account-wide policy for all topics.
func (s *Service) DeleteAllDaemonNotificationPreferences(ctx context.Context, accountID, workspaceID, daemonID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return err
	}
	daemon, err := s.daemonForAccount(ctx, accountID, daemonID)
	if err != nil {
		return err
	}
	if daemon.WorkspaceID != workspaceID {
		return ErrForbidden
	}
	return s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ? AND daemon_id = ?",
		accountID,
		workspaceID,
		daemonID,
	).Delete(&database.NotificationPreference{}).Error
}

func (s *Service) DeleteNotificationPreference(ctx context.Context, accountID, workspaceID, daemonID, topic string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	topic, err := bound(topic, 128)
	if err != nil {
		return fmt.Errorf("topic: %w", err)
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return err
	}
	if daemonID != "" {
		daemon, err := s.daemonForAccount(ctx, accountID, daemonID)
		if err != nil {
			return err
		}
		if daemon.WorkspaceID != workspaceID {
			return ErrForbidden
		}
	}
	return s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ? AND daemon_id = ? AND topic = ?",
		accountID,
		workspaceID,
		daemonID,
		topic,
	).Delete(&database.NotificationPreference{}).Error
}

func (s *Service) notificationPreference(ctx context.Context, accountID, workspaceID, daemonID, topic string) (NotificationPreferenceLevel, error) {
	var row database.NotificationPreference
	err := s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ? AND daemon_id = ? AND topic = ?",
		accountID, workspaceID, daemonID, topic,
	).First(&row).Error
	if err == nil {
		return normalizeNotificationPreference(row.Preference), nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return NotificationPreferenceNormal, err
	}
	err = s.db.WithContext(ctx).Where(
		"account_id = ? AND workspace_id = ? AND daemon_id = ? AND topic = ?",
		accountID, workspaceID, "", topic,
	).First(&row).Error
	if err == nil {
		return normalizeNotificationPreference(row.Preference), nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return NotificationPreferenceNormal, nil
	}
	return NotificationPreferenceNormal, err
}

func notificationPreferenceView(row database.NotificationPreference) NotificationPreferenceView {
	return NotificationPreferenceView{
		ID: row.ID, AccountID: row.AccountID, WorkspaceID: row.WorkspaceID,
		DaemonID: row.DaemonID, Topic: row.Topic,
		Preference: normalizeNotificationPreference(row.Preference),
		CreatedAt:  row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func validateNotificationPreference(value int) error {
	if value < int(NotificationPreferenceNormal) || value > int(NotificationPreferenceReject) {
		return fmt.Errorf("preference must be 0 (normal), 1 (silent), or 2 (reject)")
	}
	return nil
}

func normalizeNotificationPreference(value int) NotificationPreferenceLevel {
	if value < int(NotificationPreferenceNormal) || value > int(NotificationPreferenceReject) {
		return NotificationPreferenceNormal
	}
	return NotificationPreferenceLevel(value)
}

func notificationTopicDescription(topic string) string {
	words := strings.FieldsFunc(topic, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	if len(words) == 0 {
		return "Notifications"
	}
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
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
	subtitle := strings.TrimSpace(input.Subtitle)
	if subtitle != "" && (len(subtitle) > 128 || !utf8.ValidString(subtitle)) {
		return NotificationView{}, fmt.Errorf("subtitle exceeds bounds")
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
	preference, err := s.notificationPreference(ctx, accountID, daemon.WorkspaceID, daemonID, kind)
	if err != nil {
		return NotificationView{}, err
	}
	if preference == NotificationPreferenceReject {
		return NotificationView{}, nil
	}
	title, body = s.localizedAlarm(ctx, accountID, kind, title, body, normalized)
	row := database.Notification{ID: uuid.NewString(), AccountID: accountID, WorkspaceID: daemon.WorkspaceID, DaemonID: daemonID, Kind: kind, Title: title, Subtitle: subtitle, Body: body, Metadata: datatypes.JSON(normalized), CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return NotificationView{}, err
	}
	if preference == NotificationPreferenceNormal {
		if err := s.publishNotification(ctx, daemon, row); err != nil {
			return NotificationView{}, err
		}
	}
	return notificationView(row), nil
}
func (s *Service) localizedAlarm(ctx context.Context, accountID, kind, title, body string, metadata []byte) (string, string) {
	if !localizableNotificationKind(kind) || s.accounts == nil {
		return title, body
	}
	account, err := s.accounts.GetAccount(ctx, &gen.DyGetAccountRequest{Id: accountID})
	if err != nil || account == nil {
		if err != nil {
			s.logger.Warn("alarm localization skipped", "account_id", accountID, "error", err)
		}
		return title, body
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		return title, body
	}
	if values == nil {
		values = map[string]any{}
	}
	values["body"] = body
	localizedTitle, localizedBody, ok := localizeAlarm(account.GetLanguage(), kind, values)
	if !ok {
		return title, body
	}
	return localizedTitle, localizedBody
}
func localizableNotificationKind(kind string) bool {
	return kind == "daemon.disconnected" ||
		kind == "daemon.reconnected" ||
		kind == "webhook.success" ||
		kind == "webhook.failure" ||
		strings.HasPrefix(kind, "daemon.alarm.")
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
	subtitle := strings.TrimSpace(input.Subtitle)
	if subtitle != "" && (len(subtitle) > 128 || !utf8.ValidString(subtitle)) {
		return NotificationView{}, fmt.Errorf("subtitle exceeds bounds")
	}
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
	preference, err := s.notificationPreference(ctx, d.AccountID, d.WorkspaceID, d.ID, kind)
	if err != nil {
		return NotificationView{}, err
	}
	if preference == NotificationPreferenceReject {
		return NotificationView{}, nil
	}
	title, body = s.localizedAlarm(ctx, d.AccountID, kind, title, body, metadata)
	n := database.Notification{ID: uuid.NewString(), AccountID: d.AccountID, WorkspaceID: d.WorkspaceID, DaemonID: d.ID, Kind: kind, Title: title, Subtitle: subtitle, Body: body, Metadata: datatypes.JSON(metadata), CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&n).Error; err != nil {
		return NotificationView{}, err
	}
	if preference == NotificationPreferenceNormal {
		if err := s.publishNotification(ctx, d, n); err != nil {
			return NotificationView{}, err
		}
	}
	return notificationView(n), nil
}
func notificationView(n database.Notification) NotificationView {
	return NotificationView{ID: n.ID, AccountID: n.AccountID, WorkspaceID: n.WorkspaceID, DaemonID: n.DaemonID, Kind: n.Kind, Title: n.Title, Subtitle: n.Subtitle, Body: n.Body, Metadata: n.Metadata, ReadAt: n.ReadAt, CreatedAt: n.CreatedAt}
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
func (s *Service) MarkAllNotificationsRead(ctx context.Context, accountID, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspace(ctx, accountID, workspaceID); err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&database.Notification{}).
		Where("workspace_id = ? AND read_at IS NULL", workspaceID).
		Update("read_at", now).Error
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
	d, err := s.authenticateDaemon(ctx, daemonID, secret)
	if err != nil {
		return nil, err
	}
	// Workspace polling_interval_seconds quota throttles relay pickup speed.
	quotas, err := s.workspaceQuota(ctx, d.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := s.enforcePollInterval(d, quotas, time.Now().UTC()); err != nil {
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
