package daemon

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"time"
)

// webhookRelayInterval is how often the daemon asks the MaidKit cloud for
// pending webhook invocations. Polling is deliberate: no long-lived
// connections or push channels are maintained with the cloud, since those
// would need an encrypted transport of their own.
const webhookRelayInterval = time.Minute

type relayWebhookRequest struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Body      string `json:"body"` // base64
	Signature string `json:"signature"`
	// InvokedBy names the cloud caller (user handle or credential label).
	InvokedBy string `json:"invoked_by"`
}

type relayPendingResponse struct {
	Requests []relayWebhookRequest `json:"requests"`
}

type relayResultPayload struct {
	Code  int    `json:"code"`
	Body  string `json:"body"` // base64 stdout
	Error string `json:"error"`
}

// WebhookRelay polls the MaidKit cloud for webhook invocations the client
// enqueued through the cloud relay, executes them locally with the same
// signature verification as the HTTP endpoint, and reports the results back.
type WebhookRelay struct {
	publisher *CloudPublisher
	executor  *WebhookExecutor
	logger    *slog.Logger
}

// NewWebhookRelay returns nil when the cloud relay is not configured.
func NewWebhookRelay(publisher *CloudPublisher, executor *WebhookExecutor, logger *slog.Logger) *WebhookRelay {
	if publisher == nil || executor == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookRelay{publisher: publisher, executor: executor, logger: logger}
}

// Run polls until ctx is cancelled, starting with an immediate poll.
func (r *WebhookRelay) Run(ctx context.Context) {
	ticker := time.NewTicker(webhookRelayInterval)
	defer ticker.Stop()
	r.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollOnce(ctx)
		}
	}
}

func (r *WebhookRelay) pollOnce(ctx context.Context) {
	var pending relayPendingResponse
	if err := r.publisher.request(ctx, "GET", "/webhook-requests/pending", nil, &pending); err != nil {
		r.logger.Warn("webhook relay poll failed", "error", err)
		return
	}
	for _, request := range pending.Requests {
		r.process(ctx, request)
	}
}

func (r *WebhookRelay) process(ctx context.Context, request relayWebhookRequest) {
	body, err := base64.StdEncoding.DecodeString(request.Body)
	if err != nil {
		r.report(ctx, request.ID, relayResultPayload{Error: "invalid request body encoding"})
		return
	}
	response, status := r.executor.ExecuteWebhook(request.Name, body, request.Signature, "relay", request.InvokedBy)
	result := relayResultPayload{Code: status}
	if response.OK {
		result.Body = base64.StdEncoding.EncodeToString([]byte(response.Stdout))
	} else if response.Stderr != "" {
		result.Error = response.Stderr
	} else if status != http.StatusOK {
		result.Error = "webhook execution failed"
	}
	r.report(ctx, request.ID, result)
}

func (r *WebhookRelay) report(ctx context.Context, id string, result relayResultPayload) {
	if err := r.publisher.request(ctx, "POST", "/webhook-requests/"+id+"/result", result, nil); err != nil {
		r.logger.Warn("webhook relay result failed", "request", id, "error", err)
	}
}
