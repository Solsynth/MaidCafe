package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// collectorExecTimeout bounds every subprocess spawned by the stream
// collectors so a hung binary cannot stall collection or leak a process.
const collectorExecTimeout = 5 * time.Second

// Re-probe cadence while a runtime/tool is unavailable: a failed probe is
// retried at most once a minute.
const containerReProbeInterval = 60 * time.Second
const systemdReProbeInterval = 60 * time.Second

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, collectorExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, name, args...)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

func runShell(ctx context.Context, script string) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, collectorExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "sh", "-c", script)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Containers
// ---------------------------------------------------------------------------

type containerEntry struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Image          string `json:"image"`
	State          string `json:"state"`
	Status         string `json:"status"`
	ComposeProject string `json:"compose_project"`
}

type containersPayload struct {
	Runtime    *string          `json:"runtime"`
	Available  bool             `json:"available"`
	Error      *string          `json:"error"`
	Containers []containerEntry `json:"containers"`
}

// ContainersCollector probes for podman/docker once, caches the probe result,
// and re-probes every 60s while unavailable. It emits one "unavailable" event
// per availability flip and otherwise only collects while a runtime exists.
type ContainersCollector struct {
	mu        sync.Mutex
	runtime   string // "podman" | "docker" | "" while unavailable
	available bool
	errMsg    string
	probed    bool
	lastProbe time.Time
	announced bool // whether the current availability state was already sent
}

func (c *ContainersCollector) collect(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.probed || (!c.available && now.Sub(c.lastProbe) >= containerReProbeInterval) {
		c.probe(ctx)
		c.probed = true
		c.lastProbe = now
	}
	if !c.available {
		if !c.announced {
			c.announced = true
			return c.marshal(nil, false, strPtr(c.errMsg))
		}
		return nil, nil
	}
	c.announced = true
	return c.collectContainers(ctx)
}

func (c *ContainersCollector) probe(ctx context.Context) {
	prev := c.available
	c.available = false
	c.errMsg = ""
	runtime, err := probeContainerRuntime(ctx)
	if err == nil {
		c.runtime = runtime
		c.available = true
	} else {
		c.errMsg = err.Error()
	}
	if c.available != prev {
		c.announced = false
	}
}

func (c *ContainersCollector) collectContainers(ctx context.Context) ([]byte, error) {
	out, err := runCommand(ctx, c.runtime, "ps", "-a", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return c.marshal(nil, true, strPtr("list containers: "+err.Error()))
	}
	entries, err := parseContainerLines(out)
	if err != nil {
		return c.marshal(nil, true, strPtr("parse container list: "+err.Error()))
	}
	return c.marshal(entries, true, nil)
}

func (c *ContainersCollector) marshal(entries []containerEntry, available bool, errMsg *string) ([]byte, error) {
	if entries == nil {
		entries = []containerEntry{}
	}
	var runtime *string
	if c.runtime != "" {
		runtime = strPtr(c.runtime)
	}
	return json.Marshal(containersPayload{
		Runtime:    runtime,
		Available:  available,
		Error:      errMsg,
		Containers: entries,
	})
}

// probeContainerRuntime resolves the preferred container runtime via
// `command -v`, matching the client-side SSH probes.
func probeContainerRuntime(ctx context.Context) (string, error) {
	if out, err := runShell(ctx, "command -v podman 2>/dev/null"); err == nil && strings.TrimSpace(string(out)) != "" {
		return "podman", nil
	}
	if out, err := runShell(ctx, "command -v docker 2>/dev/null"); err == nil && strings.TrimSpace(string(out)) != "" {
		return "docker", nil
	}
	return "", errors.New("no container runtime found (podman or docker)")
}

// containerJSONLine mirrors the fields of one `ps -a --format '{{json .}}'`
// line. Podman emits "Id" while Docker emits "ID"; Labels may be a JSON object
// or a comma-joined "k=v,k2=v2" string depending on runtime/version.
type containerJSONLine struct {
	ID     string          `json:"ID"`
	Id     string          `json:"Id"`
	Names  []string        `json:"Names"`
	Image  string          `json:"Image"`
	State  string          `json:"State"`
	Status string          `json:"Status"`
	Labels json.RawMessage `json:"Labels"`
}

