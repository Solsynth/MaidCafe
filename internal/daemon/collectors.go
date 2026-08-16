package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

type containersRuntimePayload struct {
	Runtime    string           `json:"runtime"`
	Available  bool             `json:"available"`
	Error      *string          `json:"error"`
	Containers []containerEntry `json:"containers"`
}

type containersPayload struct {
	Runtimes []containersRuntimePayload `json:"runtimes"`
}

// runtimeProbeState resolves which container runtimes are installed and where
// their binaries live, probing once and re-probing every
// containerReProbeInterval while none is found. Shared by the containers and
// images collectors so a single probe serves both.
type runtimeProbeState struct {
	mu           sync.Mutex
	runtimes     []string
	runtimePaths map[string]string
	probed       bool
	lastProbe    time.Time
}

// probePathSnapshot ensures the probe has run and returns the current
// runtime name -> binary path map.
func (p *runtimeProbeState) probePathSnapshot(ctx context.Context) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.probed || (len(p.runtimes) == 0 && now.Sub(p.lastProbe) >= containerReProbeInterval) {
		paths := probeContainerRuntimes(ctx)
		names := make([]string, 0, len(paths))
		for _, candidate := range []string{"podman", "docker"} {
			if _, ok := paths[candidate]; ok {
				names = append(names, candidate)
			}
		}
		p.runtimes = names
		p.runtimePaths = paths
		p.probed = true
		p.lastProbe = now
	}
	out := make(map[string]string, len(p.runtimePaths))
	for name, path := range p.runtimePaths {
		out[name] = path
	}
	return out
}

// runtimeStateKey fingerprints the probed runtime set ("podman,docker") so
// collectors can re-announce an empty payload when the set changes.
func runtimeStateKey(paths map[string]string) string {
	names := make([]string, 0, 2)
	for _, candidate := range []string{"podman", "docker"} {
		if _, ok := paths[candidate]; ok {
			names = append(names, candidate)
		}
	}
	return strings.Join(names, ",")
}

// runRuntimeList runs a runtime CLI listing command, retrying through
// `sudo -n` (never interactive) when the direct invocation fails or returns
// nothing and the daemon is not root. Rootful runtimes are invisible to a
// non-root daemon — e.g. the shipped systemd unit runs as the maidcafe user
// while operators run containers as root — and the elevated retry sees them
// whenever the daemon user has passwordless sudo. The retry is never
// interactive; the systemd unit's NoNewPrivileges keeps it inert there. An
// empty direct listing whose elevated retry also fails is reported as an
// error rather than a misleading empty list, so invisible root-owned
// containers never masquerade as "no containers".
func runRuntimeList(ctx context.Context, path string, args ...string) ([]byte, error) {
	out, err := runCommand(ctx, path, args...)
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		return out, nil
	}
	if os.Geteuid() == 0 {
		return out, err
	}
	if _, lookupErr := exec.LookPath("sudo"); lookupErr != nil {
		return out, err
	}
	sudoOut, sudoErr := runCommand(ctx, "sudo", append([]string{"-n", path}, args...)...)
	if sudoErr != nil {
		if err != nil {
			// The direct failure stays the primary signal.
			return out, err
		}
		// The direct listing succeeded but returned nothing and the
		// elevated retry failed: root-owned containers are invisible to
		// this daemon. Surface the retry failure instead of a misleading
		// empty list.
		return nil, fmt.Errorf("empty listing, elevated retry failed: %w", sudoErr)
	}
	return sudoOut, nil
}

// ContainersCollector lists podman/docker containers (podman first), caching
// the runtime probe and re-probing every 60s while a runtime is unavailable.
// The stream uses collect(), which announces an empty runtimes list once per
// availability flip; the HTTP endpoints use snapshot(), which always returns
// a payload.
type ContainersCollector struct {
	mu        sync.Mutex
	probe     *runtimeProbeState
	announced bool   // whether the current runtimes state was already sent
	lastState string // fingerprint of the last probed runtimes set
}

func (c *ContainersCollector) collect(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths := c.probe.probePathSnapshot(ctx)
	state := runtimeStateKey(paths)
	if state != c.lastState {
		c.announced = false
		c.lastState = state
	}
	if len(paths) == 0 {
		if !c.announced {
			c.announced = true
			return c.marshal()
		}
		return nil, nil
	}
	c.announced = true
	return c.collectRuntimes(ctx, paths)
}

