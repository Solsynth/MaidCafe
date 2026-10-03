package daemon

// Managed compose stacks.
//
// `container.update` runs a project's own compose command where the project
// lives, and a container created by compose usually records that directory in
// its labels. Not every tool records all of it, and not every recorded value is
// usable, so a host's stacks can also be *assigned* to the daemon: a scan — of
// the operator's configured roots, or of a single starting point they name —
// records every compose project it finds, and those stacks are then managed:
// pulled and recreated on request, and watched for what they are running.
//
// Nothing is guessed at request time. A stack is only ever acted on where the
// operator's own scan found it, or where a container's own labels point.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// composeStack is one project the daemon manages: where it lives, what it is
// made of, and when it was last seen on disk.
type composeStack struct {
	Project   string    `json:"project"`
	Directory string    `json:"directory"`
	Files     []string  `json:"files"`
	Services  []string  `json:"services"`
	ScannedAt time.Time `json:"scanned_at"`
}

// key identifies a stack for lookup. Project names are unique per host without
// regard to case — compose itself treats them that way, and a registry that
// held both `Web` and `web` would offer two answers for one project.
func (s composeStack) key() string { return strings.ToLower(s.Project) }

// composeStackStore is the daemon's registry of managed stacks. It is a JSON
// file rewritten as a whole: the registry is small (one record per project),
// and a rewrite is what makes a scan's several changes land together or not at
// all.
//
// The registry is reconstructible by design — it mirrors what is on disk, and a
// scan finds it again — so a file that cannot be read is reported and skipped
// rather than repaired. What it holds is which stacks the operator assigned,
// which the next scan re-establishes.
type composeStackStore struct {
	mu     sync.Mutex
	path   string
	logger *slog.Logger
	stacks map[string]composeStack
}

func newComposeStackStore(path string, logger *slog.Logger) *composeStackStore {
	store := &composeStackStore{path: path, logger: logger, stacks: map[string]composeStack{}}
	store.load()
	return store
}

func (s *composeStackStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("compose stack registry could not be read", "path", s.path, "error", err)
		}
		return
	}
	var stacks []composeStack
	if err := json.Unmarshal(data, &stacks); err != nil {
		s.logger.Warn("compose stack registry is not valid JSON; starting empty", "path", s.path, "error", err)
		return
	}
	for _, stack := range stacks {
		if stack.Project == "" || stack.Directory == "" {
			continue
		}
		s.stacks[stack.key()] = stack
	}
}

// List returns every managed stack, ordered by project name.
func (s *composeStackStore) List() []composeStack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *composeStackStore) listLocked() []composeStack {
	stacks := make([]composeStack, 0, len(s.stacks))
	for _, stack := range s.stacks {
		stacks = append(stacks, stack)
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Project < stacks[j].Project })
	return stacks
}

// Get returns the stack managing [project], matched without regard to case.
func (s *composeStackStore) Get(project string) (composeStack, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stack, ok := s.stacks[strings.ToLower(strings.TrimSpace(project))]
	return stack, ok
}

// Apply records a scan's answer: every stack it found is added or refreshed,
// and a managed stack whose directory has since disappeared is dropped — the
// scan is the operator's statement of what is on the host, so a stack that is
// no longer there is no longer managed. A stack outside this scan's roots is
// left alone: scanning one starting point says nothing about the others.
func (s *composeStackStore) Apply(scanned []composeStack) (added, updated, removed []composeStack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(scanned))
	for _, stack := range scanned {
		key := stack.key()
		seen[key] = struct{}{}
		existing, ok := s.stacks[key]
		switch {
		case !ok:
			added = append(added, stack)
		case existing.Directory != stack.Directory || !equalStrings(existing.Services, stack.Services):
			updated = append(updated, stack)
		}
		s.stacks[key] = stack
	}
	for key, stack := range s.stacks {
		if _, ok := seen[key]; ok {
			continue
		}
		if _, err := os.Stat(stack.Directory); err == nil {
			continue
		}
		removed = append(removed, stack)
		delete(s.stacks, key)
	}
	if len(added) == 0 && len(updated) == 0 && len(removed) == 0 {
		// A scan that changed nothing does not rewrite the file: the
		// ScannedAt it stamps is bookkeeping, not news.
		return nil, nil, nil
	}
	s.saveLocked()
	return added, updated, removed
}

