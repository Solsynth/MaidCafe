package privfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// The helper's command surface. Every form is positional and exact, so the
// sudoers pattern is a fixed prefix plus a trailing wildcard and an appended
// argument is a refusal rather than a reinterpretation:
//
//	maidkit-priv [--config <path>] fs write  <profile> <rel> <mode>   # stdin
//	maidkit-priv [--config <path>] fs mkdir  <profile> <rel> <mode>
//	maidkit-priv [--config <path>] fs remove <profile> <rel>
//	maidkit-priv [--config <path>] fs profiles
//	maidkit-priv [--config <path>] systemd <verb> <unit>
//	maidkit-priv [--config <path>] packages <refresh|upgrade>
//	maidkit-priv [--config <path>] packages <install|remove> <name>
//	maidkit-priv [--config <path>] firewall <enable|disable>
//	maidkit-priv [--config <path>] firewall <allow|deny> <port> <protocol> <source>
//	maidkit-priv [--config <path>] firewall delete <action> <port> <protocol> <source>
//	maidkit-priv sudoers [user]
//	maidkit-priv version

// Action is one parsed invocation.
type Action struct {
	// Command is "fs", "sudoers" or "version".
	Command string
	// Verb is the "fs" subcommand: write, mkdir, remove or profiles. For the
	// systemd command it is the systemctl verb (start, restart, ...).
	Verb    string
	Profile string
	Rel     string
	Mode    string
	// Unit is the systemd unit name for the systemd command.
	Unit string
	// Package is the package name for `packages install|remove`.
	Package string
	// Port, Protocol and Source are the firewall rule for `firewall
	// allow|deny|delete`. Protocol and Source always carry a value ("any" when
	// the caller did not narrow them), so no argument is ever an empty string.
	Port     string
	Protocol string
	Source   string
	// RuleAction is the allow/deny a firewall `delete` has to repeat: ufw
	// identifies a rule by its full text, so deleting an allow and deleting a
	// deny are different operations.
	RuleAction string
	// ConfigPath is the profile file to read.
	ConfigPath string
	// User is the account the `sudoers` command prints a rule for.
	User string
}

// The resolvers the runners use. They are variables so a test can point them at
// a stand-in binary: the real ones resolve only from a fixed list of absolute
// paths, which a test cannot populate. Nothing else reassigns them, so a call
// through them is a call to the function below.
var (
	findManager         = FindManager
	findFirewallCommand = FindFirewallBinary
)

// runOptions is what a runner loads the grant file with. RequireRootOwner is
// the production value — the file that authorizes root operations must be
// root-owned — and the tests relax it, because a test cannot create a
// root-owned file. The check itself is covered where it belongs, in
// [LoadSet].
var runOptions = Options{RequireRootOwner: true}

// UsageError marks a malformed invocation, which maps to ExitUsage rather than
// a policy refusal.
type UsageError struct{ message string }

func (e *UsageError) Error() string { return e.message }

func usageErrorf(format string, args ...any) error {
	return &UsageError{message: fmt.Sprintf(format, args...)}
}

