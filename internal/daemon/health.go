package daemon

import (
	"fmt"
	"math"
	"time"
)

// Health status levels for the report's headline status.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthCritical = "critical"
)

// Check status levels for one dimension. `critical` is shared with the
// report status; `ok`/`warning` are check-only.
const (
	checkStatusOK       = "ok"
	checkStatusWarning  = "warning"
	checkStatusCritical = "critical"
)

// HealthCheck is one evaluated dimension of host health. Score is 0..100 for
// the dimension alone: 100 at or below Warn, decaying linearly to 0 at Crit.
// Skipped marks a dimension that does not apply on this host (no swap, no
// webhook runs, no disk data); a skipped check is excluded from the weighted
// report score.
type HealthCheck struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Score   float64 `json:"score"`
	Value   float64 `json:"value"`
	Unit    string  `json:"unit"`
	Detail  string  `json:"detail,omitempty"`
	Warn    float64 `json:"warn"`
	Crit    float64 `json:"crit"`
	Message string  `json:"message,omitempty"`
	Skipped bool    `json:"skipped,omitempty"`
}

// HealthReport is the daemon's own overview of host health: a weighted score
// over every scored dimension plus the per-dimension detail. It is served by
// GET /api/v1/health and travels to the cloud inside every metric sample as
// `health_score`/`health_status`, so the dashboard, the daemon API and the
// cloud all show one number computed in one place.
type HealthReport struct {
	Score       int           `json:"score"`
	Status      string        `json:"status"`
	EvaluatedAt time.Time     `json:"evaluated_at"`
	HostID      string        `json:"host_id,omitempty"`
	Checks      []HealthCheck `json:"checks"`
	Issues      []string      `json:"issues,omitempty"`
}

// healthSpec is one scored dimension: its warn/crit thresholds, its relative
// weight, and how to measure it from a metric sample. `measure` returns the
// observed value, an optional detail (the mount for disk), and whether the
// dimension applies on this host at all.
type healthSpec struct {
	name    string
	label   string
	unit    string
	warn    float64
	crit    float64
	weight  float64
	measure func(MetricsPayload) (value float64, detail string, ok bool)
}

// healthSpecs is the scoring model. Weights sum to 1 across all dimensions;
// evaluateHealth renormalizes them over the applicable ones, so a skipped
// check never silently deflates the score.
var healthSpecs = []healthSpec{
	{name: "cpu", label: "CPU", unit: "percent", warn: 75, crit: 95, weight: 0.22,
		measure: func(s MetricsPayload) (float64, string, bool) { return s.CPUPercent, "", true }},
	{name: "memory", label: "Memory", unit: "percent", warn: 80, crit: 95, weight: 0.24,
		measure: func(s MetricsPayload) (float64, string, bool) { return s.MemoryUsedPercent, "", true }},
	{name: "swap", label: "Swap", unit: "percent", warn: 50, crit: 90, weight: 0.10,
		measure: func(s MetricsPayload) (float64, string, bool) {
			if s.SwapTotalKb <= 0 {
				return 0, "", false
			}
			used := s.SwapTotalKb - s.SwapFreeKb
			return float64(used) / float64(s.SwapTotalKb) * 100, "", true
		}},
	{name: "disk", label: "Disk usage", unit: "percent", warn: 80, crit: 95, weight: 0.20,
		measure: worstDiskUsage},
	{name: "load", label: "Load", unit: "per_core", warn: 1, crit: 3, weight: 0.14,
		measure: func(s MetricsPayload) (float64, string, bool) {
			if s.CPUCount <= 0 {
				return 0, "", false
			}
			return s.Load1 / float64(s.CPUCount), "", true
		}},
	{name: "process_memory", label: "Process memory", unit: "bytes", warn: 512 << 20, crit: 2 << 30, weight: 0.05,
		measure: func(s MetricsPayload) (float64, string, bool) { return float64(s.ProcessMemoryBytes), "", true }},
	{name: "webhooks", label: "Webhook failures", unit: "ratio", warn: 0.10, crit: 0.50, weight: 0.05,
		measure: func(s MetricsPayload) (float64, string, bool) {
			if s.WebhookExecutions == 0 {
				return 0, "", false
			}
			return float64(s.WebhookFailures) / float64(s.WebhookExecutions), "", true
		}},
}

