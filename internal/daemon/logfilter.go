package daemon

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Log upload filtering is deliberately fail-open: a line the parser cannot
// classify is treated as the highest severity and uploaded. The alternative
// would silently lose exactly the malformed output an operator needs to see.

type logLevel int

const (
	logLevelTrace logLevel = iota
	logLevelDebug
	logLevelInfo
	logLevelWarn
	logLevelError
	logLevelFatal
)

const maxUploadLogLineBytes = 4096

var textLogLevelPattern = regexp.MustCompile(`(?i)\b(trace|debug|info|notice|warn|warning|error|err|fatal|critical|crit|panic)\b`)

func parseUploadLevel(raw string) (logLevel, bool) {
	name, ok := config.NormalizeLogUploadLevel(raw)
	if !ok || name == "" {
		return logLevelTrace, false
	}
	return levelForName(name)
}

func levelForName(raw string) (logLevel, bool) {
	name, ok := config.NormalizeLogUploadLevel(raw)
	if !ok || name == "" {
		return 0, false
	}
	switch name {
	case "trace":
		return logLevelTrace, true
	case "debug":
		return logLevelDebug, true
	case "info":
		return logLevelInfo, true
	case "warn":
		return logLevelWarn, true
	case "fatal":
		return logLevelFatal, true
	default:
		return logLevelError, true
	}
}

// uploadLogLevel reads a JSON record first (level, severity, lvl, or a nested
// log/fields object), then falls back to severity words in plain text. A
// structured logger's numeric level (10..60, the pino/bunyan scale) is read as
// well. No recognizable level means "unknown", which callers upload.
func uploadLogLevel(line string) (logLevel, bool) {
	var value any
	if json.Unmarshal([]byte(line), &value) == nil {
		if level, ok := jsonLevel(value); ok {
			return level, true
		}
	}
	return textLevel(line)
}

func jsonLevel(value any) (logLevel, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	highest := logLevelTrace
	found := false
	consider := func(raw any) {
		if level, ok := levelFromValue(raw); ok && (!found || level > highest) {
			highest = level
			found = true
		}
	}
	for _, key := range []string{"level", "severity", "lvl", "log_level"} {
		consider(object[key])
	}
	for _, key := range []string{"log", "fields"} {
		if nested, ok := object[key].(map[string]any); ok {
			for _, field := range []string{"level", "severity", "lvl"} {
				consider(nested[field])
			}
		}
	}
	return highest, found
}

func levelFromValue(value any) (logLevel, bool) {
	switch raw := value.(type) {
	case string:
		trimmed := strings.TrimSpace(raw)
		if level, ok := levelForName(trimmed); ok {
			return level, true
		}
		if number, err := strconv.Atoi(trimmed); err == nil {
			return levelFromNumber(float64(number))
		}
		return textLevel(raw)
	case float64:
		return levelFromNumber(raw)
	default:
		return 0, false
	}
}

func levelFromNumber(value float64) (logLevel, bool) {
	switch {
	case value >= 60:
		return logLevelFatal, true
	case value >= 50:
		return logLevelError, true
	case value >= 40:
		return logLevelWarn, true
	case value >= 30:
		return logLevelInfo, true
	case value >= 20:
		return logLevelDebug, true
	case value >= 10:
		return logLevelTrace, true
	default:
		return 0, false
	}
}

func textLevel(line string) (logLevel, bool) {
	highest := logLevelTrace
	found := false
	for _, match := range textLogLevelPattern.FindAllString(line, -1) {
		if level, ok := levelForName(match); ok && (!found || level > highest) {
			highest = level
			found = true
		}
	}
	return highest, found
}

// shouldUploadLog applies the configured minimum level. No threshold and
// unparseable lines both pass, so enabling a filter never hides unknown
// output.
func shouldUploadLog(line, minimum string) bool {
	threshold, filtered := parseUploadLevel(minimum)
	if !filtered {
		return true
	}
	level, known := uploadLogLevel(line)
	if !known {
		return true
	}
	return level >= threshold
}

// sanitizeUploadLogLine bounds one line to the cloud's 4096-byte field. The
// cut is UTF-8 safe and the ellipsis is inside the budget.
func sanitizeUploadLogLine(line string) string {
	line = strings.ReplaceAll(line, "\r\n", "\n")
	line = strings.ToValidUTF8(strings.TrimRight(line, "\r"), "\uFFFD")
	if len(line) <= maxUploadLogLineBytes {
		return line
	}
	cut := maxUploadLogLineBytes - len("…")
	for cut > 0 && !utf8.ValidString(line[:cut]) {
		cut--
	}
	return line[:cut] + "…"
}
