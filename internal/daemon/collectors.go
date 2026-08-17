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
// BSD-style invocation when the GNU-sort form fails (e.g. on macOS). limit is
// the daemon's default cap; per-request values override it (0 = all).
type ProcessesCollector struct {
	limit int
}

// collectEntries runs ps once and returns the parsed rows. A limit <= 0 keeps
// the complete process table; a positive limit keeps the top `limit` CPU
// consumers (ps already sorts by CPU, so head preserves that order).
func (p *ProcessesCollector) collectEntries(ctx context.Context, limit int) ([]processEntry, error) {
	head := ""
	if limit > 0 {
		head = " | head -n " + strconv.Itoa(limit)
	}
	out, err := runShell(ctx, "ps -eo pid=,user=,%cpu=,%mem=,rss=,comm= --sort=-%cpu"+head)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, err = runShell(ctx, "ps -Ao pid=,user=,%cpu=,%mem=,rss=,comm= -r"+head)
		if err != nil {
			return nil, err
		}
	}
	return parseProcesses(string(out)), nil
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
	JDK  jdkProbe       `json:"jdk"`
	JVMs []javaJvmEntry `json:"jvms"`
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
	history  *processHistoryStore
}

func (r *RuntimesCollector) readProcessTable(ctx context.Context) ([]runtimeProcessEntry, bool, error) {
	// GNU ps carries the nlwp thread count; fall back to the BSD form (no
	// nlwp) when the GNU-sort form fails, e.g. on macOS.
	out, err := runShell(ctx, "ps -eo pid=,user=,%cpu=,%mem=,rss=,nlwp=,comm=,args= --sort=-%cpu | head -n "+strconv.Itoa(runtimeProcessTableLimit))
	hasThreads := true
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, err = runShell(ctx, "ps -Ao pid=,user=,%cpu=,%mem=,rss=,comm=,args= -r | head -n "+strconv.Itoa(runtimeProcessTableLimit))
		hasThreads = false
		if err != nil {
			return nil, false, err
		}
	}
	return parseRuntimeProcesses(string(out), hasThreads), hasThreads, nil
}