// ParseArgs validates the argument vector. It is strict on purpose: an
// unknown argument, a duplicated flag or a missing operand is a refusal, so a
// caller cannot smuggle an interpretation past the sudoers pattern.
func ParseArgs(argv []string) (Action, error) {
	action := Action{ConfigPath: DefaultConfigPath}
	if len(argv) == 0 {
		return action, usageErrorf("a command is required (fs, sudoers, version)")
	}
	// --config may only appear first, before the command. Allowing it later
	// would require a wildcard in the middle of the sudoers rule, which is
	// exactly the shape this helper exists to avoid.
	if argv[0] == "--config" {
		if len(argv) < 2 {
			return action, usageErrorf("--config requires a path")
		}
		if strings.TrimSpace(argv[1]) == "" {
			return action, usageErrorf("--config requires a path")
		}
		action.ConfigPath = argv[1]
		argv = argv[2:]
		if len(argv) == 0 {
			return action, usageErrorf("a command is required after --config")
		}
	}
	for _, arg := range argv {
		if strings.HasPrefix(arg, "-") {
			return action, usageErrorf("unknown option %q", arg)
		}
	}
	action.Command = argv[0]
	switch action.Command {
	case "version":
		if len(argv) != 1 {
			return action, usageErrorf("version takes no arguments")
		}
		return action, nil
	case "sudoers":
		if len(argv) > 2 {
			return action, usageErrorf("sudoers takes at most one account name")
		}
		action.User = "maidcafe"
		if len(argv) == 2 {
			if strings.TrimSpace(argv[1]) == "" {
				return action, usageErrorf("the account name must not be empty")
			}
			action.User = argv[1]
		}
		return action, nil
	case "fs", "systemd", "packages", "firewall":
	default:
		return action, usageErrorf("unknown command %q", argv[0])
	}

	if action.Command == "packages" {
		if len(argv) < 2 {
			return action, usageErrorf("packages requires an operation (%s)", PackageVerbs())
		}
		action.Verb = argv[1]
		switch action.Verb {
		case "refresh", "upgrade":
			if len(argv) != 2 {
				return action, usageErrorf("packages %s takes no package name", action.Verb)
			}
		case "install", "remove":
			if len(argv) != 3 {
				return action, usageErrorf("packages %s takes <name>", action.Verb)
			}
			action.Package = argv[2]
			if strings.TrimSpace(action.Package) == "" {
				return action, usageErrorf("the package name must not be empty")
			}
		default:
			return action, usageErrorf("unknown packages operation %q (one of: %s)", action.Verb, PackageVerbs())
		}
		return action, nil
	}

	if action.Command == "firewall" {
		if len(argv) < 2 {
			return action, usageErrorf("firewall requires an operation (%s)", FirewallVerbs())
		}
		action.Verb = argv[1]
		switch action.Verb {
		case "enable", "disable":
			if len(argv) != 2 {
				return action, usageErrorf("firewall %s takes no arguments", action.Verb)
			}
		case "allow", "deny":
			if len(argv) != 5 {
				return action, usageErrorf("firewall %s takes <port> <protocol> <source>", action.Verb)
			}
			action.Port, action.Protocol, action.Source = argv[2], argv[3], argv[4]
		case "delete":
			if len(argv) != 6 {
				return action, usageErrorf("firewall delete takes <action> <port> <protocol> <source>")
			}
			action.RuleAction = argv[2]
			action.Port, action.Protocol, action.Source = argv[3], argv[4], argv[5]
		default:
			return action, usageErrorf("unknown firewall operation %q (one of: %s)", action.Verb, FirewallVerbs())
		}
		return action, nil
	}

	if action.Command == "systemd" {
		if len(argv) != 3 {
			return action, usageErrorf("systemd takes <verb> <unit>")
		}
		action.Verb, action.Unit = argv[1], argv[2]
		if strings.TrimSpace(action.Unit) == "" {
			return action, usageErrorf("the unit name must not be empty")
		}
		if !validSystemdVerb(action.Verb) {
			return action, usageErrorf("verb %q is not one of: %s", action.Verb, SystemdVerbs())
		}
		return action, nil
	}
	if len(argv) < 2 {
		return action, usageErrorf("fs requires an operation (write, mkdir, remove, profiles)")
	}
	action.Verb = argv[1]
	switch action.Verb {
	case "profiles":
		if len(argv) != 2 {
			return action, usageErrorf("fs profiles takes no arguments")
		}
		return action, nil
	case "write", "mkdir":
		if len(argv) != 5 {
			return action, usageErrorf("fs %s takes <profile> <relative-path> <mode>", action.Verb)
		}
		action.Profile, action.Rel, action.Mode = argv[2], argv[3], argv[4]
	case "remove":
		if len(argv) != 4 {
			return action, usageErrorf("fs remove takes <profile> <relative-path>")
		}
		action.Profile, action.Rel = argv[2], argv[3]
	default:
		return action, usageErrorf("unknown fs operation %q", action.Verb)
	}
	if strings.TrimSpace(action.Profile) == "" {
		return action, usageErrorf("the profile name must not be empty")
	}
	if strings.TrimSpace(action.Rel) == "" {
		return action, usageErrorf("the relative path must not be empty")
	}
	return action, nil
}

// Env is what the helper knows about the process that ran it. sudo sets the
// invoker variables, so the audit line names the real caller rather than one
// the command line claims.
type Env struct {
	EUID       int
	Invoker    string
	InvokerUID string
}

