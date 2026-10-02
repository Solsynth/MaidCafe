package privfs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Package operations. A package manager run as root is one of the largest
// grants this helper can make: a package's maintainer scripts run as root, so
// "install" is "execute code from the distribution's repositories", and even a
// perfectly well-formed name reaches code the operator did not write. That is
// why the grant is per verb, and why the *manager* is declared here rather than
// chosen by the caller.
//
// The name a caller supplies is the one thing it controls, and it is the thing
// that decides which package is fetched. It cannot be a path (a .deb is root
// code execution from an arbitrary file), a URL, a version pin or a file name,
// because the accepted pattern admits only the distribution's own name
// grammar. See [packageNamePattern].
//
// This is deliberately not a general "run apt" grant: there is no way to reach
// a flag, a subcommand or a second operand from a caller.

// packageManagers maps a declared manager to the binary its verbs run. A
// manager that is absent cannot be granted at all.
//
// Homebrew is deliberately absent: it installs into a user-owned prefix and is
// designed to run without root, so it has no business going through a
// root-capable helper — and a root brew would write into root's home, which is
// a worse outcome than the ordinary one.
var packageManagers = map[string]string{
	"apt":    "apt-get",
	"dnf":    "dnf",
	"yum":    "yum",
	"pacman": "pacman",
	"zypper": "zypper",
	"apk":    "apk",
	"xbps":   "xbps-install",
}

// allowedPackageVerbs is every verb a grant may name. The set matches the
// daemon's native package operations.
var allowedPackageVerbs = map[string]bool{
	"refresh": true,
	"upgrade": true,
	"install": true,
	"remove":  true,
}

// packageNamePattern is the package name a caller may supply.
//
// It is the intersection of the distributions' own grammars rather than any one
// of them, because the differences are all in syntax a caller has no business
// controlling:
//
//   - no "/" — apt reads a name containing a slash as a file path or a URL, and
//     "apt-get install ./pkg.deb" is root execution of a file the caller chose.
//     It also reads "name/release" as a release selector.
//   - no "=" — acp, dnf and zypper read "name=version" as a version pin, which
//     lets a caller name a different source than the repositories' current one.
//   - no ":" — apt's "name:arch" qualifier, and zypper's "name:repo".
//   - no "@" — pacman's version and group syntax.
//   - no leading "-" — an argument beginning with a dash is an option, whatever
//     precedes it on the command line.
//
// What survives is the plain name a package is installed by. A caller who wants
// a specific version gets the repositories' current one, which is the version
// the operator's own `upgrade` grant would have moved it to anyway.
var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ValidPackageName reports whether [name] is a package name this helper will
// pass to a manager. Exported so the daemon can reject one at the request rather
// than in the helper's audit line, and so both agree on the grammar.
func ValidPackageName(name string) bool {
	return packageNamePattern.MatchString(name)
}

// PackageGrant is one declared package manager and the verbs it may receive.
type PackageGrant struct {
	// Manager is the key into [packageManagers], never a binary path.
	Manager string
	Verbs   map[string]bool
}

// PackageManagers lists the grantable managers, sorted, for an error message.
func PackageManagers() string {
	names := make([]string, 0, len(packageManagers))
	for name := range packageManagers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// PackageVerbs lists the grantable verbs, sorted, for an error message.
func PackageVerbs() string {
	verbs := make([]string, 0, len(allowedPackageVerbs))
	for verb := range allowedPackageVerbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ", ")
}

// validPackageVerb reports whether [verb] is a verb the helper runs.
func validPackageVerb(verb string) bool { return allowedPackageVerbs[verb] }