func parseContainerLines(out []byte) ([]containerEntry, error) {
	var entries []containerEntry
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw containerJSONLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("container line %q: %w", line, err)
		}
		id := raw.ID
		if id == "" {
			id = raw.Id
		}
		name := ""
		if len(raw.Names) > 0 {
			name = raw.Names[0]
		}
		labels := parseContainerLabels(raw.Labels)
		project := labels["com.docker.compose.project"]
		if project == "" {
			project = labels["io.podman.compose.project"]
		}
		entries = append(entries, containerEntry{
			ID:             id,
			Name:           name,
			Image:          raw.Image,
			State:          raw.State,
			Status:         raw.Status,
			ComposeProject: project,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseContainerLabels(raw json.RawMessage) map[string]string {
	labels := make(map[string]string)
	if len(raw) == 0 || string(raw) == "null" {
		return labels
	}
	var object map[string]string
	if err := json.Unmarshal(raw, &object); err == nil {
		return object
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return labels
	}
	for _, part := range strings.Split(text, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key != "" {
			labels[key] = value
		}
	}
	return labels
}

// ---------------------------------------------------------------------------
// Processes
// ---------------------------------------------------------------------------

type processEntry struct {
	PID           int     `json:"pid"`
	User          string  `json:"user"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	RSSKb         float64 `json:"rss_kb"`
	Command       string  `json:"command"`
}

type processesPayload struct {
	Processes []processEntry `json:"processes"`
}

// ProcessesCollector lists the top CPU consumers via ps, falling back to the
// BSD-style invocation when the GNU-sort form fails (e.g. on macOS).
type ProcessesCollector struct {
	limit int
}

func (p *ProcessesCollector) collect(ctx context.Context) ([]byte, error) {
	out, err := runShell(ctx, "ps -eo pid=,user=,%cpu=,%mem=,rss=,comm= --sort=-%cpu | head -n "+strconv.Itoa(p.limit))
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, err = runShell(ctx, "ps -Ao pid=,user=,%cpu=,%mem=,rss=,comm= -r | head -n "+strconv.Itoa(p.limit))
		if err != nil {
			return nil, err
		}
	}
	payload := processesPayload{Processes: parseProcesses(string(out))}
	return json.Marshal(payload)
}

// parseProcesses parses `ps -o pid=,user=,%cpu=,%mem=,rss=,comm=` output.
// The command is everything after the rss column, joined with single spaces.
func parseProcesses(output string) []processEntry {
	procs := make([]processEntry, 0)
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < 5 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cpu, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			continue
		}
		mem, err := strconv.ParseFloat(fields[3], 64)
		if err != nil {
			continue
		}
		rss, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			continue
		}
		procs = append(procs, processEntry{
			PID:           pid,
			User:          fields[1],
			CPUPercent:    cpu,
			MemoryPercent: mem,
			RSSKb:         rss,
			Command:       strings.Join(fields[5:], " "),
		})
	}
	return procs
}

// ---------------------------------------------------------------------------
// Systemd
// ---------------------------------------------------------------------------

type systemdUnit struct {
	Name          string `json:"name"`
	LoadState     string `json:"load_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	Description   string `json:"description"`
	UnitFileState string `json:"unit_file_state"`
}

type systemdPayload struct {
	Available bool          `json:"available"`
	Error     *string       `json:"error"`
	Units     []systemdUnit `json:"units"`
}

// SystemdCollector probes for systemctl once and re-probes every 60s while
// unavailable, announcing the unavailable state once per flip.
type SystemdCollector struct {
	mu        sync.Mutex
	available bool
	errMsg    string
	probed    bool
	lastProbe time.Time
	announced bool
}

func (s *SystemdCollector) collect(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.probed || (!s.available && now.Sub(s.lastProbe) >= systemdReProbeInterval) {
		s.probe(ctx)
		s.probed = true
		s.lastProbe = now
	}
	if !s.available {
		if !s.announced {
			s.announced = true
			return s.marshal(nil, false, strPtr(s.errMsg))
		}
		return nil, nil
	}
	s.announced = true
	return s.collectUnits(ctx)
}

func (s *SystemdCollector) probe(ctx context.Context) {
	prev := s.available
	s.available = false
	s.errMsg = ""
	if out, err := runShell(ctx, "command -v systemctl 2>/dev/null"); err == nil && strings.TrimSpace(string(out)) != "" {
		s.available = true
	} else {
		s.errMsg = "systemctl was not found on this host (systemd may be absent)"
	}
	if s.available != prev {
		s.announced = false
	}
}

func (s *SystemdCollector) collectUnits(ctx context.Context) ([]byte, error) {
	unitsOut, err := runCommand(ctx, "systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return s.marshal(nil, true, strPtr("list-units: "+err.Error()))
	}
	filesOut, err := runCommand(ctx, "systemctl", "list-unit-files", "--type=service", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return s.marshal(nil, true, strPtr("list-unit-files: "+err.Error()))
	}
	return s.marshal(mergeSystemdUnits(string(unitsOut), string(filesOut)), true, nil)
}

func (s *SystemdCollector) marshal(units []systemdUnit, available bool, errMsg *string) ([]byte, error) {
	if units == nil {
		units = []systemdUnit{}
	}
	return json.Marshal(systemdPayload{
		Available: available,
		Error:     errMsg,
		Units:     units,
	})
}

// systemdUnitNamePattern mirrors the client's safe unit-name pattern; both
// sides reject anything outside it.
var systemdUnitNamePattern = regexp.MustCompile(`^[A-Za-z0-9:._@\-]+\.service$`)

var systemdListUnitsLinePattern = regexp.MustCompile(`^(\S+\.service)\s+(\S+)\s+(\S+)\s+(\S+)\s*(.*)$`)

// parseSystemdListUnits parses `systemctl list-units --type=service --all
// --plain --no-legend` output: UNIT LOAD ACTIVE SUB DESCRIPTION.
func parseSystemdListUnits(output string) map[string]systemdUnit {
	units := make(map[string]systemdUnit)
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimLeft(rawLine, "●○* \t")
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "UNIT ") || strings.HasPrefix(line, "LOAD ") {
			continue
		}
		matches := systemdListUnitsLinePattern.FindStringSubmatch(line)
		if matches == nil || !systemdUnitNamePattern.MatchString(matches[1]) {
			continue
		}
		units[matches[1]] = systemdUnit{
			Name:        matches[1],
			LoadState:   matches[2],
			ActiveState: matches[3],
			SubState:    matches[4],
			Description: strings.TrimSpace(matches[5]),
		}
	}
	return units
}