// snapshot always returns a payload — unlike collect, which the stream uses
// and which skips re-announcing an already-announced unavailable state.
func (c *ContainersCollector) snapshot(ctx context.Context) ([]byte, error) {
	data, err := c.collect(ctx)
	if err != nil {
		return nil, err
	}
	if data != nil {
		return data, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.marshal()
}

func (c *ContainersCollector) collectRuntimes(ctx context.Context, paths map[string]string) ([]byte, error) {
	payload := containersPayload{
		Runtimes: make([]containersRuntimePayload, 0, len(paths)),
	}
	for _, runtime := range []string{"podman", "docker"} {
		path := paths[runtime]
		if path == "" {
			continue
		}
		out, err := runRuntimeList(ctx, path, "ps", "-a", "--no-trunc", "--format", "{{json .}}")
		if err != nil {
			payload.Runtimes = append(payload.Runtimes, containersRuntimePayload{
				Runtime: runtime, Available: true, Error: strPtr("list containers: " + err.Error()),
				Containers: []containerEntry{},
			})
			continue
		}
		entries, err := parseContainerLines(out)
		if err != nil {
			payload.Runtimes = append(payload.Runtimes, containersRuntimePayload{
				Runtime: runtime, Available: true, Error: strPtr("parse container list: " + err.Error()),
				Containers: []containerEntry{},
			})
			continue
		}
		payload.Runtimes = append(payload.Runtimes, containersRuntimePayload{
			Runtime: runtime, Available: true, Containers: entries,
		})
	}
	return json.Marshal(payload)
}

func (c *ContainersCollector) marshal() ([]byte, error) {
	return json.Marshal(containersPayload{Runtimes: []containersRuntimePayload{}})
}

// isPodmanDockerShim reports whether the docker binary at path is really
// podman: distro podman-docker shims answer `--version` with podman's own
// version string, while a real docker reports "Docker version ...".
func isPodmanDockerShim(ctx context.Context, path string) bool {
	out, err := runCommand(ctx, path, "--version")
	return err == nil && strings.Contains(strings.ToLower(string(out)), "podman")
}

// probeContainerRuntimes resolves the available container runtimes, podman
// preferred. `command -v` can miss binaries outside the systemd service PATH
// (e.g. /opt/homebrew/bin), so known install locations are checked as well;
// the resolved absolute path is what the collector execs. A docker binary
// that is really podman is dropped while the real podman is present, so the
// same rootful containers and images are never reported twice.
func probeContainerRuntimes(ctx context.Context) map[string]string {
	paths := make(map[string]string)
	for _, candidate := range []string{"podman", "docker"} {
		if resolved := resolveRuntimeBinary(ctx, candidate); resolved != "" {
			paths[candidate] = resolved
		}
	}
	if dockerPath, podmanPath := paths["docker"], paths["podman"]; dockerPath != "" &&
		podmanPath != "" && isPodmanDockerShim(ctx, dockerPath) {
		delete(paths, "docker")
	}
	return paths
}

func resolveRuntimeBinary(ctx context.Context, name string) string {
	if out, err := runShell(ctx, "command -v "+name+" 2>/dev/null"); err == nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return trimmed
		}
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/usr/local/sbin", "/opt/homebrew/bin"} {
		path := filepath.Join(dir, name)
		if out, err := runShell(ctx, "test -x "+path+" && echo ok 2>/dev/null"); err == nil && strings.TrimSpace(string(out)) == "ok" {
			return path
		}
	}
	return ""
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
	entries := make([]containerEntry, 0)
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
// Images
// ---------------------------------------------------------------------------

type imageEntry struct {
	ID      string   `json:"id"`
	Tags    []string `json:"tags"`
	Size    int64    `json:"size"`
	Created int64    `json:"created"`
	Digest  string   `json:"digest"`
}

type imagesRuntimePayload struct {
	Runtime   string       `json:"runtime"`
	Available bool         `json:"available"`
	Error     *string      `json:"error"`
	Images    []imageEntry `json:"images"`
}

type imagesPayload struct {
	Runtimes []imagesRuntimePayload `json:"runtimes"`
}