// buildPackageGrant validates the [packages] table.
func buildPackageGrant(manager string, verbs []string) (*PackageGrant, error) {
	manager = strings.ToLower(strings.TrimSpace(manager))
	if manager == "" {
		return nil, fmt.Errorf("manager is required (one of: %s)", PackageManagers())
	}
	if _, ok := packageManagers[manager]; !ok {
		return nil, fmt.Errorf("manager %q is not one of: %s", manager, PackageManagers())
	}
	if len(verbs) == 0 {
		return nil, fmt.Errorf("manager %q declares no verbs", manager)
	}
	allowed := make(map[string]bool, len(verbs))
	for _, verb := range verbs {
		verb = strings.ToLower(strings.TrimSpace(verb))
		if !validPackageVerb(verb) {
			return nil, fmt.Errorf("verb %q is not one of: %s", verb, PackageVerbs())
		}
		allowed[verb] = true
	}
	return &PackageGrant{Manager: manager, Verbs: allowed}, nil
}

// verbList renders the granted verbs, sorted, for the `fs profiles` listing.
func (g *PackageGrant) verbList() string {
	verbs := make([]string, 0, len(g.Verbs))
	for verb := range g.Verbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ",")
}

// CheckVerb reports whether the grant authorizes [verb].
func (g *PackageGrant) CheckVerb(verb string) error {
	if !validPackageVerb(verb) {
		return fmt.Errorf("verb %q is not one of: %s", verb, PackageVerbs())
	}
	if !g.Verbs[verb] {
		granted := make([]string, 0, len(g.Verbs))
		for name := range g.Verbs {
			granted = append(granted, name)
		}
		sort.Strings(granted)
		return fmt.Errorf("verb %q is not granted for %s (granted: %s)", verb, g.Manager, strings.Join(granted, ", "))
	}
	return nil
}

// managerBinary is the command a verb runs. It is a bare name, resolved by
// [FindManager] against a fixed list — never from PATH, because the helper runs
// as root and PATH is one more thing a caller could influence.
func managerBinary(manager, verb string) string {
	if manager == "xbps" && verb == "remove" {
		// The BSD-family split: installs and removals are separate binaries.
		return "xbps-remove"
	}
	return packageManagers[manager]
}

// PackageArgv builds the command that runs one granted verb.
//
// [name] is required by install and remove and refused by refresh and upgrade,
// so a caller cannot smuggle an operand into a verb that has none.
//
// The package name follows "--", which ends option parsing in every manager
// here. That is defense in depth rather than the boundary: the name pattern
// already refuses a leading dash, so the argument could not have been read as an
// option even without it.
func (g *PackageGrant) PackageArgv(verb, name string) ([]string, error) {
	if err := g.CheckVerb(verb); err != nil {
		return nil, err
	}
	return PackageCommand(g.Manager, verb, name)
}

// PackageCommand builds the argv for one package operation, with the manager's
// command name in argv[0] rather than a resolved path.
//
// It is exported, and the grant method above is a thin wrapper, so the helper
// and the daemon's unprivileged fallback cannot disagree about what a verb
// means: there is one table of invocations, not two that drift.
func PackageCommand(manager, verb, name string) ([]string, error) {
	if !ValidPackageManager(manager) {
		return nil, fmt.Errorf("package manager %q is not one of: %s", manager, PackageManagers())
	}
	if !ValidPackageVerb(verb) {
		return nil, fmt.Errorf("package verb %q is not one of: %s", verb, PackageVerbs())
	}
	switch verb {
	case "install", "remove":
		if !ValidPackageName(name) {
			return nil, fmt.Errorf("package name %q must be a plain name matching %s (a path, a URL, a version pin and an architecture qualifier are not accepted)", name, packageNamePattern)
		}
	case "refresh", "upgrade":
		if name != "" {
			return nil, fmt.Errorf("verb %q takes no package name", verb)
		}
	}
	binary := managerBinary(manager, verb)
	args := packageArgs(manager, verb, name)
	return append([]string{binary}, args...), nil
}

// ValidPackageManager reports whether [manager] is a manager this package can
// build a command for.
func ValidPackageManager(manager string) bool {
	_, ok := packageManagers[manager]
	return ok
}

// ValidPackageVerb reports whether [verb] is a package verb.
func ValidPackageVerb(verb string) bool { return allowedPackageVerbs[verb] }

