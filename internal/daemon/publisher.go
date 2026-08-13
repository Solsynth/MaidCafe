package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type notificationPayload struct { Kind string `json:"kind"`; Title string `json:"title"`; Body string `json:"body"`; Metadata map[string]any `json:"metadata"` }
type CloudPublisher struct { baseURL string; daemonID string; secret string; client *http.Client; timeout time.Duration; logger *slog.Logger }
func NewCloudPublisher(cfg config.DaemonConfig, logger *slog.Logger) (*CloudPublisher,error) { if strings.TrimSpace(cfg.CloudURL)==""||strings.TrimSpace(cfg.CloudSecret)=="" { return nil,nil };parsed,err:=url.Parse(strings.TrimRight(strings.TrimSpace(cfg.CloudURL),"/"));if err!=nil||parsed.Host==""||(parsed.Scheme!="https"&&!(parsed.Scheme=="http"&&(parsed.Hostname()=="localhost"||parsed.Hostname()=="127.0.0.1"))){return nil,fmt.Errorf("cloudUrl must be HTTPS, or HTTP to localhost")};if logger==nil{logger=slog.Default()};return &CloudPublisher{baseURL:parsed.String(),daemonID:cfg.ID,secret:cfg.CloudSecret,timeout:cfg.RequestTimeout,logger:logger,client:&http.Client{CheckRedirect:func(_ *http.Request,_ []*http.Request)error{return http.ErrUseLastResponse}}},nil }
func (p *CloudPublisher) post(ctx context.Context,suffix string,payload any){body,err:=json.Marshal(payload);if err!=nil{return};ctx,cancel:=context.WithTimeout(ctx,p.timeout);defer cancel();req,err:=http.NewRequestWithContext(ctx,http.MethodPost,p.baseURL+"/api/daemons/"+p.daemonID+suffix,bytes.NewReader(body));if err!=nil{return};req.Header.Set("Authorization","Bearer "+p.secret);req.Header.Set("Content-Type","application/json");resp,err:=p.client.Do(req);if err!=nil{p.logger.Warn("cloud publish failed", "error",err);return};_ = resp.Body.Close();if resp.StatusCode<200||resp.StatusCode>=300{p.logger.Warn("cloud publish rejected", "status",resp.StatusCode)}}
func (p *CloudPublisher) PublishMetrics(ctx context.Context,payload MetricsPayload){if p!=nil{p.post(ctx,"/metrics",payload)}}
func (p *CloudPublisher) PublishNotification(ctx context.Context,payload notificationPayload){if p!=nil{p.post(ctx,"/notifications",payload)}}
