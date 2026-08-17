package daemon

import (
	"fmt"
	"sync"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// alarmEvaluator decides, per metric tick, which configured alarms have
// crossed their threshold and are outside their cooldown, and turns each
// trigger into the notification payload the cloud forwards as-is.
type alarmEvaluator struct {
	mu    sync.Mutex
	state map[string]time.Time
}

func newAlarmEvaluator() *alarmEvaluator {
	return &alarmEvaluator{state: make(map[string]time.Time)}
}

func (e *alarmEvaluator) evaluate(alarms []config.AlarmConfig, sample MetricsPayload, now time.Time) []notificationPayload {
	var out []notificationPayload
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, alarm := range alarms {
		if alarm.Enabled != nil && !*alarm.Enabled {
			continue
		}
		var value float64
		switch alarm.Kind {
		case "cpu_percent":
			value = sample.CPUPercent
		case "memory_used_percent":
			value = sample.MemoryUsedPercent
		case "disk_used_percent":
			if sample.DiskTotalKb <= 0 || sample.DiskAvailableKb < 0 || sample.DiskAvailableKb > sample.DiskTotalKb {
				continue
			}
			value = 100 * float64(sample.DiskTotalKb-sample.DiskAvailableKb) / float64(sample.DiskTotalKb)
		default:
			continue
		}
		if value < alarm.Threshold {
			continue
		}
		key := alarm.Kind + "\x00" + alarm.Target
		if !e.ready(key, alarm.CooldownSeconds, now) {
			continue
		}
		out = append(out, notificationPayload{
			Kind:     "daemon.alarm." + alarm.Kind,
			Title:    fmt.Sprintf("%s threshold exceeded", alarm.Kind),
			Body:     fmt.Sprintf("%s reached %.2f%% (threshold %.2f%%)", alarm.Kind, value, alarm.Threshold),
			Metadata: map[string]any{"value": value, "threshold": alarm.Threshold},
		})
	}
	return out
}

func (e *alarmEvaluator) evaluateContainers(alarms []config.AlarmConfig, sample containersPayload, now time.Time) []notificationPayload {
	var out []notificationPayload
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, alarm := range alarms {
		if alarm.Kind != "container_down" || alarm.Enabled != nil && !*alarm.Enabled {
			continue
		}
		for _, runtime := range sample.Runtimes {
			if !runtime.Available || runtime.Error != nil {
				continue
			}
			for _, container := range runtime.Containers {
				if alarm.Target != "" && alarm.Target != container.Name && alarm.Target != container.ID {
					continue
				}
				if container.State == "running" {
					continue
				}
				key := "container_down\x00" + alarm.Target + "\x00" + container.ID
				if !e.ready(key, alarm.CooldownSeconds, now) {
					continue
				}
				out = append(out, notificationPayload{
					Kind:  "daemon.alarm.container_down",
					Title: "container_down alarm",
					Body:  fmt.Sprintf("container %s is %s", container.Name, container.State),
					Metadata: map[string]any{
						"container_id":   container.ID,
						"container_name": container.Name,
						"state":          container.State,
						"runtime":        runtime.Runtime,
					},
				})
			}
		}
	}
	return out
}

func (e *alarmEvaluator) ready(key string, cooldownSeconds int, now time.Time) bool {
	cooldown := time.Duration(cooldownSeconds) * time.Second
	if last, ok := e.state[key]; ok && now.Sub(last) < cooldown {
		return false
	}
	e.state[key] = now
	return true
}
