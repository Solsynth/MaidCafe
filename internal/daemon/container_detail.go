package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Container detail reads.
//
// These are the daemon-side half of what the runtime CLI can tell you about a
// container beyond its lifecycle: what it was created from, what it is doing
// right now, and what it logged. They read through the same runtime probe and
// the same never-interactive `sudo -n` retry the collectors use, so a non-root
// daemon can see rootful containers, and every payload is bounded — a detail
// request must never be able to make the daemon buffer an unbounded amount of
// a container's output.

const (
	// containerInspectBytes bounds one inspect payload. A container with many
	// mounts, labels and environment entries still fits well inside this.
	containerInspectBytes = 1 << 20
	// containerStatsBytes bounds one one-shot stats payload.
	containerStatsBytes = 64 << 10
	// containerStatsTimeout is longer than the collectors' bound: a one-shot
	// stats read samples the container, and a busy host can take several
	// seconds to answer.
	containerStatsTimeout = 15 * time.Second
)

// containerRef identifies one container on one runtime: what the daemon needs
// to run a runtime command against it.
type containerRef struct {
	Runtime string // "podman" or "docker"
	Path    string // resolved runtime binary
	ID      string
	Name    string
}

// containersForRead resolves every container the daemon currently sees to its
// runtime binary, in probe order (podman first). Resolution goes through the
// containers collector's own snapshot, so a detail request reuses the list the
// daemon already keeps instead of probing the host again per request.
func (a *App) containersForRead(ctx context.Context) ([]containerRef, error) {
	data, err := a.containers.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var payload containersPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("container snapshot: %w", err)
	}
	paths := a.containers.runtimePaths(ctx)
	refs := make([]containerRef, 0, 8)
	for _, runtime := range payload.Runtimes {
		path := paths[runtime.Runtime]
		if path == "" {
			continue
		}
		for _, entry := range runtime.Containers {
			refs = append(refs, containerRef{
				Runtime: runtime.Runtime, Path: path, ID: entry.ID, Name: entry.Name,
			})
		}
	}
	return refs, nil
}

// resolveContainer finds the container [ref] names — a full id, an id prefix
// or a name — in the current container set, podman first. The snapshot is the
// daemon's own source of truth for which runtime holds what, so a detail
// request never has to guess which runtime to ask, and a container that is not
// there is a clean not-found instead of a runtime error.
func (a *App) resolveContainer(ctx context.Context, ref string) (containerRef, bool, error) {
	ref = strings.TrimSpace(ref)
	if !nativeContainerRefPattern.MatchString(ref) {
		return containerRef{}, false, nil
	}
	refs, err := a.containersForRead(ctx)
	if err != nil {
		return containerRef{}, false, err
	}
	for _, candidate := range refs {
		switch {
		case candidate.ID == ref, candidate.Name == ref:
			return candidate, true, nil
		}
	}
	for _, candidate := range refs {
		if strings.HasPrefix(candidate.ID, ref) {
			return candidate, true, nil
		}
	}
	return containerRef{}, false, nil
}

// runRuntimeRead runs one read-only runtime command, retrying through `sudo
// -n` when the direct invocation fails and elevating that runtime means
// anything (see elevationAttempt) — the same never-interactive elevation the
// collectors use, and the reason a detail request works against a rootful
// runtime behind a non-root daemon. The runtime's own stderr becomes the
// error: "no such container" is actionable, "exit status 125" is not.
func runRuntimeRead(ctx context.Context, timeout time.Duration, limit int, command string, args ...string) ([]byte, error) {
	stdout, stderr, err := runReadOnce(ctx, timeout, limit, command, args...)
	if err == nil {
		return stdout, nil
	}
	if elevated, ok := elevationAttempt(ctx, command, args...); ok {
		if elevatedOut, _, elevatedErr := runReadOnce(ctx, timeout, limit, elevated[0], elevated[1:]...); elevatedErr == nil {
			return elevatedOut, nil
		}
	}
	if message := strings.TrimSpace(stderr); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	return nil, fmt.Errorf("%s failed: %w", command, err)
}