// ImagesCollector lists podman/docker images with the same probe cache and
// stream announcement semantics as ContainersCollector.
type ImagesCollector struct {
	mu        sync.Mutex
	probe     *runtimeProbeState
	announced bool
	lastState string
}

func (c *ImagesCollector) collect(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths := c.probe.probePathSnapshot(ctx)
	state := runtimeStateKey(paths)
	if state != c.lastState {
		c.announced = false
		c.lastState = state
	}
	if len(paths) == 0 {
		if !c.announced {
			c.announced = true
			return c.marshal()
		}
		return nil, nil
	}
	c.announced = true
	return c.collectRuntimes(ctx, paths)
}

// snapshot always returns a payload — unlike collect, which the stream uses
// and which skips re-announcing an already-announced unavailable state.
func (c *ImagesCollector) snapshot(ctx context.Context) ([]byte, error) {
	data, err := c.collect(ctx)
	if err != nil {
		return nil, err
	}
	if data != nil {
		return data, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.marshal()
}

func (c *ImagesCollector) collectRuntimes(ctx context.Context, paths map[string]string) ([]byte, error) {
	payload := imagesPayload{
		Runtimes: make([]imagesRuntimePayload, 0, len(paths)),
	}
	for _, runtime := range []string{"podman", "docker"} {
		path := paths[runtime]
		if path == "" {
			continue
		}
		out, err := runRuntimeList(ctx, path, "images", "--no-trunc", "--format", "{{json .}}")
		if err != nil {
			payload.Runtimes = append(payload.Runtimes, imagesRuntimePayload{
				Runtime: runtime, Available: true, Error: strPtr("list images: " + err.Error()),
				Images: []imageEntry{},
			})
			continue
		}
		entries, err := parseImageLines(out)
		if err != nil {
			payload.Runtimes = append(payload.Runtimes, imagesRuntimePayload{
				Runtime: runtime, Available: true, Error: strPtr("parse image list: " + err.Error()),
				Images: []imageEntry{},
			})
			continue
		}
		payload.Runtimes = append(payload.Runtimes, imagesRuntimePayload{
			Runtime: runtime, Available: true, Images: entries,
		})
	}
	return json.Marshal(payload)
}

func (c *ImagesCollector) marshal() ([]byte, error) {
	return json.Marshal(imagesPayload{Runtimes: []imagesRuntimePayload{}})
}

// imageJSONLine mirrors one `images --format '{{json .}}'` line. Podman emits
// "Id"/"RepoTags"/"Names" and an integer Size; Docker emits "ID"/"Repository"/
// "Tag" and a string Size.
type imageJSONLine struct {
	ID         string          `json:"ID"`
	Id         string          `json:"Id"`
	Repository string          `json:"Repository"`
	Tag        string          `json:"Tag"`
	RepoTags   []string        `json:"RepoTags"`
	Names      []string        `json:"Names"`
	Size       json.RawMessage `json:"Size"`
	Created    int64           `json:"Created"`
	Digest     string          `json:"Digest"`
}

func parseImageLines(out []byte) ([]imageEntry, error) {
	entries := make([]imageEntry, 0)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw imageJSONLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("image line %q: %w", line, err)
		}
		id := raw.ID
		if id == "" {
			id = raw.Id
		}
		tags := raw.RepoTags
		if len(tags) == 0 {
			tags = raw.Names
		}
		if len(tags) == 0 && !strings.HasPrefix(raw.Repository, "<none>") {
			tag := raw.Tag
			if tag == "" || strings.HasPrefix(tag, "<none>") {
				tag = "latest"
			}
			tags = []string{raw.Repository + ":" + tag}
		}
		if tags == nil {
			tags = []string{}
		}
		digest := raw.Digest
		if strings.HasPrefix(digest, "<none>") {
			digest = ""
		}
		entries = append(entries, imageEntry{
			ID:      id,
			Tags:    tags,
			Size:    parseImageSize(raw.Size),
			Created: raw.Created,
			Digest:  digest,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// parseImageSize accepts both integer (podman) and string (docker) Size JSON.
func parseImageSize(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parsed, _ := strconv.ParseInt(s, 10, 64)
		return parsed
	}
	return 0
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

// snapshot always returns a payload — for the HTTP endpoints, unlike collect
// which is used by the stream and skips re-announcing an unavailable state.
func (s *SystemdCollector) snapshot(ctx context.Context) ([]byte, error) {
	data, err := s.collect(ctx)
	if err != nil {
		return nil, err
	}
	if data != nil {
		return data, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marshal(nil, false, strPtr(s.errMsg))
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

// ---------------------------------------------------------------------------
// Runtimes
// ---------------------------------------------------------------------------

// maxJvmProbePids caps the per-collection jstat probes: jstat runs once per
// JVM and shares the same host, so a Java-heavy box must not stall the frame.
const maxJvmProbePids = 8

// jdkReProbeInterval is the re-probe cadence while jps/jstat are unavailable.
const jdkReProbeInterval = 60 * time.Second

// runtimeProcessTableLimit caps the head of the ps pipe read per collection.
const runtimeProcessTableLimit = 300

type runtimeProcessEntry struct {
	PID           int     `json:"pid"`
	User          string  `json:"user"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	RSSKb         int64   `json:"rss_kb"`
	Threads       *int64  `json:"threads"`
	Command       string  `json:"command"`
}

// jvmProcessEntry is one `jps -l` line; MainClass is empty when jps could not
// resolve it.
type jvmProcessEntry struct {
	PID       int
	MainClass string
}

type javaJvmEntry struct {
	PID        int      `json:"pid"`
	MainClass  *string  `json:"main_class"`
	OldPercent *float64 `json:"old_percent"`
	YGC        *int64   `json:"ygc"`
	FGC        *int64   `json:"fgc"`
	GCTSeconds *float64 `json:"gct_seconds"`
	Error      *string  `json:"error"`
}

type jdkProbe struct {
	Available bool    `json:"available"`
	Error     *string `json:"error"`
}

type javaRuntimePayload struct {
	JDK  jdkProbe        `json:"jdk"`
	JVMs []javaJvmEntry  `json:"jvms"`
}

type runtimeGroup struct {
	Runtime   string                `json:"runtime"`
	Available bool                  `json:"available"`
	Error     *string               `json:"error"`
	Processes []runtimeProcessEntry `json:"processes"`
	Java      *javaRuntimePayload   `json:"java,omitempty"`
}

// watchedProcessGroup is one user-defined process watcher: processes whose
// comm token starts with the watched name.
type watchedProcessGroup struct {
	Name      string                `json:"name"`
	Available bool                  `json:"available"`
	Error     *string               `json:"error"`
	Processes []runtimeProcessEntry `json:"processes"`
}

type runtimePayload struct {
	Runtimes []runtimeGroup        `json:"runtimes"`
	Watched  []watchedProcessGroup `json:"watched"`
}

// jdkProbeState resolves the jps/jstat binaries once and re-probes every
// jdkReProbeInterval while either is missing. Probed only when at least one
// java process exists, so hosts without Java never pay for the probe.
type jdkProbeState struct {
	mu        sync.Mutex
	jpsPath   string
	jstatPath string
	probed    bool
	lastProbe time.Time
}

// probePathSnapshot ensures the probe has run and returns the resolved
// jps/jstat absolute paths ("" when missing), mirroring runtimeProbeState.
func (j *jdkProbeState) probePathSnapshot(ctx context.Context) (jpsPath, jstatPath string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	if !j.probed || ((j.jpsPath == "" || j.jstatPath == "") && now.Sub(j.lastProbe) >= jdkReProbeInterval) {
		j.jpsPath = resolveRuntimeBinary(ctx, "jps")
		j.jstatPath = resolveRuntimeBinary(ctx, "jstat")
		j.probed = true
		j.lastProbe = now
	}
	return j.jpsPath, j.jstatPath
}

// RuntimesCollector groups the configured runtime names (default java/dotnet/
// python) from ps, capping each at `limit` entries, attaches JVM/GC detail
// (jps/jstat) to the java group when a JDK is present, and reports the
// daemon-side watched processes alongside. Groups follow the configured
// runtime order; a group with no matching process carries available:false.
type RuntimesCollector struct {
	limit    int
	jdk      *jdkProbeState
	runtimes []string
	watched  *watchedProcessStore
}

func (r *RuntimesCollector) collect(ctx context.Context) ([]byte, error) {
	// GNU ps carries the nlwp thread count; fall back to the BSD form (no
	// nlwp) when the GNU-sort form fails, e.g. on macOS.
	out, err := runShell(ctx, "ps -eo pid=,user=,%cpu=,%mem=,rss=,nlwp=,comm=,args= --sort=-%cpu | head -n "+strconv.Itoa(runtimeProcessTableLimit))
	hasThreads := true
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, err = runShell(ctx, "ps -Ao pid=,user=,%cpu=,%mem=,rss=,comm=,args= -r | head -n "+strconv.Itoa(runtimeProcessTableLimit))
		hasThreads = false
		if err != nil {
			return nil, err
		}
	}
	entries := parseRuntimeProcesses(string(out), hasThreads)
	runtimeNames := r.runtimes
	if len(runtimeNames) == 0 {
		runtimeNames = []string{"java", "dotnet", "python"}
	}
	groups := groupRuntimeProcesses(entries, r.limit, runtimeNames)
	// The java key is present only when a java process exists.
	for i := range groups {
		if groups[i].Runtime == "java" && len(groups[i].Processes) > 0 {
			groups[i].Java = r.collectJavaDetail(ctx)
			break
		}
	}
	var watched []watchedProcessGroup
	if r.watched != nil {
		watched = groupWatchedProcesses(entries, r.limit, r.watched.List())
	} else {
		watched = []watchedProcessGroup{}
	}
	return json.Marshal(runtimePayload{Runtimes: groups, Watched: watched})
}

// collectJavaDetail runs jps -l and per-pid jstat -gcutil for the first
// maxJvmProbePids JVMs. Per-pid failures become entry errors and never abort
// the frame; missing tools flip the jdk probe to unavailable.
func (r *RuntimesCollector) collectJavaDetail(ctx context.Context) *javaRuntimePayload {
	if r.jdk == nil {
		r.jdk = &jdkProbeState{}
	}
	jpsPath, jstatPath := r.jdk.probePathSnapshot(ctx)
	if jpsPath == "" || jstatPath == "" {
		errMsg := "jps/jstat were not found on this host (no JDK)"
		return &javaRuntimePayload{JDK: jdkProbe{Available: false, Error: &errMsg}, JVMs: []javaJvmEntry{}}
	}
	jpsOut, err := runCommand(ctx, jpsPath, "-l")
	if err != nil {
		errMsg := "jps -l: " + err.Error()
		return &javaRuntimePayload{JDK: jdkProbe{Available: false, Error: &errMsg}, JVMs: []javaJvmEntry{}}
	}
	jvms := parseJpsOutput(string(jpsOut))
	if len(jvms) > maxJvmProbePids {
		jvms = jvms[:maxJvmProbePids]
	}
	entries := make([]javaJvmEntry, 0, len(jvms))
	for _, jvm := range jvms {
		mainClass := jvm.MainClass
		entry := javaJvmEntry{PID: jvm.PID, MainClass: &mainClass}
		out, jerr := runCommand(ctx, jstatPath, "-gcutil", strconv.Itoa(jvm.PID))
		if jerr != nil {
			errMsg := "jstat -gcutil: " + jerr.Error()
			entry.Error = &errMsg
			entries = append(entries, entry)
			continue
		}
		oldPct, ygc, fgc, gct, perr := parseJstatGcutilOutput(string(out))
		if perr != nil {
			errMsg := "parse jstat output: " + perr.Error()
			entry.Error = &errMsg
			entries = append(entries, entry)
			continue
		}
		entry.OldPercent = &oldPct
		entry.YGC = &ygc
		entry.FGC = &fgc
		entry.GCTSeconds = &gct
		entries = append(entries, entry)
	}
	return &javaRuntimePayload{JDK: jdkProbe{Available: true}, JVMs: entries}
}

// groupRuntimeProcesses partitions runtime processes into one group per
// configured runtime name, capping each at limit in the input (CPU) order. A
// process matches a group when its comm token starts with the runtime name;
// a group with no match reports available:false.
func groupRuntimeProcesses(entries []runtimeProcessEntry, limit int, names []string) []runtimeGroup {
	groups := make([]runtimeGroup, len(names))
	for i, name := range names {
		groups[i] = runtimeGroup{Runtime: name, Available: false, Processes: []runtimeProcessEntry{}}
	}
	for _, entry := range entries {
		comm := processComm(entry)
		for i, name := range names {
			if !strings.HasPrefix(comm, name) {
				continue
			}
			if len(groups[i].Processes) >= limit {
				break
			}
			groups[i].Available = true
			groups[i].Processes = append(groups[i].Processes, entry)
			break
		}
	}
	for i, group := range groups {
		if !group.Available {
			errMsg := "no " + group.Runtime + " processes found"
			groups[i].Error = &errMsg
		}
	}
	return groups
}

// groupWatchedProcesses partitions processes into one group per watched name
// (comm token prefix match), capping each at limit in CPU order. A group with
// no match reports available:false. Names are sorted for deterministic order.
func groupWatchedProcesses(entries []runtimeProcessEntry, limit int, names []string) []watchedProcessGroup {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	groups := make([]watchedProcessGroup, 0, len(sorted))
	for _, name := range sorted {
		group := watchedProcessGroup{Name: name, Available: false, Processes: []runtimeProcessEntry{}}
		for _, entry := range entries {
			if len(group.Processes) >= limit {
				break
			}
			if strings.HasPrefix(processComm(entry), name) {
				group.Available = true
				group.Processes = append(group.Processes, entry)
			}
		}
		if !group.Available {
			errMsg := "no " + name + " processes found"
			group.Error = &errMsg
		}
		groups = append(groups, group)
	}
	return groups
}

// processComm returns the comm token of a parsed process row: the first
// whitespace-delimited token of the joined command.
func processComm(entry runtimeProcessEntry) string {
	comm := entry.Command
	if idx := strings.IndexByte(comm, ' '); idx >= 0 {
		comm = comm[:idx]
	}
	return comm
}

// parseRuntimeProcesses parses `ps -eo pid=,user=,%cpu=,%mem=,rss=,nlwp=,comm=,args=`
// (hasThreads) or the BSD `ps -Ao pid=,user=,%cpu=,%mem=,rss=,comm=,args= -r`
// form (no nlwp). Fixed columns are pid user cpu mem rss [nlwp] comm; the
// command is the rest of the line joined with single spaces. Malformed rows
// are skipped.
func parseRuntimeProcesses(output string, hasThreads bool) []runtimeProcessEntry {
	minFields := 6
	if hasThreads {
		minFields = 7
	}
	entries := make([]runtimeProcessEntry, 0)
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < minFields {
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
		rss, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			continue
		}
		entry := runtimeProcessEntry{
			PID:           pid,
			User:          fields[1],
			CPUPercent:    cpu,
			MemoryPercent: mem,
			RSSKb:         rss,
		}
		if hasThreads {
			threads, terr := strconv.ParseInt(fields[5], 10, 64)
			if terr != nil {
				continue
			}
			entry.Threads = &threads
			entry.Command = strings.Join(fields[6:], " ")
		} else {
			entry.Command = strings.Join(fields[5:], " ")
		}
		entries = append(entries, entry)
	}
	return entries
}

// parseJpsOutput parses `jps -l` output: `<pid> [<main-class>]`.
func parseJpsOutput(output string) []jvmProcessEntry {
	jvms := make([]jvmProcessEntry, 0)
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		mainClass := ""
		if len(fields) > 1 {
			mainClass = strings.Join(fields[1:], " ")
		}
		jvms = append(jvms, jvmProcessEntry{PID: pid, MainClass: mainClass})
	}
	return jvms
}

// parseJstatGcutilOutput parses one `jstat -gcutil <pid>` line (header
// S0 S1 E O M CCS YGC YGCT FGC FGCT GCT): O = fields[3], YGC = fields[6],
// FGC = fields[8], GCT = fields[10].
func parseJstatGcutilOutput(output string) (oldPercent float64, ygc, fgc int64, gctSeconds float64, err error) {
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < 11 {
			continue
		}
		old, oerr := strconv.ParseFloat(fields[3], 64)
		y, yerr := strconv.ParseInt(fields[6], 10, 64)
		f, ferr := strconv.ParseInt(fields[8], 10, 64)
		gct, gerr := strconv.ParseFloat(fields[10], 64)
		if oerr != nil || yerr != nil || ferr != nil || gerr != nil {
			continue
		}
		return old, y, f, gct, nil
	}
	return 0, 0, 0, 0, fmt.Errorf("no valid jstat -gcutil data line found")
}