// Run executes one invocation and returns the process exit code. The daemon
// reads the exit code, stdout and stderr; nothing else about the helper is
// visible to it.
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	action, err := ParseArgs(argv)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		fmt.Fprintf(stderr, "usage: maidkit-priv [--config <path>] fs write|mkdir|remove <profile> <relative-path> [mode]\n")
		fmt.Fprintf(stderr, "       maidkit-priv [--config <path>] systemd <verb> <unit>\n")
		fmt.Fprintf(stderr, "       maidkit-priv [--config <path>] packages <operation> [name]\n")
		fmt.Fprintf(stderr, "       maidkit-priv [--config <path>] firewall <operation> [...rule]\n")
		return ExitUsage
	}
	switch action.Command {
	case "version":
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "sudoers":
		fmt.Fprint(stdout, SudoersRule(action.User, DefaultHelperPath))
		return ExitOK
	}
	if action.Command == "systemd" {
		return runSystemd(action, stdout, stderr, env)
	}
	if action.Command == "packages" {
		return runPackages(action, stdout, stderr, env)
	}
	if action.Command == "firewall" {
		return runFirewall(action, stdout, stderr, env)
	}
	if action.Verb == "profiles" {
		return runProfiles(action, stdout, stderr, env)
	}
	if env.EUID != 0 {
		// The helper is reached through sudo with a NOPASSWD rule; running it
		// unprivileged means the rule is missing, which is an installation
		// problem rather than a request problem.
		fmt.Fprintln(stderr, "maidkit-priv: must run as root (install the sudoers rule printed by `maidkit-priv sudoers`)")
		return ExitNotRoot
	}
	set, err := LoadSet(action.ConfigPath, runOptions)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Profile: action.Profile, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitConfigBad
	}
	profile, err := set.Lookup(action.Profile)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Profile: action.Profile, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitRefused
	}

	switch action.Verb {
	case "write":
		mode, err := ParseMode(action.Mode)
		if err != nil {
			return refuse(stderr, env, action, err)
		}
		data, err := io.ReadAll(io.LimitReader(stdin, MaxContentBytes+1))
		if err != nil {
			fmt.Fprintf(stderr, "maidkit-priv: read standard input: %v\n", err)
			writeEntry(stderr, Entry{Verb: action.Verb, Profile: profile.Name, Path: action.Rel, OK: false, Error: err.Error(), EUID: env.EUID})
			return ExitIO
		}
		if len(data) > MaxContentBytes {
			return refuse(stderr, env, action, fmt.Errorf("content is over the %d-byte limit", MaxContentBytes))
		}
		if err := set.Write(profile, action.Rel, mode, data); err != nil {
			return fail(stderr, env, action, err, len(data))
		}
		writeEntry(stderr, Entry{
			Verb: "write", Profile: profile.Name, Path: action.Rel, Mode: action.Mode,
			Bytes: len(data), Digest: Digest(data), OK: true,
			EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
		})
		return ExitOK
	case "mkdir":
		mode, err := ParseMode(action.Mode)
		if err != nil {
			return refuse(stderr, env, action, err)
		}
		if err := set.Mkdir(profile, action.Rel, mode); err != nil {
			return fail(stderr, env, action, err, 0)
		}
		writeEntry(stderr, Entry{
			Verb: "mkdir", Profile: profile.Name, Path: action.Rel, Mode: action.Mode, OK: true,
			EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
		})
		return ExitOK
	case "remove":
		if err := set.Remove(profile, action.Rel); err != nil {
			return fail(stderr, env, action, err, 0)
		}
		writeEntry(stderr, Entry{
			Verb: "remove", Profile: profile.Name, Path: action.Rel, OK: true,
			EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
		})
		return ExitOK
	}
	fmt.Fprintf(stderr, "maidkit-priv: unknown fs operation %q\n", action.Verb)
	return ExitUsage
}

// runSystemd runs one granted unit action.
//
// The unit and the verb are both checked against the root-owned grant file, so
// a caller who appends arguments to the sudoers pattern reaches nothing: an
// ungranted unit is a refusal, and a granted one can only receive the verbs the
// operator listed. systemctl's own output is passed through, because an
// operator diagnosing a unit needs it, and its exit code is propagated so a
// failed action is not reported as a success.
func runSystemd(action Action, stdout, stderr io.Writer, env Env) int {
	if env.EUID != 0 {
		fmt.Fprintln(stderr, "maidkit-priv: must run as root (install the sudoers rule printed by `maidkit-priv sudoers`)")
		return ExitNotRoot
	}
	set, err := LoadSet(action.ConfigPath, runOptions)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Unit: action.Unit, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitConfigBad
	}
	grant, err := set.LookupUnit(action.Unit)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Unit: action.Unit, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitRefused
	}
	argv, err := grant.SystemdArgv(action.Verb)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Unit: action.Unit, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitRefused
	}
	return runExternal(
		[][]string{argv}, nil, stdout, stderr, env,
		Entry{Verb: action.Verb, Unit: action.Unit},
	)
}

