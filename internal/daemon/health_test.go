package daemon

import (
	"testing"
	"time"
)

func TestScoreHealthValueThresholds(t *testing.T) {
	cases := []struct {
		value       float64
		wantScore   float64
		wantStatus  string
		description string
	}{
		{0, 100, checkStatusOK, "well below warn"},
		{80, 100, checkStatusOK, "exactly at warn scores 100"},
		{87.5, 50, checkStatusWarning, "midway between warn and crit"},
		{95, 0, checkStatusCritical, "exactly at crit scores 0"},
		{100, 0, checkStatusCritical, "beyond crit clamps to 0"},
	}
	for _, tc := range cases {
		score, status := scoreHealthValue(tc.value, 80, 95)
		if score != tc.wantScore || status != tc.wantStatus {
			t.Errorf("%s: scoreHealthValue(%v) = %v/%s, want %v/%s",
				tc.description, tc.value, score, status, tc.wantScore, tc.wantStatus)
		}
	}
}

func TestHealthStatusForScoreBands(t *testing.T) {
	cases := map[int]string{100: HealthHealthy, 90: HealthHealthy, 89: HealthDegraded, 70: HealthDegraded, 69: HealthCritical, 0: HealthCritical}
	for score, want := range cases {
		if got := healthStatusForScore(score); got != want {
			t.Errorf("healthStatusForScore(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestEvaluateHealthHealthySample(t *testing.T) {
	sample := MetricsPayload{
		HostID:            "host-1",
		CPUPercent:        10,
		CPUCount:          4,
		MemoryUsedPercent: 40,
		SwapTotalKb:       1024,
		SwapFreeKb:        900,
		Disks:             []DiskUsage{{Mount: "/", TotalKb: 1000, AvailableKb: 500}},
		Load1:             0.5,
		WebhookExecutions: 10,
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	report := evaluateHealth(sample, now)

	if report.Score != 100 || report.Status != HealthHealthy {
		t.Fatalf("healthy sample scored %d/%s, want 100/healthy", report.Score, report.Status)
	}
	if len(report.Issues) != 0 {
		t.Fatalf("healthy sample reported issues: %v", report.Issues)
	}
	if !report.EvaluatedAt.Equal(now) || report.HostID != "host-1" {
		t.Fatalf("report metadata mismatch: %+v", report)
	}
	if len(report.Checks) != len(healthSpecs) {
		t.Fatalf("got %d checks, want one per dimension (%d)", len(report.Checks), len(healthSpecs))
	}
	for _, check := range report.Checks {
		if check.Skipped {
			t.Errorf("check %q unexpectedly skipped", check.Name)
		}
		if check.Score != 100 {
			t.Errorf("check %q scored %v, want 100", check.Name, check.Score)
		}
	}
}

// TestEvaluateHealthCriticalSample pins the weighted arithmetic: three
// critical dimensions pull the score down while the healthy ones keep it from
// reaching zero, and each crossed threshold contributes one issue sentence.
func TestEvaluateHealthCriticalSample(t *testing.T) {
	sample := MetricsPayload{
		CPUPercent:        100,
		CPUCount:          4,
		MemoryUsedPercent: 100,
		Disks:             []DiskUsage{{Mount: "/data", TotalKb: 100, AvailableKb: 4}},
		Load1:             4,
	}
	report := evaluateHealth(sample, time.Unix(0, 0))

	// weights: cpu .22 + memory .24 + disk .20 + load .14 + process_memory .05
	// scores:  0       0          0        100        100
	// (0*.22 + 0*.24 + 0*.20 + 100*.14 + 100*.05) / 0.85 = 22.35 -> 22
	if report.Score != 22 {
		t.Fatalf("critical sample scored %d, want 22", report.Score)
	}
	if report.Status != HealthCritical {
		t.Fatalf("critical sample status = %q, want critical", report.Status)
	}
	if len(report.Issues) != 3 {
		t.Fatalf("issued %d problems (%v), want 3", len(report.Issues), report.Issues)
	}
	disk := checkByName(t, report, "disk")
	if disk.Status != checkStatusCritical || disk.Detail != "/data" {
		t.Fatalf("disk check = %+v, want critical on /data", disk)
	}
	if disk.Message == "" {
		t.Fatal("critical disk check carries no message")
	}
	// Swap and webhooks do not apply to this sample and must be excluded from
	// the score rather than counted as healthy.
	for _, name := range []string{"swap", "webhooks"} {
		if !checkByName(t, report, name).Skipped {
			t.Errorf("check %q should be skipped for this sample", name)
		}
	}
}

// TestEvaluateHealthWarningMessage proves a dimension between its thresholds
// reports a warning with a rendered threshold sentence.
func TestEvaluateHealthWarningMessage(t *testing.T) {
	sample := MetricsPayload{CPUPercent: 85, CPUCount: 4, MemoryUsedPercent: 10}
	report := evaluateHealth(sample, time.Unix(0, 0))
	cpu := checkByName(t, report, "cpu")
	if cpu.Status != checkStatusWarning {
		t.Fatalf("cpu status = %q, want warning", cpu.Status)
	}
	if cpu.Value != 85 || cpu.Warn != 75 || cpu.Crit != 95 {
		t.Fatalf("cpu check thresholds/value = %+v", cpu)
	}
	if len(report.Issues) != 1 {
		t.Fatalf("issues = %v, want exactly the cpu warning", report.Issues)
	}
}

func TestWorstDiskUsagePicksWorstMount(t *testing.T) {
	sample := MetricsPayload{Disks: []DiskUsage{
		{Mount: "/", TotalKb: 1000, AvailableKb: 900},
		{Mount: "/var", TotalKb: 1000, AvailableKb: 50},
		{Mount: "/srv", TotalKb: 0, AvailableKb: 0},
	}}
	value, detail, ok := worstDiskUsage(sample)
	if !ok || detail != "/var" {
		t.Fatalf("worstDiskUsage = %v/%q/%v, want 95%% on /var", value, detail, ok)
	}
	if value < 94.9 || value > 95.1 {
		t.Fatalf("worstDiskUsage value = %v, want 95", value)
	}
}

// TestWorstDiskUsageFallsBackToAggregate covers samples without a per-mount
// list (older history rows), and the no-data case.
func TestWorstDiskUsageFallsBackToAggregate(t *testing.T) {
	value, _, ok := worstDiskUsage(MetricsPayload{DiskTotalKb: 200, DiskAvailableKb: 50})
	if !ok || value != 75 {
		t.Fatalf("aggregate fallback = %v/%v, want 75/true", value, ok)
	}
	if _, _, ok := worstDiskUsage(MetricsPayload{}); ok {
		t.Fatal("empty sample should not produce a disk measurement")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:       "0 B",
		512:     "512 B",
		1536:    "1.5 KiB",
		1 << 20: "1.0 MiB",
		3 << 30: "3.0 GiB",
	}
	for value, want := range cases {
		if got := humanBytes(value); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", value, got, want)
		}
	}
}

func checkByName(t *testing.T, report HealthReport, name string) HealthCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q missing from report", name)
	return HealthCheck{}
}
