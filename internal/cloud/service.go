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
type PushPublisher interface { Publish(context.Context, NotificationEvent) error }

type NotificationEvent struct {
	EventID string `json:"event_id"`
	Timestamp time.Time `json:"timestamp"`
	AccountID string `json:"account_id"`
	DaemonID string `json:"daemon_id"`
	NotificationID string `json:"notification_id"`
	Kind string `json:"kind"`
	Title string `json:"title"`
	Body string `json:"body"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (s *Service) daemonForAccount(ctx context.Context, accountID, id string) (database.Daemon, error) {
	var d database.Daemon
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) { return d, ErrNotFound }
		return d, err
	}
	if d.AccountID != accountID { return database.Daemon{}, ErrForbidden }
	return d, nil
}
type Service struct { db *database.DB; publisher PushPublisher }
func NewService(db *database.DB, publisher PushPublisher) *Service { return &Service{db: db, publisher: publisher} }

 type DaemonView struct {
	ID string `json:"id"`; Name string `json:"name"`; Enabled bool `json:"enabled"`
	LastSeenAt *time.Time `json:"last_seen_at"`; CreatedAt time.Time `json:"created_at"`; UpdatedAt time.Time `json:"updated_at"`
}
 type Credential struct { DaemonView; Secret string `json:"secret"` }
 type NotificationView struct {
	ID string `json:"id"`; AccountID string `json:"account_id"`; DaemonID string `json:"daemon_id"`; Kind string `json:"kind"`; Title string `json:"title"`; Body string `json:"body"`; Metadata datatypes.JSON `json:"metadata"`; ReadAt *time.Time `json:"read_at"`; CreatedAt time.Time `json:"created_at"`
}
 type MetricInput struct { SentAt time.Time `json:"sent_at"`; UptimeSeconds int64 `json:"uptime_seconds"`; ProcessMemoryBytes int64 `json:"process_memory_bytes"`; WebhookExecutions uint64 `json:"webhook_executions"`; WebhookFailures uint64 `json:"webhook_failures"` }
 type NotificationInput struct { Kind string `json:"kind"`; Title string `json:"title"`; Body string `json:"body"`; Metadata json.RawMessage `json:"metadata"` }

func generateSecret() (string, error) { buf := make([]byte, 32); if _, err := rand.Read(buf); err != nil { return "", err }; return base64.RawURLEncoding.EncodeToString(buf), nil }
func viewDaemon(d database.Daemon) DaemonView { return DaemonView{ID:d.ID,Name:d.Name,Enabled:d.Enabled,LastSeenAt:d.LastSeenAt,CreatedAt:d.CreatedAt,UpdatedAt:d.UpdatedAt} }
func (s *Service) CreateDaemon(ctx context.Context, accountID, name string) (Credential, error) {
	name = strings.TrimSpace(name); if name == "" { return Credential{}, fmt.Errorf("name is required") }
	secret, err := generateSecret(); if err != nil { return Credential{}, err }; hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost); if err != nil { return Credential{}, err }
	d := database.Daemon{ID:uuid.NewString(), AccountID:accountID, Name:name, SecretHash:string(hash), Enabled:true}
	if err := s.db.WithContext(ctx).Create(&d).Error; err != nil { return Credential{}, err }
	return Credential{DaemonView:viewDaemon(d),Secret:secret}, nil
}
func (s *Service) ListDaemons(ctx context.Context, accountID string) ([]DaemonView,error) { var rows []database.Daemon; if err:=s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("created_at desc").Find(&rows).Error; err!=nil{return nil,err}; out:=make([]DaemonView,len(rows));for i:=range rows{out[i]=viewDaemon(rows[i])};return out,nil }
func (s *Service) GetDaemon(ctx context.Context, accountID, id string) (DaemonView, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil { return DaemonView{}, err }
	return viewDaemon(d), nil
}
func (s *Service) UpdateDaemon(ctx context.Context, accountID, id string, name *string, enabled *bool) (DaemonView, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil { return DaemonView{}, err }
	if name != nil { d.Name = strings.TrimSpace(*name); if d.Name == "" { return DaemonView{}, fmt.Errorf("name is required") } }
	if enabled != nil { d.Enabled = *enabled }
	if err := s.db.WithContext(ctx).Save(&d).Error; err != nil { return DaemonView{}, err }
	return viewDaemon(d), nil
}
func (s *Service) RotateSecret(ctx context.Context, accountID, id string) (string, error) {
	d, err := s.daemonForAccount(ctx, accountID, id)
	if err != nil { return "", err }
	secret, err := generateSecret(); if err != nil { return "", err }
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost); if err != nil { return "", err }
	d.SecretHash = string(hash)
	if err := s.db.WithContext(ctx).Save(&d).Error; err != nil { return "", err }
	return secret, nil
}
func (s *Service) DisableDaemon(ctx context.Context, accountID, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var d database.Daemon
		if err := tx.Where("id = ?", id).First(&d).Error; err != nil { if errors.Is(err, gorm.ErrRecordNotFound) { return ErrNotFound }; return err }
		if d.AccountID != accountID { return ErrForbidden }
		d.Enabled = false
		if err := tx.Save(&d).Error; err != nil { return err }
		return tx.Where("daemon_id = ?", id).Delete(&database.DaemonMetric{}).Error
	})
}
func (s *Service) authenticateDaemon(ctx context.Context,id,secret string)(database.Daemon,error){var d database.Daemon;if err:=s.db.WithContext(ctx).Where("id = ?",id).First(&d).Error;err!=nil{return d,ErrUnauthorized};if !d.Enabled || bcrypt.CompareHashAndPassword([]byte(d.SecretHash),[]byte(secret))!=nil{return database.Daemon{},ErrUnauthorized};return d,nil}
func (s *Service) IngestMetric(ctx context.Context,id,secret string,input MetricInput)error{d,err:=s.authenticateDaemon(ctx,id,secret);if err!=nil{return ErrUnauthorized};if input.SentAt.IsZero()||input.UptimeSeconds<0||input.ProcessMemoryBytes<0{return fmt.Errorf("invalid metric")};now:=time.Now().UTC();if err:=s.db.WithContext(ctx).Create(&database.DaemonMetric{ID:uuid.NewString(),DaemonID:d.ID,SentAt:input.SentAt.UTC(),ReceivedAt:now,UptimeSeconds:input.UptimeSeconds,ProcessMemoryBytes:input.ProcessMemoryBytes,WebhookExecutions:input.WebhookExecutions,WebhookFailures:input.WebhookFailures}).Error;err!=nil{return err};return s.db.WithContext(ctx).Model(&database.Daemon{}).Where("id = ?",d.ID).Updates(map[string]any{"last_seen_at":now,"updated_at":now}).Error}
func bound(value string,max int) (string,error){value=strings.TrimSpace(value);if value==""||len(value)>max||!utf8.ValidString(value){return "",fmt.Errorf("value exceeds bounds or is empty")};return value,nil}
func (s *Service) CreateNotification(ctx context.Context,id,secret string,input NotificationInput)(NotificationView,error){d,err:=s.authenticateDaemon(ctx,id,secret);if err!=nil{return NotificationView{},ErrUnauthorized};kind,err:=bound(input.Kind,128);if err!=nil{return NotificationView{},fmt.Errorf("kind: %w",err)};title,err:=bound(input.Title,128);if err!=nil{return NotificationView{},fmt.Errorf("title: %w",err)};if len(input.Body)>4096||!utf8.ValidString(input.Body){return NotificationView{},fmt.Errorf("body exceeds bounds")};body:=strings.TrimSpace(input.Body);if body==""{return NotificationView{},fmt.Errorf("body is required")};metadata:=input.Metadata;if len(metadata)==0{metadata=[]byte("{}")} ;var decoded any;if err:=json.Unmarshal(metadata,&decoded);err!=nil{return NotificationView{},fmt.Errorf("metadata: invalid json")};metadata,err=json.Marshal(decoded);if err!=nil||len(metadata)>16*1024{return NotificationView{},fmt.Errorf("metadata exceeds bounds")};n:=database.Notification{ID:uuid.NewString(),AccountID:d.AccountID,DaemonID:d.ID,Kind:kind,Title:title,Body:body,Metadata:datatypes.JSON(metadata),CreatedAt:time.Now().UTC()};if err:=s.db.WithContext(ctx).Create(&n).Error;err!=nil{return NotificationView{},err};event:=NotificationEvent{EventID:uuid.NewString(),Timestamp:n.CreatedAt,AccountID:n.AccountID,DaemonID:n.DaemonID,NotificationID:n.ID,Kind:n.Kind,Title:n.Title,Body:n.Body,Metadata:json.RawMessage(metadata)};if s.publisher!=nil{_ = s.publisher.Publish(ctx,event)};return notificationView(n),nil}
func notificationView(n database.Notification)NotificationView{return NotificationView{ID:n.ID,AccountID:n.AccountID,DaemonID:n.DaemonID,Kind:n.Kind,Title:n.Title,Body:n.Body,Metadata:n.Metadata,ReadAt:n.ReadAt,CreatedAt:n.CreatedAt}}
func (s *Service) ListNotifications(ctx context.Context,accountID string,unread bool,daemonID string,limit int,before *time.Time)([]NotificationView,error){if limit<=0{limit=50};if limit>100{limit=100};q:=s.db.WithContext(ctx).Where("account_id = ?",accountID);if unread{q=q.Where("read_at IS NULL")};if daemonID!=""{q=q.Where("daemon_id = ?",daemonID)};if before!=nil{q=q.Where("created_at < ?",before.UTC())};var rows []database.Notification;if err:=q.Order("created_at desc").Limit(limit).Find(&rows).Error;err!=nil{return nil,err};out:=make([]NotificationView,len(rows));for i:=range rows{out[i]=notificationView(rows[i])};return out,nil}
func (s *Service) MarkNotificationRead(ctx context.Context, accountID, id string) error { var n database.Notification; if err:=s.db.WithContext(ctx).Where("id = ?",id).First(&n).Error;err!=nil{if errors.Is(err,gorm.ErrRecordNotFound){return ErrNotFound};return err};if n.AccountID!=accountID{return ErrForbidden};if n.ReadAt==nil{now:=time.Now().UTC();if err:=s.db.WithContext(ctx).Model(&n).Update("read_at",now).Error;err!=nil{return err}};return nil}