// runPackages runs one granted package verb.
//
// The manager comes from the grant, so a caller cannot point a root invocation
// at a different package manager, and the package name is validated against the
// distributions' own grammar rather than being trusted because it arrived as
// one argv element.
func runPackages(action Action, stdout, stderr io.Writer, env Env) int {
	if env.EUID != 0 {
		fmt.Fprintln(stderr, "maidkit-priv: must run as root (install the sudoers rule printed by `maidkit-priv sudoers`)")
		return ExitNotRoot
	}
	set, err := LoadSet(action.ConfigPath, runOptions)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Package: action.Package, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitConfigBad
	}
	grant := set.Packages()
	if grant == nil {
		err := fmt.Errorf("no [packages] grant is configured in %s", action.ConfigPath)
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, Package: action.Package, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitRefused
	}
	argv, err := grant.PackageArgv(action.Verb, action.Package)
	entry := Entry{Verb: action.Verb, Package: action.Package, Manager: grant.Manager}
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		entry.OK, entry.Error, entry.EUID = false, err.Error(), env.EUID
		writeEntry(stderr, entry)
		return ExitRefused
	}
	path, err := findManager(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		entry.OK, entry.Error, entry.EUID = false, err.Error(), env.EUID
		writeEntry(stderr, entry)
		return ExitRefused
	}
	argv[0] = path
	return runExternal([][]string{argv}, PackageEnv(grant.Manager), stdout, stderr, env, entry)
}

// runFirewall runs one granted firewall verb.
func runFirewall(action Action, stdout, stderr io.Writer, env Env) int {
	if env.EUID != 0 {
		fmt.Fprintln(stderr, "maidkit-priv: must run as root (install the sudoers rule printed by `maidkit-priv sudoers`)")
		return ExitNotRoot
	}
	set, err := LoadSet(action.ConfigPath, runOptions)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitConfigBad
	}
	grant := set.Firewall()
	if grant == nil {
		err := fmt.Errorf("no [firewall] grant is configured in %s", action.ConfigPath)
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		writeEntry(stderr, Entry{Verb: action.Verb, OK: false, Error: err.Error(), EUID: env.EUID})
		return ExitRefused
	}
	entry := Entry{Verb: action.Verb, Backend: grant.Backend}
	var rule FirewallRule
	if action.Verb == "allow" || action.Verb == "deny" || action.Verb == "delete" {
		ruleAction := action.Verb
		if action.Verb == "delete" {
			ruleAction = action.RuleAction
		}
		parsed, err := ParseFirewallRule(ruleAction, action.Port, action.Protocol, action.Source)
		if err != nil {
			fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
			entry.OK, entry.Error, entry.EUID = false, err.Error(), env.EUID
			writeEntry(stderr, entry)
			return ExitRefused
		}
		rule = parsed
		entry.Rule = rule.String()
	}
	commands, err := grant.FirewallCommands(action.Verb, rule)
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		entry.OK, entry.Error, entry.EUID = false, err.Error(), env.EUID
		writeEntry(stderr, entry)
		return ExitRefused
	}
	// Each command is resolved from a fixed list before anything runs, so a
	// missing binary is a refusal rather than a half-applied rule.
	for _, argv := range commands {
		path, err := findFirewallCommand(argv[0])
		if err != nil {
			fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
			entry.OK, entry.Error, entry.EUID = false, err.Error(), env.EUID
			writeEntry(stderr, entry)
			return ExitRefused
		}
		argv[0] = path
	}
	return runExternal(commands, nil, stdout, stderr, env, entry)
}

// runExternal runs the commands one operation needs, in order, and turns their
// exit into the helper's own code.
//
// Standard input is /dev/null rather than the helper's own: these tools are
// interactive when they can be, and with no terminal the only thing a prompt
// can do is read whatever the caller wrote. A tool that insists on an answer
// sees end-of-file and fails, which is a loud failure instead of a command that
// waits forever for input nobody will send.
//
// A multi-command operation stops at the first failure: a firewalld reload
// after a failed permanent write would only obscure what went wrong.
func runExternal(commands [][]string, extraEnv []string, stdout, stderr io.Writer, env Env, entry Entry) int {
	for _, argv := range commands {
		command := exec.Command(argv[0], argv[1:]...)
		command.Stdin = nil
		if len(extraEnv) > 0 {
			command.Env = append(os.Environ(), extraEnv...)
		}
		command.Stdout = stdout
		command.Stderr = stderr
		if runErr := command.Run(); runErr != nil {
			entry.OK = false
			entry.Error = runErr.Error()
			entry.EUID = env.EUID
			entry.Invoker, entry.InvokerID = env.Invoker, env.InvokerUID
			writeEntry(stderr, entry)
			return externalExitCode(runErr)
		}
	}
	entry.OK = true
	entry.EUID = env.EUID
	entry.Invoker, entry.InvokerID = env.Invoker, env.InvokerUID
	writeEntry(stderr, entry)
	return ExitOK
}