// Remove forgets one stack, returning it when it was managed.
func (s *composeStackStore) Remove(project string) (composeStack, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(project))
	stack, ok := s.stacks[key]
	if !ok {
		return composeStack{}, false
	}
	delete(s.stacks, key)
	s.saveLocked()
	return stack, true
}

// saveLocked rewrites the registry: a temporary file in the same directory,
// fsynced and renamed, so a reader never sees half a registry and a crash
// leaves either the old one or the new one.
func (s *composeStackStore) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.listLocked(), "", "  ")
	if err != nil {
		s.logger.Error("compose stack registry could not be encoded", "error", err)
		return
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		s.logger.Error("compose stack registry directory could not be created", "path", directory, "error", err)
		return
	}
	temp, err := os.CreateTemp(directory, ".compose-stacks-*")
	if err != nil {
		s.logger.Error("compose stack registry could not be written", "error", err)
		return
	}
	name := temp.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		s.logger.Error("compose stack registry could not be written", "error", err)
		return
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		s.logger.Error("compose stack registry could not be synced", "error", err)
		return
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		s.logger.Error("compose stack registry could not be protected", "error", err)
		return
	}
	if err := temp.Close(); err != nil {
		s.logger.Error("compose stack registry could not be closed", "error", err)
		return
	}
	if err := os.Rename(name, s.path); err != nil {
		s.logger.Error("compose stack registry could not be replaced", "path", s.path, "error", err)
		return
	}
	name = ""
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// composeScanLimits bounds one scan. The depth keeps a scan inside the
// directories projects are kept in, and the file cap bounds the work a single
// request can cause however large the tree is.
type composeScanLimits struct {
	depth    int
	maxFiles int
}

// composeScanLimitsDefault is what a scan uses when the request and the
// configuration both leave a bound alone.
var composeScanLimitsDefault = composeScanLimits{depth: 3, maxFiles: 400}

// composeDefaultScanRoots is where a scan looks when the operator configured
// nothing: the directories compose projects are kept in on the hosts this
// daemon runs on.
var composeDefaultScanRoots = []string{
	"/opt",
	"/srv",
	"/root",
	"/home",
	"/etc/compose",
	"/var/lib/compose",
}

// scanComposeStacks walks [roots] and returns the projects it finds, newest
// view of each project's directory and files.
//
// A project is one directory: the compose files in it are merged the way
// compose merges them (the base files first, overrides last), and its services
// are the union of what those files declare. The project's name is the one its
// file declares, or the directory's name when it declares none — the same
// fallback compose itself applies.
func scanComposeStacks(
	ctx context.Context,
	roots []string,
	limits composeScanLimits,
) []composeStack {
	if limits.depth <= 0 {
		limits.depth = composeScanLimitsDefault.depth
	}
	if limits.maxFiles <= 0 {
		limits.maxFiles = composeScanLimitsDefault.maxFiles
	}
	byDirectory := map[string]*composeStackBuilder{}
	read := 0
	now := time.Now().UTC()
	for _, root := range roots {
		if ctx.Err() != nil || read >= limits.maxFiles {
			break
		}
		rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil || read >= limits.maxFiles {
				return fs.SkipAll
			}
			if entry.IsDir() {
				if path != root && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				if strings.Count(path, string(filepath.Separator))-rootDepth >= limits.depth {
					return fs.SkipDir
				}
				return nil
			}
			if !isComposeFileName(entry.Name()) {
				return nil
			}
			read++
			document, ok := readComposeDocument(path)
			if !ok || len(document.Services) == 0 {
				return nil
			}
			recordComposeFile(byDirectory, path, document, now)
			return nil
		})
	}
	stacks := make([]composeStack, 0, len(byDirectory))
	for _, builder := range byDirectory {
		stack := builder.stack
		stack.Files = orderComposeFiles(stack.Files)
		stack.Services = dedupeSorted(stack.Services)
		stacks = append(stacks, stack)
	}
	sort.Slice(stacks, func(i, j int) bool {
		if stacks[i].Project != stacks[j].Project {
			return stacks[i].Project < stacks[j].Project
		}
		return stacks[i].Directory < stacks[j].Directory
	})
	return stacks
}