// parseSystemdListUnitFiles parses `systemctl list-unit-files --type=service
// --plain --no-legend` output: UNIT FILE STATE [PRESET].
func parseSystemdListUnitFiles(output string) map[string]string {
	files := make(map[string]string)
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "UNIT FILE") || strings.HasPrefix(line, "STATE ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name, state := parts[0], parts[1]
		if !strings.HasSuffix(name, ".service") || !systemdUnitNamePattern.MatchString(name) {
			continue
		}
		files[name] = state
	}
	return files
}

// mergeSystemdUnits merges list-units with list-unit-files exactly like the
// client's mergeSystemdListings: unit-file states are attached to known units,
// and enabled-but-inactive units that only appear in unit-files are included
// with not-found/inactive/dead states. Result is sorted failed-first, then
// active, then by name.
func mergeSystemdUnits(listUnitsOutput, listUnitFilesOutput string) []systemdUnit {
	units := parseSystemdListUnits(listUnitsOutput)
	files := parseSystemdListUnitFiles(listUnitFilesOutput)
	for name, state := range files {
		if existing, ok := units[name]; ok {
			existing.UnitFileState = state
			units[name] = existing
		} else {
			units[name] = systemdUnit{
				Name:          name,
				LoadState:     "not-found",
				ActiveState:   "inactive",
				SubState:      "dead",
				Description:   "",
				UnitFileState: state,
			}
		}
	}
	merged := make([]systemdUnit, 0, len(units))
	for _, unit := range units {
		merged = append(merged, unit)
	}
	sort.Slice(merged, func(i, j int) bool {
		left, right := systemdUnitRank(merged[i]), systemdUnitRank(merged[j])
		if left != right {
			return left < right
		}
		return strings.ToLower(merged[i].Name) < strings.ToLower(merged[j].Name)
	})
	return merged
}

func systemdUnitRank(unit systemdUnit) int {
	switch strings.ToLower(unit.ActiveState) {
	case "failed":
		return 0
	case "active":
		return 1
	default:
		return 2
	}
}