// externalExitCode maps a failed command to an exit code, keeping the helper's
// own codes out of the way: an exit code that collides with one of them (3 for
// a stopped unit, for instance) would otherwise be reported as a policy
// refusal. The audit line records what actually happened and the command's own
// message is on stderr unprefixed.
func externalExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= ExitUsage && code <= ExitConfigBad {
			return ExitIO
		}
		return code
	}
	return ExitIO
}

// runProfiles lists the configured profiles, which is how an operator checks
// that a freshly installed profile file parses without granting anything.
func runProfiles(action Action, stdout, stderr io.Writer, env Env) int {
	set, err := LoadSet(action.ConfigPath, Options{RequireRootOwner: env.EUID == 0})
	if err != nil {
		fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
		return ExitConfigBad
	}
	for _, name := range set.Names() {
		profile, err := set.Lookup(name)
		if err != nil {
			continue
		}
		fmt.Fprintf(stdout, "profile\t%s\t%s\t%s\n", profile.Name, profile.Path, profile.modeList())
	}
	// Granted units are listed too: this command exists to answer "what will
	// this file let the helper do", and a file that grants systemd units while
	// printing only file profiles answers half the question.
	if grant := set.Packages(); grant != nil {
		fmt.Fprintf(stdout, "packages\t%s\tservice\t%s\n", grant.Manager, grant.verbList())
	}
	if grant := set.Firewall(); grant != nil {
		fmt.Fprintf(stdout, "firewall\t%s\tservice\t%s\n", grant.Backend, grant.verbList())
	}
	for _, unit := range set.GrantedUnits() {
		grant, err := set.LookupUnit(unit)
		if err != nil {
			continue
		}
		verbs := make([]string, 0, len(grant.Verbs))
		for verb := range grant.Verbs {
			verbs = append(verbs, verb)
		}
		sort.Strings(verbs)
		fmt.Fprintf(stdout, "systemd\t%s\t%s\n", grant.Unit, strings.Join(verbs, ","))
	}
	return ExitOK
}

func refuse(stderr io.Writer, env Env, action Action, err error) int {
	fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
	writeEntry(stderr, Entry{Verb: action.Verb, Profile: action.Profile, Path: action.Rel, Mode: action.Mode, OK: false, Error: err.Error(), EUID: env.EUID})
	return ExitRefused
}

func fail(stderr io.Writer, env Env, action Action, err error, bytes int) int {
	fmt.Fprintf(stderr, "maidkit-priv: %v\n", err)
	code := ExitIO
	if errors.Is(err, fs.ErrPermission) {
		code = ExitIO
	}
	writeEntry(stderr, Entry{
		Verb: action.Verb, Profile: action.Profile, Path: action.Rel, Mode: action.Mode,
		Bytes: bytes, OK: false, Error: err.Error(),
		EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
	})
	return code
}

// SudoersRule renders the drop-in the installer writes. The grant names the
// helper once per mutating verb; the profile file, not the pattern, decides
// which directories those verbs can reach, so the rule never needs to be
// regenerated when a profile is added.
func SudoersRule(user, helperPath string) string {
	var builder strings.Builder
	builder.WriteString("# MaidCafe privileged helper. Every command resolves what it may touch\n")
	builder.WriteString("# against " + DefaultConfigPath + ", which must stay root-owned and\n")
	builder.WriteString("# unwritable by anyone else: that file, not this rule, is the boundary.\n")
	for _, verb := range []string{"write", "mkdir", "remove"} {
		fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s fs %s *\n", user, helperPath, verb)
	}
	// One line for systemd: the unit and the verb are checked against the
	// grant file, so the pattern does not have to enumerate them — and cannot
	// be widened by a caller who appends arguments.
	fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s systemd *\n", user, helperPath)
	// Package and firewall verbs too. The manager, the backend, the verbs and
	// the arguments are all checked against the grant file, so the pattern does
	// not have to enumerate them and cannot be widened by appending arguments.
	fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s packages *\n", user, helperPath)
	fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s firewall *\n", user, helperPath)
	return builder.String()
}

// Version is the helper's build identifier, overridable at link time.
var Version = "dev"