// evaluateHealth scores one metric sample. It is pure arithmetic over values
// already collected, so the metric tick, the SSE metric frames, the stdio
// `metrics` action and the local history all share it and agree.
func evaluateHealth(sample MetricsPayload, now time.Time) HealthReport {
	report := HealthReport{EvaluatedAt: now.UTC(), HostID: sample.HostID}
	var weighted, weights float64
	for _, spec := range healthSpecs {
		check := evaluateHealthCheck(spec, sample)
		report.Checks = append(report.Checks, check)
		if check.Skipped {
			continue
		}
		weighted += check.Score * spec.weight
		weights += spec.weight
		if check.Status != checkStatusOK {
			report.Issues = append(report.Issues, check.Message)
		}
	}
	report.Score = 100
	if weights > 0 {
		report.Score = int(math.Round(weighted / weights))
	}
	report.Status = healthStatusForScore(report.Score)
	return report
}

// evaluateHealthCheck measures and scores one dimension against its spec.
func evaluateHealthCheck(spec healthSpec, sample MetricsPayload) HealthCheck {
	value, detail, ok := spec.measure(sample)
	check := HealthCheck{
		Name:   spec.name,
		Unit:   spec.unit,
		Detail: detail,
		Warn:   spec.warn,
		Crit:   spec.crit,
		Value:  roundHealthValue(value),
	}
	if !ok {
		check.Status = checkStatusOK
		check.Score = 100
		check.Skipped = true
		return check
	}
	check.Score, check.Status = scoreHealthValue(value, spec.warn, spec.crit)
	if check.Status != checkStatusOK {
		check.Message = healthCheckMessage(spec, check)
	}
	return check
}

// scoreHealthValue maps a measurement to a 0..100 score and a status: 100 at
// or below warn, decaying linearly to 0 at crit and beyond. Status follows
// the same thresholds.
func scoreHealthValue(value, warn, crit float64) (float64, string) {
	switch {
	case value <= warn:
		return 100, checkStatusOK
	case value >= crit:
		return 0, checkStatusCritical
	default:
		return 100 * (crit - value) / (crit - warn), checkStatusWarning
	}
}

// healthStatusForScore bands the weighted score into the report's headline
// status. The bands are deliberately coarse; the per-check detail carries the
// nuance.
func healthStatusForScore(score int) string {
	switch {
	case score >= 90:
		return HealthHealthy
	case score >= 70:
		return HealthDegraded
	default:
		return HealthCritical
	}
}

// worstDiskUsage returns the highest used percentage across the sample's
// mounted filesystems, naming the mount as detail. When the per-mount list is
// absent (older samples) it falls back to the aggregate root fields.
func worstDiskUsage(sample MetricsPayload) (float64, string, bool) {
	worst, detail, ok := 0.0, "", false
	for _, usage := range sample.Disks {
		if usage.TotalKb <= 0 {
			continue
		}
		used := float64(usage.TotalKb-usage.AvailableKb) / float64(usage.TotalKb) * 100
		if !ok || used > worst {
			worst, detail, ok = used, usage.Mount, true
		}
	}
	if ok {
		return worst, detail, true
	}
	if sample.DiskTotalKb <= 0 {
		return 0, "", false
	}
	used := float64(sample.DiskTotalKb-sample.DiskAvailableKb) / float64(sample.DiskTotalKb) * 100
	return used, "", true
}

// healthCheckMessage renders the sentence for a dimension that crossed a
// threshold, e.g. "Memory at 96.1% (warn 80.0%, crit 95.0%)".
func healthCheckMessage(spec healthSpec, check HealthCheck) string {
	target := spec.label
	if check.Detail != "" {
		target += " " + check.Detail
	}
	return fmt.Sprintf("%s at %s (warn %s, crit %s)",
		target,
		formatHealthValue(spec.unit, check.Value),
		formatHealthValue(spec.unit, spec.warn),
		formatHealthValue(spec.unit, spec.crit),
	)
}

// formatHealthValue renders one measurement for a check message.
func formatHealthValue(unit string, value float64) string {
	switch unit {
	case "percent":
		return fmt.Sprintf("%.1f%%", value)
	case "per_core":
		return fmt.Sprintf("%.2f per core", value)
	case "ratio":
		return fmt.Sprintf("%.1f%%", value*100)
	case "bytes":
		return humanBytes(uint64(value))
	default:
		return fmt.Sprintf("%.1f", value)
	}
}

// roundHealthValue trims measurement noise so reports compare cleanly.
func roundHealthValue(value float64) float64 {
	return math.Round(value*10) / 10
}

// humanBytes renders a byte count in binary units, e.g. 1536 -> "1.5 KiB".
func humanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
