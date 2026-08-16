package daemon

import (
	"fmt"
	"sync"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// alarmEvaluator decides, per metric tick, which configured alarms have
// crossed their threshold and are outside their cooldown, and turns each
// trigger into the notification payload the cloud forwards as-is. Evaluation
// is daemon-side on purpose: the daemon reports `daemon.alarm.<kind>` through
// the same daemon-authenticated notification endpoint it already uses for
// webhook results, so the cloud never needs to reach back into the daemon.
type alarmEvaluator struct {
	mu    sync.Mutex
	state map[string]time.Time
}

func newAlarmEvaluator() *alarmEvaluator {
	return &alarmEvaluator{state: make(map[string]time.Time)}
}

// evaluate returns the notifications to report for one metric sample. An
// alarm fires when the sample value is at or above its threshold and the
// cooldown since the last trigger has elapsed. Trigger state is in-memory: a
// daemon restart may re-fire an alarm once while the metric stays over
// threshold, which a multi-minute cooldown makes acceptable.
func (e *alarmEvaluator) evaluate(alarms []config.AlarmConfig, sample MetricsPayload, now time.Time) []notificationPayload {
	var out []notificationPayload
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, alarm := range alarms {
		if alarm.Enabled != nil && !*alarm.Enabled {
			continue
		}
		value := sample.CPUPercent
		if alarm.Kind == "memory_used_percent" {
			value = sample.MemoryUsedPercent
		}
		if value < alarm.Threshold {
			continue
		}
		cooldown := time.Duration(alarm.CooldownSeconds) * time.Second
		if last, ok := e.state[alarm.Kind]; ok && now.Sub(last) < cooldown {
			continue
		}
		e.state[alarm.Kind] = now
		out = append(out, notificationPayload{
			Kind:     "daemon.alarm." + alarm.Kind,
			Title:    fmt.Sprintf("%s threshold exceeded", alarm.Kind),
			Body:     fmt.Sprintf("%s reached %.2f%% (threshold %.2f%%)", alarm.Kind, value, alarm.Threshold),
			Metadata: map[string]any{"value": value, "threshold": alarm.Threshold},
		})
	}
	return out
}