// runReadOnce runs one command, capturing stdout up to [limit] bytes and
// stderr up to a short diagnostic bound.
func runReadOnce(ctx context.Context, timeout time.Duration, limit int, command string, args ...string) ([]byte, string, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, command, args...)
	stdout, stderr := &limitedBuffer{limit: limit}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	return []byte(stdout.String()), stderr.String(), err
}

// containerInspect is a container's own configuration as its runtime reports
// it: the raw payload for callers that want everything, plus the three fields
// the daemon resolves itself — the image reference the container was created
// from, the image ID it runs, and its labels (compose identity among them).
type containerInspect struct {
	JSON     json.RawMessage
	ImageRef string
	ImageID  string
	Labels   map[string]string
}

// inspectContainer reads one container's inspect payload.
func inspectContainer(ctx context.Context, path, id string) (containerInspect, error) {
	out, err := runRuntimeRead(ctx, collectorExecTimeout, containerInspectBytes, path, "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return containerInspect{}, err
	}
	return parseContainerInspect(out)
}

// parseContainerInspect reads the inspect object out of a runtime's answer.
// Docker renders one object per container through its template engine; podman
// answers with the inspect JSON itself, which may be an object or an array.
func parseContainerInspect(out []byte) (containerInspect, error) {
	object, raw, err := parseJSONObject(out)
	if err != nil {
		return containerInspect{}, err
	}
	config := jsonObject(object["Config"])
	// Config.Labels is authoritative — it is what the container was created
	// with — and the top-level Labels field (docker's list format) fills in
	// anything it does not carry.
	labels := jsonStringMap(object["Labels"])
	for key, value := range jsonStringMap(config["Labels"]) {
		labels[key] = value
	}
	// The reference the container was created from: podman records the name it
	// resolved at the top level, docker records it in the container's config,
	// and a container created from an image ID has neither.
	imageRef := jsonString(object["ImageName"])
	if imageRef == "" || strings.HasPrefix(imageRef, "sha256:") {
		imageRef = jsonString(config["Image"])
	}
	if strings.HasPrefix(imageRef, "sha256:") {
		imageRef = ""
	}
	return containerInspect{
		JSON:     raw,
		ImageRef: imageRef,
		ImageID:  jsonString(object["Image"]),
		Labels:   labels,
	}, nil
}

// containerImageInfo is the local image store's answer for one reference: the
// image ID it holds and the digests it was pulled at.
type containerImageInfo struct {
	ID          string
	RepoDigests []string
}

// imageInfo reads one image's inspect payload from the local store.
func imageInfo(ctx context.Context, path, ref string) (containerImageInfo, error) {
	out, err := runRuntimeRead(ctx, collectorExecTimeout, containerInspectBytes, path, "image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return containerImageInfo{}, err
	}
	object, _, err := parseJSONObject(out)
	if err != nil {
		return containerImageInfo{}, err
	}
	id := jsonString(object["Id"])
	if id == "" {
		id = jsonString(object["ID"])
	}
	digests := make([]string, 0, 2)
	if raw, ok := object["RepoDigests"].([]any); ok {
		for _, entry := range raw {
			if value := jsonString(entry); value != "" {
				digests = append(digests, value)
			}
		}
	}
	return containerImageInfo{ID: id, RepoDigests: digests}, nil
}