// composeStackBuilder accumulates one directory's files into a stack. It tracks
// whether the project's name was declared, because a declaration outranks the
// directory's name: compose uses the declared name as the project, and would
// refuse `-p <directory>` against a file that declares another.
type composeStackBuilder struct {
	stack    composeStack
	declared bool
}

// recordComposeFile folds one parsed file into the project of its directory.
func recordComposeFile(
	byDirectory map[string]*composeStackBuilder,
	path string,
	document composeDocument,
	now time.Time,
) {
	directory := filepath.Dir(path)
	builder := byDirectory[directory]
	if builder == nil {
		builder = &composeStackBuilder{}
		builder.stack.Directory = directory
		builder.stack.Project = filepath.Base(directory)
		byDirectory[directory] = builder
	}
	// A declared name wins over the directory fallback, and the first
	// declaration wins over a later one: two files in one directory naming
	// different projects is not something this daemon can manage, and compose
	// itself would answer with the last of them.
	if name := strings.TrimSpace(document.Name); name != "" && !builder.declared {
		builder.stack.Project = name
		builder.declared = true
	}
	builder.stack.Files = append(builder.stack.Files, path)
	for service := range document.Services {
		builder.stack.Services = append(builder.stack.Services, service)
	}
	builder.stack.ScannedAt = now
}

// orderComposeFiles puts a project's files in the order compose merges them:
// the base files first, the overrides after, each group ordered. Passing them
// explicitly in this order reproduces what running compose in the directory
// would load, including for a project whose files are not named the defaults.
func orderComposeFiles(files []string) []string {
	ordered := append([]string{}, files...)
	sort.Slice(ordered, func(i, j int) bool {
		leftOverride := isComposeOverrideFile(ordered[i])
		rightOverride := isComposeOverrideFile(ordered[j])
		if leftOverride != rightOverride {
			return !leftOverride
		}
		return ordered[i] < ordered[j]
	})
	return ordered
}

func isComposeOverrideFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.Contains(name, ".override.") ||
		strings.HasSuffix(name, "override.yml") ||
		strings.HasSuffix(name, "override.yaml")
}

// dedupeSorted returns the values, sorted, without repeats: one service is one
// service however many of a project's files declare it.
func dedupeSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for i, value := range values {
		if i == 0 || values[i-1] != value {
			out = append(out, value)
		}
	}
	return out
}

// isComposeFileName reports whether a file can be a compose declaration. The
// standard names are what both tools look for; any other YAML is read too,
// because a project may name its file (`-f stack.yml`) and the content check is
// what decides.
func isComposeFileName(name string) bool {
	lowered := strings.ToLower(name)
	return strings.HasSuffix(lowered, ".yml") || strings.HasSuffix(lowered, ".yaml")
}

// composeDocument is the part of a compose file a scan reads: the project name
// it declares (optional — compose falls back to the directory's name) and the
// services it defines.
type composeDocument struct {
	Name     string                    `yaml:"name"`
	Services map[string]map[string]any `yaml:"services"`
}

