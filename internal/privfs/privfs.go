// Package privfs implements the privileged-file helper (`maidkit-priv`), the
// only root-capable component in the MaidCafe stack.
//
// The daemon runs unprivileged and reaches root operations by running this
// helper through a passwordless sudoers rule. A sudoers rule alone is not an
// authorization boundary — `sudo tee` is root, `sudo systemctl` is root, and a
// wildcard rule over either can be pointed anywhere with the right argument —
// so the helper is the boundary instead: it accepts a *profile name*, never a
// path, and resolves that name against a root-owned configuration file the
// daemon cannot write. A caller that appends arbitrary arguments still cannot
// name a directory the operator did not declare, and every operation is
// confined to the profile's directory by os.Root, so a symlink planted inside
// it cannot redirect a write outside it.
//
// Argument parsing is deliberately positional and exact: a fixed argument
// count means the sudoers pattern needs no wildcard in the middle of the
// command, and an appended extra argument is a refusal rather than a
// reinterpretation.
package privfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// DefaultConfigPath is the root-owned profile file the helper reads. It lives
// outside /etc/maidcafe because that directory is the daemon's own state: the
// daemon must not be able to replace the file that authorizes its privileges.
const DefaultConfigPath = "/etc/maidkit/priv.toml"

// DefaultHelperPath is where the installer places this binary.
const DefaultHelperPath = "/usr/local/libexec/maidkit-priv"

// MaxContentBytes bounds one write's stdin. Profiles describe configuration
// and small data files, not bulk storage; a larger payload belongs in the
// file API's own roots.
const MaxContentBytes = 4 << 20

// Exit codes. A refusal is distinct from an I/O failure so an operator can
// tell a policy answer from a broken filesystem.
const (
	ExitOK        = 0
	ExitUsage     = 2
	ExitRefused   = 3
	ExitIO        = 4
	ExitNotRoot   = 5
	ExitConfigBad = 6
)

// allowedModeSet is every permission bit pattern a profile may grant. Setuid,
// setgid and sticky bits are excluded on purpose: the helper makes files that
// root executes, and a setuid bit here would escalate whoever can write the
// contents. Group and other write are excluded too, which is what keeps a
// privileged write from becoming a way to plant a world-writable file.
var allowedModeSet = map[fs.FileMode]bool{
	0o600: true,
	0o640: true,
	0o644: true,
	0o700: true,
	0o750: true,
	0o755: true,
}

// profileNamePattern constrains a profile name so it is unambiguous in both
// the config file and the helper's argv.
var profileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidProfileName reports whether a name is a usable profile identifier. The
// daemon validates its own configuration with this so an operator learns about
// a typo at config load rather than at the first privileged write.
func ValidProfileName(name string) bool {
	return profileNamePattern.MatchString(name)
}

// Profile is one declared directory the helper may write into, plus the modes
// it may use there.
type Profile struct {
	Name string
	// Path is the resolved directory: the configured path with symlinks
	// followed once at load, so a later swap of a path component cannot move
	// the profile somewhere else after it was validated.
	Path  string
	Modes map[fs.FileMode]bool
}

// UnitGrant authorizes the systemd verbs a unit may receive.
//
// This is the same shape as a file profile, and for the same reason: a sudoers
// rule over `systemctl` is not a boundary, because `systemctl` takes the unit
// from its argument — and a unit *file* is root code execution. The allowlist
// lives here, in a root-owned file the daemon cannot write, so the daemon can
// name only units an operator declared and only the verbs they granted.
type UnitGrant struct {
	Unit  string
	Verbs map[string]bool
}

// Set is the loaded profile file.
type Set struct {
	byName map[string]*Profile
	units  map[string]*UnitGrant
}

