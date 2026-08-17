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
	"sync"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// quotaRefreshInterval is how often the daemon re-fetches the workspace quota
// (GET /api/daemons/:id/quota) to re-tune its cloud traffic pacing. The quota
// GET itself is not throttled by the cloud.
const quotaRefreshInterval = 5 * time.Minute

// quotaRetryInterval is the backoff after a failed quota refresh, so a broken
// quota endpoint does not turn every paced request into a quota GET.
const quotaRetryInterval = time.Minute

type notificationPayload struct {
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Subtitle string         `json:"subtitle,omitempty"`
	Body     string         `json:"body"`
	Metadata map[string]any `json:"metadata"`
}

// CloudPublisher talks to the MaidKit cloud on behalf of the daemon: metrics,
// notifications and webhook-relay polling. All requests are authenticated
// with the daemon cloud secret and run over HTTPS (or localhost HTTP in
// development).
type CloudPublisher struct {
	baseURL  string
	daemonID string
	secret   string
	client   *http.Client
	timeout  time.Duration
	logger   *slog.Logger

	// Workspace quota pacing: the cloud throttles daemon-initiated metric
	// ingest and webhook-relay pickup to at most one request per
	// polling_interval_seconds per daemon (HTTP 429 otherwise). The daemon
	// paces exactly those two paths itself, sharing one slot like the cloud's
	// per-daemon bucket. A zero pollInterval disables pacing.
	paceMu         sync.Mutex
	pollInterval   time.Duration
	lastQuotaFetch time.Time
	lastPaced      time.Time
}

func NewCloudPublisher(cfg config.DaemonConfig, logger *slog.Logger) (*CloudPublisher, error) {
	if strings.TrimSpace(cfg.CloudURL) == "" || strings.TrimSpace(cfg.CloudSecret) == "" {
		return nil, nil
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.CloudURL), "/"))
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" &&
			!(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"))) {
		return nil, fmt.Errorf("cloudUrl must be HTTPS, or HTTP to localhost")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CloudPublisher{
		baseURL:  parsed.String(),
		daemonID: cfg.ID,
		secret:   cfg.CloudSecret,
		timeout:  cfg.RequestTimeout,
		logger:   logger,
		client: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (p *CloudPublisher) request(ctx context.Context, method, suffix string, payload any, dst any) error {
	if p == nil {
		return fmt.Errorf("cloud publisher is not configured")
	}
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+"/api/daemons/"+p.daemonID+suffix, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloud %s %s: status %d", method, suffix, resp.StatusCode)
	}
	if dst != nil {
		return json.NewDecoder(resp.Body).Decode(dst)
	}
	return nil
}

func (p *CloudPublisher) post(ctx context.Context, suffix string, payload any) {
	if err := p.request(ctx, http.MethodPost, suffix, payload, nil); err != nil {
		p.logger.Error("cloud publish failed", "suffix", suffix, "error", err)
	}
}

// pacedOK reports whether a cloud-throttled request may be sent now. It
// refreshes the workspace polling interval quota when stale and returns false
// while inside the interval window, so callers skip the request instead of
// sending one the cloud will 429. The window check and slot recording are
// atomic: metric ingest and relay pickup must not both pass the gate inside
// one window.
func (p *CloudPublisher) pacedOK(ctx context.Context) bool {
	p.refreshQuota(ctx)
	p.paceMu.Lock()
	defer p.paceMu.Unlock()
	if p.pollInterval <= 0 {
		return true
	}
	now := time.Now()
	if now.Sub(p.lastPaced) < p.pollInterval {
		return false
	}
	p.lastPaced = now
	return true
}

// refreshQuota pulls the workspace quota and applies the
// polling_interval_seconds dimension. A missing or non-positive dimension
// disables pacing, mirroring the cloud's enforcePollInterval. Failures keep
// the previous interval and retry after quotaRetryInterval.
func (p *CloudPublisher) refreshQuota(ctx context.Context) {
	p.paceMu.Lock()
	defer p.paceMu.Unlock()
	if !p.lastQuotaFetch.IsZero() && time.Since(p.lastQuotaFetch) < quotaRefreshInterval {
		return
	}
	quotas, err := p.WorkspaceQuota(ctx)
	if err != nil {
		p.logger.Warn("cloud quota refresh failed; keeping previous poll interval", "error", err)
		p.lastQuotaFetch = time.Now().Add(-(quotaRefreshInterval - quotaRetryInterval))
		return
	}
	p.lastQuotaFetch = time.Now()
	var interval time.Duration
	if secs := quotas["polling_interval_seconds"]; secs > 0 {
		interval = time.Duration(secs) * time.Second
	}
	p.pollInterval = interval
	if interval > 0 {
		p.logger.Debug("daemon cloud traffic paced by workspace quota", "interval", interval)
	}
}

func (p *CloudPublisher) PublishMetrics(ctx context.Context, payload MetricsPayload) {
	if p == nil {
		return
	}
	if !p.pacedOK(ctx) {
		p.logger.Debug("metric publish skipped: inside workspace poll interval")
		return
	}
	p.post(ctx, "/metrics", payload)
}

// WorkspaceQuota returns the connected workspace's effective quota map (plan
// preset + active addon grants) served by the cloud at GET /api/daemons/:id/quota,
// so the daemon can self-tune its polling and reporting cadence.
func (p *CloudPublisher) WorkspaceQuota(ctx context.Context) (map[string]int64, error) {
	if p == nil {
		return nil, fmt.Errorf("cloud publisher is not configured")
	}
	var out struct {
		WorkspaceID string           `json:"workspace_id"`
		Quotas      map[string]int64 `json:"quotas"`
	}
	if err := p.request(ctx, "GET", "/quota", nil, &out); err != nil {
		return nil, err
	}
	return out.Quotas, nil
}

// actionReportPayload is the wire shape of one configured action the daemon
// reports to the cloud for listing; invocation happens through the webhook
// relay, so no script body or secret ever leaves the host.
type actionReportPayload struct {
	Name            string `json:"name"`
	DisplayName     string `json:"display_name,omitempty"`
	Enabled         bool   `json:"enabled"`
	NotifyOnSuccess bool   `json:"notify_on_success"`
	NotifyOnFailure bool   `json:"notify_on_failure"`
	Timeout         string `json:"timeout,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	User            string `json:"user,omitempty"`
}

// PublishActions replaces the cloud's action list for this daemon with the
// current configuration, so the cloud always lists what the daemon can run.
func (p *CloudPublisher) PublishActions(ctx context.Context, actions []config.WebhookConfig) {
	if p == nil {
		return
	}
	payload := make([]actionReportPayload, 0, len(actions))
	for _, hook := range actions {
		timeout := ""
		if hook.Timeout > 0 {
			timeout = hook.Timeout.String()
		}
		payload = append(payload, actionReportPayload{
			Name:            hook.Name,
			DisplayName:     hook.DisplayName,
			Enabled:         hook.Enabled,
			NotifyOnSuccess: hook.NotifyOnSuccess,
			NotifyOnFailure: hook.NotifyOnFailure,
			Timeout:         timeout,
			Cwd:             hook.Cwd,
			User:            hook.User,
		})
	}
	p.post(ctx, "/actions", payload)
}
func (p *CloudPublisher) PublishNotification(ctx context.Context, payload notificationPayload) {
	if p != nil {
		p.post(ctx, "/notifications", payload)
	}
}