// PackageManagerPreference is the order the daemon probes managers in when it
// is building its own command instead of asking the helper. The order is the
// hosts' own: apt is by far the most common, and a host that has both yum and
// dnf is a RHEL host whose yum is the wrapper.
func PackageManagerPreference() []string {
	return []string{"apt", "dnf", "yum", "pacman", "zypper", "apk", "xbps"}
}

// packageArgs is the per-manager argument tail. It is separate from
// [PackageArgv] so the shape of every manager's invocation is testable without
// that manager being installed.
//
// Every manager is asked for a non-interactive run: these commands run with no
// terminal and a caller waiting on the result, so a prompt would hang until the
// op times out with nothing to show for it.
func packageArgs(manager, verb, name string) []string {
	switch manager {
	case "apt":
		// The upgrade and install verbs are run with DEBIAN_FRONTEND in
		// PackageEnv rather than through `env`, so no extra binary is involved.
		switch verb {
		case "refresh":
			return []string{"update"}
		case "upgrade":
			return []string{"-y", "upgrade"}
		default:
			return []string{"-y", verb, "--", name}
		}
	case "dnf", "yum":
		// dnf and yum share a CLI; the binary differs (see managerBinary).
		switch verb {
		case "refresh":
			return []string{"makecache"}
		case "upgrade":
			return []string{"-y", "upgrade"}
		default:
			return []string{"-y", verb, "--", name}
		}
	case "pacman":
		switch verb {
		case "refresh":
			return []string{"-Sy"}
		case "upgrade":
			return []string{"--noconfirm", "-Syu"}
		case "install":
			return []string{"--noconfirm", "-S", "--", name}
		default:
			return []string{"--noconfirm", "-R", "--", name}
		}
	case "zypper":
		switch verb {
		case "refresh":
			return []string{"--non-interactive", "refresh"}
		case "upgrade":
			return []string{"--non-interactive", "update"}
		default:
			return []string{"--non-interactive", verb, "--", name}
		}
	case "apk":
		switch verb {
		case "refresh":
			return []string{"update"}
		case "upgrade":
			return []string{"upgrade"}
		case "install":
			return []string{"add", "--", name}
		default:
			return []string{"del", "--", name}
		}
	case "xbps":
		switch verb {
		case "refresh":
			return []string{"-S"}
		case "upgrade":
			return []string{"-yu"}
		case "install":
			return []string{"-y", "--", name}
		default:
			return []string{"-y", "--", name}
		}
	}
	return nil
}

// PackageEnv is the environment additions a verb needs. apt's is the one that
// matters: without DEBIAN_FRONTEND a package with a conffile prompt or a
// debconf question waits on a terminal that is not there.
func PackageEnv(manager string) []string {
	if manager == "apt" {
		return []string{"DEBIAN_FRONTEND=noninteractive"}
	}
	return nil
}

// managerPaths is where a manager's binary is looked for, in order. A fixed
// list rather than PATH: the helper runs as root, and resolving an absolute
// path from the environment is one more thing a caller could influence.
var managerPaths = map[string][]string{
	"apt-get":      {"/usr/bin/apt-get", "/bin/apt-get"},
	"dnf":          {"/usr/bin/dnf", "/bin/dnf"},
	"yum":          {"/usr/bin/yum", "/bin/yum"},
	"pacman":       {"/usr/bin/pacman", "/bin/pacman"},
	"zypper":       {"/usr/bin/zypper", "/sbin/zypper"},
	"apk":          {"/sbin/apk", "/usr/sbin/apk"},
	"xbps-install": {"/usr/bin/xbps-install", "/bin/xbps-install"},
	"xbps-remove":  {"/usr/bin/xbps-remove", "/bin/xbps-remove"},
}

// FindManager returns the absolute path of a manager's binary.
func FindManager(binary string) (string, error) {
	candidates, ok := managerPaths[binary]
	if !ok {
		return "", fmt.Errorf("%s is not a package manager this helper runs", binary)
	}
	return findExecutable(candidates, binary)
}
