package cloud

import (
	"fmt"
	"strings"
)

var alarmMessages = map[string]map[string]string{
	"en": {
		"cpu_percent.title":         "CPU threshold exceeded",
		"cpu_percent.body":          "CPU usage reached {value}% (threshold {threshold}%).",
		"memory_used_percent.title": "Memory threshold exceeded",
		"memory_used_percent.body":  "Memory usage reached {value}% (threshold {threshold}%).",
		"disk_used_percent.title":   "Disk threshold exceeded",
		"disk_used_percent.body":    "Disk usage reached {value}% (threshold {threshold}%).",
		"container_down.title":      "Container is down",
		"container_down.body":       "Container {name} is {state}.",
		"disconnected.title":        "Daemon disconnected",
		"disconnected.body":         "No metrics received for {age}; last seen at {last_seen}.",
		"reconnected.title":         "Daemon reconnected",
		"reconnected.body":          "Metrics resumed after {downtime}.",
	},
	"zh-cn": {
		"cpu_percent.title":         "CPU 已超过阈值",
		"cpu_percent.body":          "CPU 使用率达到 {value}%（阈值 {threshold}%）。",
		"memory_used_percent.title": "内存已超过阈值",
		"memory_used_percent.body":  "内存使用率达到 {value}%（阈值 {threshold}%）。",
		"disk_used_percent.title":   "磁盘已超过阈值",
		"disk_used_percent.body":    "磁盘使用率达到 {value}%（阈值 {threshold}%）。",
		"container_down.title":      "容器已停止",
		"container_down.body":       "容器 {name} 当前状态为 {state}。",
		"disconnected.title":        "守护进程已断开连接",
		"disconnected.body":         "已有 {age} 未收到指标；最后上报时间为 {last_seen}。",
		"reconnected.title":         "守护进程已重新连接",
		"reconnected.body":          "指标已恢复上报，间隔 {downtime}。",
	},
	"zh-tw": {
		"cpu_percent.title":         "CPU 已超過閾值",
		"cpu_percent.body":          "CPU 使用率達到 {value}%（閾值 {threshold}%）。",
		"memory_used_percent.title": "記憶體已超過閾值",
		"memory_used_percent.body":  "記憶體使用率達到 {value}%（閾值 {threshold}%）。",
		"disk_used_percent.title":   "磁碟已超過閾值",
		"disk_used_percent.body":    "磁碟使用率達到 {value}%（閾值 {threshold}%）。",
		"container_down.title":      "容器已停止",
		"container_down.body":       "容器 {name} 目前狀態為 {state}。",
		"reconnected.title":         "守護程式已重新連線",
		"reconnected.body":          "指標已恢復回報，間隔 {downtime}。",
		"disconnected.title":        "守護程式已中斷連線",
		"disconnected.body":         "已有 {age} 未收到指標；最後回報時間為 {last_seen}。",
	},
}

func localizeAlarm(language, kind string, metadata map[string]any) (string, string, bool) {
	if strings.HasPrefix(kind, "daemon.alarm.") {
		kind = strings.TrimPrefix(kind, "daemon.alarm.")
	} else {
		kind = strings.TrimPrefix(kind, "daemon.")
	}
	locale := normalizeAlarmLocale(language)
	messages, ok := alarmMessages[locale]
	if !ok {
		messages = alarmMessages["en"]
	}
	title, titleOK := messages[kind+".title"]
	body, bodyOK := messages[kind+".body"]
	if !titleOK || !bodyOK {
		return "", "", false
	}
	args := map[string]string{
		"value":     formatAlarmValue(metadata["value"]),
		"threshold": formatAlarmValue(metadata["threshold"]),
		"name":      fmt.Sprint(metadata["container_name"]),
		"state":     fmt.Sprint(metadata["state"]),
		"age":       fmt.Sprint(metadata["age"]),
		"last_seen": fmt.Sprint(metadata["last_seen"]),
		"downtime":  fmt.Sprint(metadata["downtime"]),
	}
	for name, value := range args {
		title = strings.ReplaceAll(title, "{"+name+"}", value)
		body = strings.ReplaceAll(body, "{"+name+"}", value)
	}
	return title, body, true
}

func formatAlarmValue(value any) string {
	if number, ok := value.(float64); ok {
		return fmt.Sprintf("%.2f", number)
	}
	return fmt.Sprint(value)
}

func normalizeAlarmLocale(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	switch {
	case strings.HasPrefix(language, "zh-tw"), strings.HasPrefix(language, "zh-hk"):
		return "zh-tw"
	case strings.HasPrefix(language, "zh-cn"), strings.HasPrefix(language, "zh-sg"), strings.HasPrefix(language, "zh-hans"), strings.HasPrefix(language, "zh"):
		return "zh-cn"
	case strings.HasPrefix(language, "en"):
		return "en"
	default:
		return "en"
	}
}
