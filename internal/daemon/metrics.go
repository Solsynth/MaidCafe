package daemon

import (
	"runtime"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type MetricsPayload struct {
	SentAt time.Time `json:"sent_at"`
	UptimeSeconds int64 `json:"uptime_seconds"`
	ProcessMemoryBytes int64 `json:"process_memory_bytes"`
	WebhookExecutions uint64 `json:"webhook_executions"`
	WebhookFailures uint64 `json:"webhook_failures"`
}
type MetricsCollector struct { started time.Time; executor *WebhookExecutor }
func NewMetricsCollector(_ config.DaemonConfig, executor *WebhookExecutor) *MetricsCollector { return &MetricsCollector{started:time.Now(),executor:executor} }
func (m *MetricsCollector) Collect() MetricsPayload { var stats runtime.MemStats;runtime.ReadMemStats(&stats);successes,failures:=m.executor.Counts();return MetricsPayload{SentAt:time.Now().UTC(),UptimeSeconds:int64(time.Since(m.started).Seconds()),ProcessMemoryBytes:int64(stats.Alloc),WebhookExecutions:successes+failures,WebhookFailures:failures} }
