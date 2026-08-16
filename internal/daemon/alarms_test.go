package daemon

import (
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func alarmConfig(kind string, threshold float64, enabled bool, cooldown int) config.AlarmConfig {
	return config.AlarmConfig{Kind: kind, Threshold: threshold, Enabled: &enabled, CooldownSeconds: cooldown}
}

func TestAlarmEvaluatorFiresWhenThresholdExceeded(t *testing.T) {
	e := newAlarmEvaluator()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	sample := MetricsPayload{CPUPercent: 92.5, MemoryUsedPercent: 40}

	out := e.evaluate([]config.AlarmConfig{
		alarmConfig("cpu_percent", 80, true, 300),
		alarmConfig("memory_used_percent", 90, true, 300),
	}, sample, now)

	if len(out) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(out))
	}
	n := out[0]
	if n.Kind != "daemon.alarm.cpu_percent" {
		t.Errorf("kind = %q, want daemon.alarm.cpu_percent", n.Kind)
	}
	if n.Title != "cpu_percent threshold exceeded" {
		t.Errorf("title = %q", n.Title)
	}
	if want := "cpu_percent reached 92.50% (threshold 80.00%)"; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
	if n.Metadata["value"] != 92.5 || n.Metadata["threshold"] != 80.0 {
		t.Errorf("metadata = %v", n.Metadata)
	}
}

func TestAlarmEvaluatorHonorsCooldown(t *testing.T) {
	e := newAlarmEvaluator()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	alarms := []config.AlarmConfig{alarmConfig("cpu_percent", 80, true, 300)}
	sample := MetricsPayload{CPUPercent: 95}

	if got := e.evaluate(alarms, sample, now); len(got) != 1 {
		t.Fatalf("first tick should fire, got %d", len(got))
	}
	if got := e.evaluate(alarms, sample, now.Add(1*time.Minute)); len(got) != 0 {
		t.Fatalf("tick inside cooldown should not fire, got %d", len(got))
	}
	if got := e.evaluate(alarms, sample, now.Add(6*time.Minute)); len(got) != 1 {
		t.Fatalf("tick after cooldown should fire again, got %d", len(got))
	}
}

func TestAlarmEvaluatorRecoversBelowThreshold(t *testing.T) {
	e := newAlarmEvaluator()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	alarms := []config.AlarmConfig{alarmConfig("memory_used_percent", 80, true, 300)}

	if got := e.evaluate(alarms, MetricsPayload{MemoryUsedPercent: 90}, now); len(got) != 1 {
		t.Fatalf("high tick should fire, got %d", len(got))
	}
	if got := e.evaluate(alarms, MetricsPayload{MemoryUsedPercent: 50}, now.Add(1*time.Minute)); len(got) != 0 {
		t.Fatalf("recovered tick should not fire, got %d", len(got))
	}
	// Back above threshold after recovery: cooldown restarts from the new
	// trigger, so a same-minute spike is suppressed.
	if got := e.evaluate(alarms, MetricsPayload{MemoryUsedPercent: 95}, now.Add(1*time.Minute)); len(got) != 0 {
		t.Fatalf("immediate re-trigger after recovery should be cooled down, got %d", len(got))
	}
}

func TestAlarmEvaluatorSkipsDisabledAlarms(t *testing.T) {
	e := newAlarmEvaluator()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	disabled := config.AlarmConfig{Kind: "cpu_percent", Threshold: 10, Enabled: boolPtr(false), CooldownSeconds: 300}
	absent := config.AlarmConfig{Kind: "memory_used_percent", Threshold: 10, CooldownSeconds: 300}

	out := e.evaluate([]config.AlarmConfig{disabled, absent}, MetricsPayload{CPUPercent: 99, MemoryUsedPercent: 99}, now)

	if len(out) != 1 {
		t.Fatalf("expected only the enabled-by-default alarm to fire, got %d", len(out))
	}
	if out[0].Kind != "daemon.alarm.memory_used_percent" {
		t.Errorf("kind = %q", out[0].Kind)
	}
}

func boolPtr(value bool) *bool { return &value }