func (r *RuntimesCollector) collect(ctx context.Context) ([]byte, error) {
	entries, _, err := r.readProcessTable(ctx)
	if err != nil {
		return nil, err
	}
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

// recordHistory appends one usage sample per watched process to the history
// store. It runs on its own ungated ticker so history accumulates even while
// no SSE subscriber is connected. Groups with no matching process record a
// zero-count sample so charts show downtime explicitly.
func (r *RuntimesCollector) recordHistory(ctx context.Context) {
	if r.history == nil || r.watched == nil {
		return
	}
	entries, _, err := r.readProcessTable(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	for _, group := range groupWatchedProcesses(entries, r.limit, r.watched.List()) {
		var cpu float64
		var rss int64
		var threads int64
		var threadCount int
		for _, entry := range group.Processes {
			cpu += entry.CPUPercent
			rss += entry.RSSKb
			if entry.Threads != nil {
				threads += *entry.Threads
				threadCount++
			}
		}
		sample := processHistorySample{
			Name:         group.Name,
			TS:           now,
			CPUPercent:   cpu,
			RSSKb:        rss,
			ProcessCount: len(group.Processes),
		}
		if threadCount > 0 {
			sample.Threads = &threads
		}
		r.history.Append(sample)
	}
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

// ---------------------------------------------------------------------------
// Database metrics
// ---------------------------------------------------------------------------

// databaseMetricDB is one PostgreSQL database's pg_stat_database counters.
type databaseMetricDB struct {
	Name        string `json:"name"`
	Connections *int64 `json:"connections"`
	Commits     *int64 `json:"commits"`
	Rollbacks   *int64 `json:"rollbacks"`
	BlksHit     *int64 `json:"blks_hit"`
	BlksRead    *int64 `json:"blks_read"`
	Deadlocks   *int64 `json:"deadlocks"`
}

// databaseMetricsEntry is one engine's health snapshot. PostgreSQL fills the
// commit/rollback/cache counters plus per-database rows; MySQL-like engines
// fill the query/thread/buffer-pool counters. memory_bytes is shared_buffers
// for PostgreSQL and innodb_buffer_pool_size for MySQL-like engines.
type databaseMetricsEntry struct {
	Engine             string             `json:"engine"`
	Available          bool               `json:"available"`
	Error              *string            `json:"error"`
	Version            *string            `json:"version"`
	Connections        *int64             `json:"connections"`
	MaxConnections     *int64             `json:"max_connections"`
	MemoryBytes        *int64             `json:"memory_bytes"`
	MemoryUsedBytes    *int64             `json:"memory_used_bytes"`
	MemoryDirtyBytes   *int64             `json:"memory_dirty_bytes"`
	CacheHitRatio      *float64           `json:"cache_hit_ratio"`
	Commits            *int64             `json:"commits"`
	Rollbacks          *int64             `json:"rollbacks"`
	BlksHit            *int64             `json:"blks_hit"`
	BlksRead           *int64             `json:"blks_read"`
	Deadlocks          *int64             `json:"deadlocks"`
	TempBytes          *int64             `json:"temp_bytes"`
	Queries            *int64             `json:"queries"`
	SlowQueries        *int64             `json:"slow_queries"`
	ThreadsRunning     *int64             `json:"threads_running"`
	MaxUsedConnections *int64             `json:"max_used_connections"`
	UptimeSeconds      *int64             `json:"uptime_seconds"`
	BytesReceived      *int64             `json:"bytes_received"`
	BytesSent          *int64             `json:"bytes_sent"`
	Databases          []databaseMetricDB `json:"databases"`
}

type databaseMetricsPayload struct {
	Engines []databaseMetricsEntry `json:"engines"`
}

// DatabaseMetricsCollector samples PostgreSQL / MySQL / MariaDB health
// counters over the local client tools (peer auth for postgres, socket /
// debian.cnf for MySQL). It mirrors the SSH fallback in the MaidKit client,
// so both channels report the same shape. Each engine is probed
// independently: a failure marks that engine unavailable and never aborts
// the payload.
type DatabaseMetricsCollector struct{}

// collect probes every engine and returns the marshaled payload. It never
// returns an error for a failed engine; only a failed marshal does.
func (c *DatabaseMetricsCollector) collect(ctx context.Context) ([]byte, error) {
	entries := make([]databaseMetricsEntry, 0, 2)
	if entry := c.collectPostgres(ctx); entry != nil {
		entries = append(entries, *entry)
	}
	if entry := c.collectMySQL(ctx); entry != nil {
		entries = append(entries, *entry)
	}
	return json.Marshal(databaseMetricsPayload{Engines: entries})
}

const postgresMetricsScript = `PGBIN=$(for d in /usr/pgsql-*/bin /usr/lib/postgresql/*/bin /usr/local/pgsql/bin; do
  [ -d "$d" ] && echo "$d"
done | sort -V | tail -n 1)
PSQL=""
if [ -n "$PGBIN" ] && [ -x "$PGBIN/psql" ]; then
  PSQL="$PGBIN/psql"
else
  PSQL=$(command -v psql 2>/dev/null || true)
fi
if [ -z "$PSQL" ]; then exit 1; fi
echo '--DB-PG-ROWS--'
if id postgres >/dev/null 2>&1; then
  su -s /bin/sh postgres -c "$PSQL -X -A -t -F '|' -c 'SELECT d.datname, s.numbackends, s.xact_commit, s.xact_rollback, s.blks_read, s.blks_hit, s.deadlocks, s.temp_bytes FROM pg_catalog.pg_stat_database s JOIN pg_catalog.pg_database d ON d.oid = s.datid WHERE d.datallowconn ORDER BY d.datname'" 2>&1
else
  "$PSQL" -X -A -t -F '|' -c 'SELECT d.datname, s.numbackends, s.xact_commit, s.xact_rollback, s.blks_read, s.blks_hit, s.deadlocks, s.temp_bytes FROM pg_catalog.pg_stat_database s JOIN pg_catalog.pg_database d ON d.oid = s.datid WHERE d.datallowconn ORDER BY d.datname' 2>&1
fi
echo '--DB-PG-MAXCONN--'
if id postgres >/dev/null 2>&1; then
  su -s /bin/sh postgres -c "$PSQL -X -A -t -c 'SHOW max_connections'" 2>&1
else
  "$PSQL" -X -A -t -c 'SHOW max_connections' 2>&1
fi
echo '--DB-PG-SHARED--'
if id postgres >/dev/null 2>&1; then
  su -s /bin/sh postgres -c "$PSQL -X -A -t -c 'SHOW shared_buffers'" 2>&1
else
  "$PSQL" -X -A -t -c 'SHOW shared_buffers' 2>&1
fi
echo '--DB-PG-VERSION--'
if [ -n "$PGBIN" ] && [ -x "$PGBIN/postgres" ]; then
  "$PGBIN/postgres" --version
else
  "$PSQL" --version
fi`

const mysqlMetricsScript = `MYSQL=$(command -v mysql 2>/dev/null || command -v mariadb 2>/dev/null || true)
if [ -z "$MYSQL" ]; then exit 1; fi
AUTH=""
if "$MYSQL" -N -e 'SELECT 1' >/dev/null 2>&1; then AUTH=""
elif [ -f /etc/mysql/debian.cnf ]; then AUTH="--defaults-file=/etc/mysql/debian.cnf"
else exit 1; fi
echo '--DB-MY-STATUS--'
"$MYSQL" $AUTH -N -e "SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME IN ('Threads_connected','Max_used_connections','Threads_running','Innodb_buffer_pool_pages_total','Innodb_buffer_pool_pages_data','Innodb_buffer_pool_pages_dirty','Innodb_buffer_pool_read_requests','Innodb_buffer_pool_reads','Queries','Slow_queries','Uptime','Innodb_page_size','Bytes_received','Bytes_sent')"
echo '--DB-MY-VARS--'
"$MYSQL" $AUTH -N -e "SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_VARIABLES WHERE VARIABLE_NAME IN ('innodb_buffer_pool_size','max_connections')"
echo '--DB-MY-VERSION--'
"$MYSQL" $AUTH -N -e "SELECT VERSION()"`

func (c *DatabaseMetricsCollector) collectPostgres(ctx context.Context) *databaseMetricsEntry {
	out, err := runShell(ctx, postgresMetricsScript)
	if err != nil {
		return unavailableEntry("postgres", "psql probe failed")
	}
	return parsePostgresDatabaseMetricsOutput(string(out))
}

func (c *DatabaseMetricsCollector) collectMySQL(ctx context.Context) *databaseMetricsEntry {
	out, err := runShell(ctx, mysqlMetricsScript)
	if err != nil {
		return unavailableEntry("mysql", "mysql probe failed")
	}
	return parseMySQLDatabaseMetricsOutput(string(out))
}

func unavailableEntry(engine, message string) *databaseMetricsEntry {
	return &databaseMetricsEntry{
		Engine:    engine,
		Available: false,
		Error:     strPtr(message),
		Databases: []databaseMetricDB{},
	}
}

// sectionBetween returns the text between startMarker and endMarker (or the
// end of output), or "" when startMarker is missing.
func sectionBetween(output, startMarker, endMarker string) string {
	start := strings.Index(output, startMarker)
	if start < 0 {
		return ""
	}
	start += len(startMarker)
	end := len(output)
	if endMarker != "" {
		if idx := strings.Index(output[start:], endMarker); idx >= 0 {
			end = start + idx
		}
	}
	return strings.TrimSpace(output[start:end])
}

// parsePostgresDatabaseMetricsOutput parses the postgres probe into an
// entry. Engine totals are the sum of the per-database counters.
func parsePostgresDatabaseMetricsOutput(output string) *databaseMetricsEntry {
	entry := &databaseMetricsEntry{
		Engine:    "postgres",
		Available: false,
		Error:     strPtr("psql returned no data"),
		Databases: []databaseMetricDB{},
	}
	rows := sectionBetween(output, "--DB-PG-ROWS--", "--DB-PG-MAXCONN--")
	if rows == "" {
		return entry
	}
	var connections, commits, rollbacks, blksRead, blksHit, deadlocks, tempBytes int64
	for _, rawLine := range strings.Split(rows, "\n") {
		fields := strings.Split(strings.TrimSpace(rawLine), "|")
		if len(fields) < 8 || fields[0] == "" {
			continue
		}
		vals := make([]int64, 7)
		ok := true
		for i, field := range fields[1:8] {
			v, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if !ok {
			continue
		}
		connections += vals[0]
		commits += vals[1]
		rollbacks += vals[2]
		blksRead += vals[3]
		blksHit += vals[4]
		deadlocks += vals[5]
		tempBytes += vals[6]
		entry.Databases = append(entry.Databases, databaseMetricDB{
			Name:        fields[0],
			Connections: int64Ptr(vals[0]),
			Commits:     int64Ptr(vals[1]),
			Rollbacks:   int64Ptr(vals[2]),
			BlksRead:    int64Ptr(vals[3]),
			BlksHit:     int64Ptr(vals[4]),
			Deadlocks:   int64Ptr(vals[5]),
		})
	}
	if len(entry.Databases) == 0 {
		for _, line := range strings.Split(rows, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "psql:") || strings.HasPrefix(line, "su:") {
				entry.Error = strPtr(line)
				break
			}
		}
		return entry
	}
	entry.Available = true
	entry.Error = nil
	entry.Connections = int64Ptr(connections)
	entry.Commits = int64Ptr(commits)
	entry.Rollbacks = int64Ptr(rollbacks)
	entry.BlksRead = int64Ptr(blksRead)
	entry.BlksHit = int64Ptr(blksHit)
	entry.Deadlocks = int64Ptr(deadlocks)
	entry.TempBytes = int64Ptr(tempBytes)
	if hit := ratio(blksHit, blksRead); hit != nil {
		entry.CacheHitRatio = hit
	}
	if maxConn := parseSectionInt(output, "--DB-PG-MAXCONN--", "--DB-PG-SHARED--"); maxConn != nil {
		entry.MaxConnections = maxConn
	}
	if shared := sectionBetween(output, "--DB-PG-SHARED--", "--DB-PG-VERSION--"); shared != "" {
		if bytes := parseSizeToBytes(shared); bytes != nil {
			entry.MemoryBytes = bytes
		}
	}
	if version := sectionBetween(output, "--DB-PG-VERSION--", ""); version != "" {
		trimmed := strings.TrimSpace(strings.TrimPrefix(version, "postgres (PostgreSQL) "))
		if trimmed != "" {
			entry.Version = strPtr(trimmed)
		}
	}
	return entry
}

// parseMySQLDatabaseMetricsOutput parses the mysql/mariadb probe into an
// entry, tagging the engine by the server version.
func parseMySQLDatabaseMetricsOutput(output string) *databaseMetricsEntry {
	entry := &databaseMetricsEntry{
		Engine:    "mysql",
		Available: false,
		Error:     strPtr("mysql returned no data"),
		Databases: []databaseMetricDB{},
	}
	status := sectionBetween(output, "--DB-MY-STATUS--", "--DB-MY-VARS--")
	if status == "" {
		return entry
	}
	values := parseNameValueLines(status)
	get := func(name string) *int64 {
		if raw, ok := values[name]; ok {
			if v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
				return int64Ptr(v)
			}
		}
		return nil
	}
	entry.Available = true
	entry.Error = nil
	entry.Connections = get("Threads_connected")
	entry.MaxUsedConnections = get("Max_used_connections")
	entry.ThreadsRunning = get("Threads_running")
	entry.Queries = get("Queries")
	entry.SlowQueries = get("Slow_queries")
	entry.UptimeSeconds = get("Uptime")
	entry.BytesReceived = get("Bytes_received")
	entry.BytesSent = get("Bytes_sent")

	pageSize := get("Innodb_page_size")
	pagesTotal := get("Innodb_buffer_pool_pages_total")
	pagesData := get("Innodb_buffer_pool_pages_data")
	pagesDirty := get("Innodb_buffer_pool_pages_dirty")
	if pageSize != nil {
		if pagesData != nil {
			entry.MemoryUsedBytes = int64Ptr(*pagesData * *pageSize)
		}
		if pagesDirty != nil {
			entry.MemoryDirtyBytes = int64Ptr(*pagesDirty * *pageSize)
		}
	}
	_ = pagesTotal
	if requests := get("Innodb_buffer_pool_read_requests"); requests != nil {
		if reads := get("Innodb_buffer_pool_reads"); reads != nil {
			if hit := ratio(*requests, *reads); hit != nil {
				entry.CacheHitRatio = hit
			}
		}
	}

	vars := parseNameValueLines(sectionBetween(output, "--DB-MY-VARS--", "--DB-MY-VERSION--"))
	if raw, ok := vars["innodb_buffer_pool_size"]; ok {
		if bytes := parseSizeToBytes(raw); bytes != nil {
			entry.MemoryBytes = bytes
		}
	}
	if raw, ok := vars["max_connections"]; ok {
		if v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
			entry.MaxConnections = int64Ptr(v)
		}
	}
	version := strings.TrimSpace(sectionBetween(output, "--DB-MY-VERSION--", ""))
	if version != "" {
		entry.Version = strPtr(version)
		if strings.Contains(strings.ToLower(version), "mariadb") {
			entry.Engine = "mariadb"
		}
	}
	return entry
}

// parseNameValueLines parses `<name>\t<value>` lines (mysql -N output).
func parseNameValueLines(output string) map[string]string {
	result := make(map[string]string)
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.SplitN(rawLine, "\t", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" {
			continue
		}
		result[strings.TrimSpace(fields[0])] = strings.TrimSpace(fields[1])
	}
	return result
}

func parseSectionInt(output, startMarker, endMarker string) *int64 {
	raw := strings.TrimSpace(sectionBetween(output, startMarker, endMarker))
	if raw == "" {
		return nil
	}
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return int64Ptr(v)
	}
	return nil
}

// parseSizeToBytes converts PostgreSQL/MySQL size strings ("128MB", "1GB",
// "8192") into bytes.
func parseSizeToBytes(raw string) *int64 {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return nil
	}
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"gb", 1 << 30}, {"g", 1 << 30},
		{"mb", 1 << 20}, {"m", 1 << 20},
		{"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	} {
		if strings.HasSuffix(trimmed, unit.suffix) {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
			multiplier = unit.factor
			break
		}
	}
	v, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return nil
	}
	return int64Ptr(v * multiplier)
}

func ratio(numerator, denominator int64) *float64 {
	total := numerator + denominator
	if total <= 0 {
		return nil
	}
	r := float64(numerator) / float64(total)
	return &r
}

func int64Ptr(v int64) *int64 { return &v }
