package daemon

import (
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

type MetricsPayload struct {
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

const metricHistoryLimit = 1440

type MetricsCollector struct {
	started  time.Time
	executor *WebhookExecutor
	mu       sync.RWMutex
	history  []MetricsPayload
}

func NewMetricsCollector(_ config.DaemonConfig, executor *WebhookExecutor) *MetricsCollector {
	return &MetricsCollector{
		started:  time.Now(),
		executor: executor,
		history:  make([]MetricsPayload, 0, metricHistoryLimit),
	}
}

// Record collects one snapshot and retains it for the history endpoint.
func (m *MetricsCollector) Record() MetricsPayload {
	payload := m.Collect()
	m.mu.Lock()
	m.history = append(m.history, payload)
	if len(m.history) > metricHistoryLimit {
		m.history = m.history[len(m.history)-metricHistoryLimit:]
	}
	m.mu.Unlock()
	return payload
}

// History returns the newest retained samples in chronological order.
func (m *MetricsCollector) History(limit int) []MetricsPayload {
	if limit <= 0 || limit > metricHistoryLimit {
		limit = metricHistoryLimit
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	start := len(m.history) - limit
	if start < 0 {
		start = 0
	}
	return append([]MetricsPayload(nil), m.history[start:]...)
}

func (m *MetricsCollector) Collect() MetricsPayload {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	successes, failures := m.executor.Counts()
	var cpuPercent float64
	if values, err := cpu.Percent(0, false); err == nil && len(values) > 0 {
		cpuPercent = values[0]
	}
	var memoryUsedPercent float64
	var memoryUsedBytes, memoryTotalBytes uint64
	if stats, err := mem.VirtualMemory(); err == nil {
		memoryUsedPercent = stats.UsedPercent
		memoryUsedBytes = stats.Used
		memoryTotalBytes = stats.Total
	}
	return MetricsPayload{
		SentAt:             time.Now().UTC(),
		UptimeSeconds:      int64(time.Since(m.started).Seconds()),
		ProcessMemoryBytes: int64(stats.Alloc),
		CPUPercent:         cpuPercent,
		MemoryUsedPercent:  memoryUsedPercent,
		MemoryUsedBytes:    memoryUsedBytes,
		MemoryTotalBytes:   memoryTotalBytes,
		WebhookExecutions:  successes + failures,
		WebhookFailures:    failures,
	}
}