// parseJSONObject reads one JSON object out of a runtime's output, tolerating
// the shapes the two runtimes emit: a bare object, an array of objects (podman
// inspect and image inspect), or a first line followed by more (docker renders
// one line per object).
func parseJSONObject(out []byte) (map[string]any, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("runtime returned no data")
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err == nil {
		return object, json.RawMessage(append([]byte(nil), trimmed...)), nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err == nil {
		if len(list) == 0 {
			return nil, nil, fmt.Errorf("runtime returned an empty list")
		}
		if err := json.Unmarshal(list[0], &object); err != nil {
			return nil, nil, fmt.Errorf("runtime returned an unexpected payload: %w", err)
		}
		return object, list[0], nil
	}
	if line, _, _ := bytes.Cut(trimmed, []byte("\n")); len(line) > 0 {
		if err := json.Unmarshal(line, &object); err == nil {
			return object, json.RawMessage(append([]byte(nil), line...)), nil
		}
	}
	return nil, nil, fmt.Errorf("runtime returned an unexpected payload")
}

// jsonObject returns [value] as an object, or an empty map.
func jsonObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// jsonString returns [value] as a string, or "".
func jsonString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// jsonStringMap converts a JSON object of strings into a Go map, skipping
// entries that are not strings.
func jsonStringMap(value any) map[string]string {
	out := map[string]string{}
	object, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for key, entry := range object {
		if text, ok := entry.(string); ok {
			out[key] = text
		}
	}
	return out
}

// containerStats is one container's resource usage, normalized across the two
// runtimes. Every measurement is optional: a rootless runtime reports "--" for
// what it cannot see, and null says that honestly where 0 would claim the
// container used nothing.
type containerStats struct {
	Container          string    `json:"container"`
	Name               string    `json:"name"`
	Runtime            string    `json:"runtime"`
	CPUPercent         *float64  `json:"cpu_percent"`
	MemoryUsageBytes   *int64    `json:"memory_usage_bytes"`
	MemoryLimitBytes   *int64    `json:"memory_limit_bytes"`
	MemoryPercent      *float64  `json:"memory_percent"`
	NetworkInputBytes  *int64    `json:"network_input_bytes"`
	NetworkOutputBytes *int64    `json:"network_output_bytes"`
	BlockInputBytes    *int64    `json:"block_input_bytes"`
	BlockOutputBytes   *int64    `json:"block_output_bytes"`
	PIDs               *int      `json:"pids"`
	FetchedAt          time.Time `json:"fetched_at"`
}

// statsArgs builds the one-shot stats invocation for a runtime. Podman has a
// dedicated JSON format; docker renders the stats document through its
// template engine. Both feed parseContainerStats.
func statsArgs(runtime, id string) []string {
	if runtime == "podman" {
		return []string{"stats", "--no-stream", "--format=json", id}
	}
	return []string{"stats", "--no-stream", "--format", "{{json .}}", id}
}

// containerStatsFor reads one container's current resource usage.
func containerStatsFor(ctx context.Context, ref containerRef) (containerStats, error) {
	out, err := runRuntimeRead(ctx, containerStatsTimeout, containerStatsBytes, ref.Path, statsArgs(ref.Runtime, ref.ID)...)
	if err != nil {
		return containerStats{}, err
	}
	return parseContainerStats(out, ref)
}

// parseContainerStats normalizes a stats payload. Docker answers with one
// object keyed "CPUPerc"/"MemUsage"/…; podman's JSON format answers with an
// array keyed "cpu_percent"/"mem_usage"/… (and "--" where it has no answer).
func parseContainerStats(out []byte, ref containerRef) (containerStats, error) {
	var list []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		var single map[string]any
		if singleErr := json.Unmarshal(bytes.TrimSpace(out), &single); singleErr != nil {
			return containerStats{}, fmt.Errorf("runtime returned an unexpected stats payload")
		}
		list = []map[string]any{single}
	}
	if len(list) == 0 {
		return containerStats{}, fmt.Errorf("runtime returned no stats")
	}
	values := map[string]string{}
	for key, value := range list[0] {
		values[key] = statValueString(value)
	}
	stats := containerStats{
		Container: ref.ID,
		Name:      ref.Name,
		Runtime:   ref.Runtime,
		FetchedAt: time.Now().UTC(),
	}
	if name := statLookup(values, "Name", "name"); name != "" {
		stats.Name = name
	}
	stats.CPUPercent = parseStatPercent(statLookup(
		values, "CPUPerc", "cpu_percent", "CPU",
	))
	memoryUsage, memoryLimit := parseStatPair(statLookup(
		values, "MemUsage", "mem_usage", "MemUsageBytes",
	))
	stats.MemoryUsageBytes, stats.MemoryLimitBytes = memoryUsage, memoryLimit
	stats.MemoryPercent = parseStatPercent(statLookup(
		values, "MemPerc", "mem_percent",
	))
	stats.NetworkInputBytes, stats.NetworkOutputBytes = parseStatPair(statLookup(
		values, "NetIO", "netio", "net_io",
	))
	stats.BlockInputBytes, stats.BlockOutputBytes = parseStatPair(statLookup(
		values, "BlockIO", "blocki", "block_io", "BlockIOBytes",
	))
	if pids := statLookup(values, "PIDs", "PIDS", "pids"); pids != "" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(pids)); err == nil {
			stats.PIDs = &parsed
		}
	}
	return stats, nil
}