// readComposeDocument reads one candidate file, bounded by the size cap: a
// compose file is a declaration, and anything past the cap is not one.
func readComposeDocument(path string) (composeDocument, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > composeFileMaxBytes {
		return composeDocument{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > composeFileMaxBytes {
		return composeDocument{}, false
	}
	var document composeDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return composeDocument{}, false
	}
	return document, true
}

// composeFileMaxBytes caps one candidate read.
const composeFileMaxBytes = 256 << 10

// composeTool is one command that runs compose commands: the runtime's own
// subcommand, or the standalone tool for that runtime.
type composeTool struct {
	command string
	// prefix is what precedes the compose arguments. The runtime's subcommand
	// needs the `compose` word; the standalone tools are the command itself.
	//
	// Neither carries an ANSI flag. The runtime's `compose` subcommand is a
	// dispatcher, not an implementation: podman execs whichever provider is
	// installed — often `podman-compose`, whose flag is `--no-ansi` — so a flag
	// spelled for one provider reaches a tool that rejects it, and an
	// unrecognized flag fails the command before anything runs
	// (`podman-compose: error: argument command: invalid choice: 'never'`).
	// Asking for none costs nothing: compose writes to a pipe here and never to
	// a terminal, and its own `ansi: auto` turns colors off for exactly that
	// case. A colored line in captured output is cheaper than a failed pull.
	prefix []string
}

// composeTools is the compose invocations for the runtime at [runtimePath], in
// the order a step tries them.
//
// The order differs per runtime, because their `compose` subcommand is a
// different kind of thing. Docker *implements* compose — the plugin is the tool
// — so `docker compose` comes first and the standalone binary is the fallback
// for a host that has only that. Podman does not implement compose at all: its
// `podman compose` is a wrapper that execs whichever provider is installed,
// normally the very `podman-compose` the host also has. There the direct tool
// goes first and the wrapper second, because the wrapper only adds a layer that
// evaluates the arguments before forwarding them — and that layer is where a
// flag one tool accepts and the other rejects kills a stack update before it
// runs (`--ansi never`, the plugin's spelling, against podman-compose).
func composeTools(runtimePath string) []composeTool {
	runtimeTool := composeTool{command: runtimePath, prefix: []string{"compose"}}
	standalone := standaloneComposePath(runtimePath)
	if standalone == "" {
		return []composeTool{runtimeTool}
	}
	direct := composeTool{command: standalone}
	if composeToolName(runtimePath) == "podman-compose" {
		return []composeTool{direct, runtimeTool}
	}
	return []composeTool{runtimeTool, direct}
}

// composeToolName is the standalone compose tool for the runtime at [path]
// — `podman-compose`, `docker-compose` — or "" when this daemon does not
// recognize the runtime.
//
// The tool is derived from the runtime's own name so a docker host is never
// sent to podman-compose: the two write to different image stores, and pulling
// into the wrong one would leave the container exactly where it was.
func composeToolName(runtimePath string) string {
	name := strings.ToLower(filepath.Base(runtimePath))
	switch {
	case strings.Contains(name, "podman"):
		return "podman-compose"
	case strings.Contains(name, "docker"):
		return "docker-compose"
	}
	return ""
}

// standaloneComposePath resolves that tool on this host, or "" when it has none.
func standaloneComposePath(runtimePath string) string {
	name := composeToolName(runtimePath)
	if name == "" {
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// validateComposeScanPath checks a starting point or root a request names: an
// absolute directory with no parent segment, which is the same rule a compose
// directory recorded in a container's labels has to satisfy.
func validateComposeScanPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("path %q is not absolute", value)
	}
	if strings.ContainsAny(value, "\x00\n\r") || strings.Contains(filepath.ToSlash(value), "/../") {
		return "", fmt.Errorf("path %q is not usable", value)
	}
	cleaned := filepath.Clean(value)
	if strings.HasSuffix(filepath.ToSlash(cleaned), "/..") {
		return "", fmt.Errorf("path %q is not usable", value)
	}
	return cleaned, nil
}