// systemdUnitPattern is the unit name a grant may name. It matches the daemon's
// own accepted shape and additionally requires a first character that is not a
// dash: a unit named "-H.service" would otherwise reach systemctl's option
// parser, where "-H.service" reads as the --host option with the value
// ".service".
var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9:._@][A-Za-z0-9:._@-]*\.service$`)

// allowedSystemdVerbs is every systemd verb a grant may name. The set matches
// the daemon's native systemd operations, so a grant cannot authorize a verb
// the daemon would never ask for and an operator cannot grant more than it can
// use.
var allowedSystemdVerbs = map[string]bool{
	"start": true, "stop": true, "restart": true,
	"reload": true, "enable": true, "disable": true,
}

// validSystemdVerb reports whether [verb] is a systemd verb the helper runs.
func validSystemdVerb(verb string) bool { return allowedSystemdVerbs[verb] }

// SystemdVerbs lists the grantable verbs for an error message, sorted.
func SystemdVerbs() string {
	verbs := make([]string, 0, len(allowedSystemdVerbs))
	for verb := range allowedSystemdVerbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ", ")
}

// systemctlPath is where systemctl is looked for, in order. A fixed list rather
// than PATH: the helper runs as root, and resolving an absolute path from the
// environment is one more thing an attacker could influence.
var systemctlPaths = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// FindSystemctl returns the systemctl binary to run.
func FindSystemctl() (string, error) {
	for _, candidate := range systemctlPaths {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("systemctl was not found in %s", strings.Join(systemctlPaths, ", "))
}

// Options controls loading. RequireRootOwner is the production setting: the
// profile file must be owned by root and not writable by anyone else, or a
// process holding the daemon's own file access could rewrite its privileges.
// Tests disable it because they cannot create root-owned files.
type Options struct {
	RequireRootOwner bool
}

// fileConfig is the on-disk shape.
type fileConfig struct {
	Profiles []struct {
		Name  string   `mapstructure:"name"`
		Path  string   `mapstructure:"path"`
		Modes []string `mapstructure:"modes"`
	} `mapstructure:"profiles"`
	Systemd []struct {
		Unit  string   `mapstructure:"unit"`
		Verbs []string `mapstructure:"verbs"`
	} `mapstructure:"systemd"`
}

// LoadSet reads and fully validates the profile file. Every rejection here is
// a refusal to serve any request, never a warning: a profile the helper
// cannot trust is indistinguishable from no profile at all.
func LoadSet(configPath string, opts Options) (*Set, error) {
	info, err := os.Lstat(configPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symbolic link", configPath)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", configPath)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s must not be group- or other-writable (mode %04o)", configPath, info.Mode().Perm())
	}
	if opts.RequireRootOwner && !ownedByRoot(info) {
		return nil, fmt.Errorf("%s must be owned by root", configPath)
	}

	reader := viper.New()
	reader.SetConfigFile(configPath)
	if err := reader.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	var parsed fileConfig
	if err := reader.Unmarshal(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	set := &Set{
		byName: make(map[string]*Profile, len(parsed.Profiles)),
		units:  make(map[string]*UnitGrant, len(parsed.Systemd)),
	}
	for i, entry := range parsed.Profiles {
		profile, err := buildProfile(entry.Name, entry.Path, entry.Modes)
		if err != nil {
			return nil, fmt.Errorf("profiles[%d]: %w", i, err)
		}
		if _, exists := set.byName[profile.Name]; exists {
			return nil, fmt.Errorf("profiles[%d]: name %q is duplicated", i, profile.Name)
		}
		set.byName[profile.Name] = profile
	}
	for i, entry := range parsed.Systemd {
		grant, err := buildUnitGrant(entry.Unit, entry.Verbs)
		if err != nil {
			return nil, fmt.Errorf("systemd[%d]: %w", i, err)
		}
		if _, exists := set.units[grant.Unit]; exists {
			return nil, fmt.Errorf("systemd[%d]: unit %q is duplicated", i, grant.Unit)
		}
		set.units[grant.Unit] = grant
	}
	if len(set.byName) == 0 && len(set.units) == 0 {
		return nil, fmt.Errorf("%s declares no profiles and no systemd units", configPath)
	}
	return set, nil
}

// buildUnitGrant validates one systemd entry.
func buildUnitGrant(unit string, verbs []string) (*UnitGrant, error) {
	if !systemdUnitPattern.MatchString(unit) {
		return nil, fmt.Errorf("unit %q must be a .service name such as nginx.service", unit)
	}
	if len(verbs) == 0 {
		return nil, fmt.Errorf("unit %q declares no verbs", unit)
	}
	allowed := make(map[string]bool, len(verbs))
	for _, verb := range verbs {
		if !validSystemdVerb(verb) {
			return nil, fmt.Errorf("unit %q verb %q is not one of: %s", unit, verb, SystemdVerbs())
		}
		allowed[verb] = true
	}
	return &UnitGrant{Unit: unit, Verbs: allowed}, nil
}

// LookupUnit resolves a systemd unit, or lists the granted ones so a
// misconfigured operator can see what the helper will actually accept.
func (s *Set) LookupUnit(unit string) (*UnitGrant, error) {
	grant, ok := s.units[unit]
	if !ok {
		names := make([]string, 0, len(s.units))
		for name := range s.units {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, fmt.Errorf("no systemd units are granted (add a [[systemd]] entry naming %s)", unit)
		}
		return nil, fmt.Errorf("unit %q is not granted (granted: %s)", unit, strings.Join(names, ", "))
	}
	return grant, nil
}

// GrantedUnits lists the granted units, sorted.
func (s *Set) GrantedUnits() []string {
	units := make([]string, 0, len(s.units))
	for unit := range s.units {
		units = append(units, unit)
	}
	sort.Strings(units)
	return units
}

// CheckVerb reports whether the grant authorizes [verb].
func (g *UnitGrant) CheckVerb(verb string) error {
	if !validSystemdVerb(verb) {
		return fmt.Errorf("verb %q is not one of: %s", verb, SystemdVerbs())
	}
	if !g.Verbs[verb] {
		granted := make([]string, 0, len(g.Verbs))
		for name := range g.Verbs {
			granted = append(granted, name)
		}
		sort.Strings(granted)
		return fmt.Errorf("verb %q is not granted for %s (granted: %s)", verb, g.Unit, strings.Join(granted, ", "))
	}
	return nil
}

// SystemdArgv builds the command that runs one granted unit action.
//
// The unit is passed after `--`, which ends systemctl's option parsing, and the
// unit name pattern already refuses a leading dash — two independent guards
// against a unit name being read as an option rather than an operand.
func (g *UnitGrant) SystemdArgv(verb string) ([]string, error) {
	systemctl, err := FindSystemctl()
	if err != nil {
		return nil, err
	}
	return g.systemdArgv(systemctl, verb)
}

// systemdArgv is the argv construction with the binary supplied, so its shape
// — which is what keeps a unit name from being read as an option — is testable
// on a machine that has no systemctl.
func (g *UnitGrant) systemdArgv(systemctl, verb string) ([]string, error) {
	if err := g.CheckVerb(verb); err != nil {
		return nil, err
	}
	return []string{
		systemctl,
		"--no-ask-password",
		"--no-pager",
		verb,
		"--",
		g.Unit,
	}, nil
}

// buildProfile validates one entry and resolves its directory.
func buildProfile(name, path string, modes []string) (*Profile, error) {
	if !profileNamePattern.MatchString(name) {
		return nil, fmt.Errorf("name %q must match %s", name, profileNamePattern)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("path %q must be an absolute, clean path", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("path %q is not usable: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("path %q is not usable: %w", resolved, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", resolved)
	}
	if len(modes) == 0 {
		return nil, fmt.Errorf("profile %q declares no modes", name)
	}
	allowed := make(map[fs.FileMode]bool, len(modes))
	for _, raw := range modes {
		mode, err := ParseMode(raw)
		if err != nil {
			return nil, fmt.Errorf("profile %q %w", name, err)
		}
		allowed[mode] = true
	}
	return &Profile{Name: name, Path: resolved, Modes: allowed}, nil
}

// ownedByRoot reports whether a file is owned by the root account. Windows has
// no equivalent owner to compare, so it reports false and the helper stays
// unusable there.
func ownedByRoot(info fs.FileInfo) bool {
	uid, ok := fileOwner(info)
	return ok && uid == 0
}

// ParseMode accepts the octal form the config and the command line both use.
// A three-digit value is read as-is ("644"); a four-digit value must have a
// leading zero (so "0644" is accepted and "6440" is not silently a special
// mode). Only modes in [allowedModeSet] survive.
func ParseMode(raw string) (fs.FileMode, error) {
	text := strings.TrimSpace(raw)
	if len(text) != 3 && len(text) != 4 {
		return 0, fmt.Errorf("mode %q must be three or four octal digits", raw)
	}
	if len(text) == 4 && text[0] != '0' {
		return 0, fmt.Errorf("mode %q must have a leading 0", raw)
	}
	value, err := parseOctal(text)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", raw)
	}
	mode := fs.FileMode(value)
	if !allowedModeSet[mode] {
		return 0, fmt.Errorf("mode %q is not permitted (allowed: %s)", raw, ModeList())
	}
	return mode, nil
}

func parseOctal(text string) (uint32, error) {
	var value uint32
	for _, digit := range text {
		if digit < '0' || digit > '7' {
			return 0, errors.New("not octal")
		}
		value = value<<3 | uint32(digit-'0')
	}
	if value > 0o7777 {
		return 0, errors.New("out of range")
	}
	return value, nil
}

// ModeList renders the permitted modes for an error message, in a stable
// order so two runs print the same text.
func ModeList() string {
	modes := make([]int, 0, len(allowedModeSet))
	for mode := range allowedModeSet {
		modes = append(modes, int(mode))
	}
	sort.Ints(modes)
	rendered := make([]string, 0, len(modes))
	for _, mode := range modes {
		rendered = append(rendered, fmt.Sprintf("%04o", mode))
	}
	return strings.Join(rendered, ", ")
}

// Lookup resolves a profile name, or explains which names exist so a
// misconfigured daemon is diagnosable from its own response.
func (s *Set) Lookup(name string) (*Profile, error) {
	profile, ok := s.byName[name]
	if !ok {
		names := make([]string, 0, len(s.byName))
		for candidate := range s.byName {
			names = append(names, candidate)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown profile %q (configured: %s)", name, strings.Join(names, ", "))
	}
	return profile, nil
}

// Names lists the configured profiles, sorted.
func (s *Set) Names() []string {
	names := make([]string, 0, len(s.byName))
	for name := range s.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Write creates or replaces one file inside the profile's directory.
//
// The content lands through a temporary file in the destination directory and
// is renamed into place, so a reader never observes a half-written config and
// a failed write leaves the previous file untouched. The temporary name is
// created with O_EXCL, so a pre-planted file at that name is a refusal rather
// than a file the helper would happily overwrite.
func (s *Set) Write(profile *Profile, rel string, mode fs.FileMode, data []byte) error {
	rel, err := safeRel(rel)
	if err != nil {
		return err
	}
	if err := profile.checkMode(mode); err != nil {
		return err
	}
	if len(data) > MaxContentBytes {
		return fmt.Errorf("content is %d bytes, over the %d-byte limit", len(data), MaxContentBytes)
	}
	root, err := os.OpenRoot(profile.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	// A destination that is a directory would make the rename fail with a
	// confusing errno; refuse it explicitly instead.
	if info, statErr := root.Lstat(rel); statErr == nil && info.IsDir() {
		return fmt.Errorf("%s is a directory", rel)
	}
	dir := filepath.Dir(rel)
	temp := filepath.Join(dir, tempName())
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		root.Remove(temp)
		return err
	}
	// The mode is set again after the write because the process umask has
	// already filtered the create mode; the profile's mode is the operator's
	// decision, not the umask's.
	if err := file.Chmod(mode); err != nil {
		file.Close()
		root.Remove(temp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		root.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		root.Remove(temp)
		return err
	}
	if err := root.Rename(temp, rel); err != nil {
		root.Remove(temp)
		return err
	}
	return nil
}

// Mkdir creates the named directory and any missing parents inside the
// profile's directory.
func (s *Set) Mkdir(profile *Profile, rel string, mode fs.FileMode) error {
	rel, err := safeRel(rel)
	if err != nil {
		return err
	}
	if err := profile.checkMode(mode); err != nil {
		return err
	}
	root, err := os.OpenRoot(profile.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(rel, mode); err != nil {
		return err
	}
	// MkdirAll applies the mode to every directory it creates and is filtered
	// by the umask, so the final directory is set explicitly.
	return root.Chmod(rel, mode)
}

// Remove unlinks one file inside the profile's directory. Directories are
// refused: removing a tree is not something a profile should authorize, and a
// recursive delete cannot be undone by reviewing a config file.
func (s *Set) Remove(profile *Profile, rel string) error {
	rel, err := safeRel(rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(profile.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("refusing to remove the directory %s", rel)
	}
	return root.Remove(rel)
}

// checkMode reports whether the profile grants this mode.
func (p *Profile) checkMode(mode fs.FileMode) error {
	if !allowedModeSet[mode] {
		return fmt.Errorf("mode %04o is not permitted (allowed: %s)", mode, ModeList())
	}
	if !p.Modes[mode] {
		return fmt.Errorf("mode %04o is not granted by profile %q (granted: %s)", mode, p.Name, p.modeList())
	}
	return nil
}

func (p *Profile) modeList() string {
	modes := make([]int, 0, len(p.Modes))
	for mode := range p.Modes {
		modes = append(modes, int(mode))
	}
	sort.Ints(modes)
	rendered := make([]string, 0, len(modes))
	for _, mode := range modes {
		rendered = append(rendered, fmt.Sprintf("%04o", mode))
	}
	return strings.Join(rendered, ", ")
}

// safeRel validates a profile-relative path. The helper never accepts an
// absolute path: the directory comes from the profile, and a caller that
// could name the directory could name any directory.
func safeRel(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("a relative path is required")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("the path must not contain NUL")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("the path must be relative to the profile")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q resolves outside the profile", rel)
	}
	return clean, nil
}

// tempName returns an unpredictable temporary name. A predictable one would
// let a caller who can write in the directory pre-create it and turn the
// helper's O_EXCL into a denial, or plant a symlink at it.
func tempName() string {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to something
		// unpredictable enough that the O_EXCL check still holds.
		return fmt.Sprintf(".maidkit-priv-%d", time.Now().UnixNano())
	}
	return ".maidkit-priv-" + hex.EncodeToString(suffix[:])
}

// Entry is the audit line the helper writes, one per attempt, successful or
// refused. It goes to stderr as a single JSON object: the daemon captures it
// and journals it, and systemd's own logging picks it up when the helper is
// run by hand. The identity fields come from sudo's environment, so they
// describe the account that actually invoked the helper rather than one the
// caller claims.
type Entry struct {
	Time    string `json:"time"`
	Event   string `json:"event"`
	Verb    string `json:"verb"`
	Profile string `json:"profile,omitempty"`
	// Unit is the systemd unit a systemd action targeted.
	Unit      string `json:"unit,omitempty"`
	Path      string `json:"path,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Digest    string `json:"sha256,omitempty"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	EUID      int    `json:"euid"`
	Invoker   string `json:"invoker,omitempty"`
	InvokerID string `json:"invoker_uid,omitempty"`
}

func writeEntry(stderr io.Writer, entry Entry) {
	entry.Time = time.Now().UTC().Format(time.RFC3339Nano)
	entry.Event = "privfs"
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	fmt.Fprintln(stderr, string(encoded))
}

// Digest renders the sha256 of a payload for the audit line, so a privileged
// write can be matched to the bytes a caller sent without storing them.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