// statValueString renders a stats field as a string: both runtimes report
// measurements the same way, as strings, but a JSON number is accepted too.
func statValueString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

// statLookup returns the first non-empty value among [keys], matched without
// regard to case so one key list covers both runtimes' spellings.
func statLookup(values map[string]string, keys ...string) string {
	for _, key := range keys {
		for candidate, value := range values {
			if strings.EqualFold(candidate, key) && value != "" {
				return value
			}
		}
	}
	return ""
}

// parseStatPercent reads a percentage like "0.02%", and treats the runtimes'
// "--" as no answer rather than as zero.
func parseStatPercent(raw string) *float64 {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if raw == "" || strings.HasPrefix(raw, "-") {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return &value
}

// parseStatPair reads a runtime's "a / b" pair (memory usage and limit,
// network or block traffic) into two optional byte counts.
func parseStatPair(raw string) (*int64, *int64) {
	if raw == "" || strings.HasPrefix(strings.TrimSpace(raw), "-") {
		return nil, nil
	}
	left, right, found := strings.Cut(raw, "/")
	if !found {
		return parseStatBytes(raw), nil
	}
	return parseStatBytes(left), parseStatBytes(right)
}

// statUnitBytes maps the unit suffixes both runtimes print to a multiplier.
// The distinction is preserved: docker prints IEC units (MiB) and podman
// prints SI ones (MB), and they are not the same number of bytes.
var statUnitBytes = map[string]float64{
	"":    1,
	"b":   1,
	"k":   1e3,
	"kb":  1e3,
	"kib": 1 << 10,
	"m":   1e6,
	"mb":  1e6,
	"mib": 1 << 20,
	"g":   1e9,
	"gb":  1e9,
	"gib": 1 << 30,
	"t":   1e12,
	"tb":  1e12,
	"tib": 1 << 40,
}

// parseStatBytes reads a human-readable byte size ("3.092MB", "1.5MiB").
func parseStatBytes(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-") {
		return nil
	}
	digits := 0
	for digits < len(raw) {
		char := raw[digits]
		if (char >= '0' && char <= '9') || char == '.' {
			digits++
			continue
		}
		break
	}
	value, err := strconv.ParseFloat(raw[:digits], 64)
	if err != nil {
		return nil
	}
	multiplier, ok := statUnitBytes[strings.ToLower(strings.TrimSpace(raw[digits:]))]
	if !ok {
		return nil
	}
	bytes := int64(value * multiplier)
	return &bytes
}

// runtimeLogs reads the last [tail] lines a container logged, timestamps
// included, straight from the runtime. Unlike the captured store this answers
// for a container the daemon never tailed — a container whose logs interval is
// disabled, or one that was started a moment ago.
func runtimeLogs(ctx context.Context, path, id string, tail int) ([]containerLogLine, error) {
	args := []string{"logs", "--tail", strconv.Itoa(tail), "--timestamps", id}
	out, err := runRuntimeReadBounded(ctx, path, args)
	if err != nil {
		return nil, err
	}
	return parseLogLines(out), nil
}

// runRuntimeReadBounded is runRuntimeRead sized for a log tail, whose output is
// far larger than a diagnostic read: a 200 line backfill can exceed the
// executor's 8 KiB buffer. It shares the never-interactive `sudo -n` retry, and
// reports what the runtime wrote when both the direct call and the elevated one
// failed, so the operator sees "no such container" rather than an exit status.
func runRuntimeReadBounded(ctx context.Context, path string, args []string) ([]byte, error) {
	out, err := runCommandBounded(ctx, path, args...)
	if err == nil {
		return out, nil
	}
	if elevated, ok := elevationAttempt(ctx, path, args...); ok {
		elevatedOut, elevatedErr := runCommandBounded(ctx, elevated[0], elevated[1:]...)
		if elevatedErr == nil {
			return elevatedOut, nil
		}
		if message := strings.TrimSpace(string(elevatedOut)); message != "" {
			return nil, fmt.Errorf("%s", message)
		}
	}
	if message := strings.TrimSpace(string(out)); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	return nil, err
}
