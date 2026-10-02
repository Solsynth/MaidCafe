package privfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// ConfigPath is the profile file to read.
	ConfigPath string
	// User is the account the `sudoers` command prints a rule for.
	User string
}

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
	case "fs", "systemd":
	default:
		return action, usageErrorf("unknown command %q", argv[0])
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
	set, err := LoadSet(action.ConfigPath, Options{RequireRootOwner: true})
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
	set, err := LoadSet(action.ConfigPath, Options{RequireRootOwner: true})
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
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if runErr == nil {
		writeEntry(stderr, Entry{
			Verb: action.Verb, Unit: action.Unit, OK: true,
			EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
		})
		return ExitOK
	}
	code := ExitIO
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code = exitErr.ExitCode()
		// Keep the helper's own codes out of the way: a systemctl exit code
		// that collides with one of them (3 for a stopped unit, for instance)
		// would otherwise be reported as a policy refusal. The audit line
		// records what actually happened and the daemon reads stderr, which
		// carries systemctl's own message unprefixed.
		if code >= ExitUsage && code <= ExitConfigBad {
			code = ExitIO
		}
	}
	writeEntry(stderr, Entry{
		Verb: action.Verb, Unit: action.Unit, OK: false, Error: runErr.Error(),
		EUID: env.EUID, Invoker: env.Invoker, InvokerID: env.InvokerUID,
	})
	return code
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
	builder.WriteString("# MaidCafe privileged file helper. The helper resolves a profile name against\n")
	builder.WriteString("# " + DefaultConfigPath + ", which must stay root-owned and unwritable by\n")
	builder.WriteString("# anyone else: that file, not this rule, is the authorization boundary.\n")
	for _, verb := range []string{"write", "mkdir", "remove"} {
		fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s fs %s *\n", user, helperPath, verb)
	}
	// One line for systemd: the unit and the verb are checked against the
	// grant file, so the pattern does not have to enumerate them — and cannot
	// be widened by a caller who appends arguments.
	fmt.Fprintf(&builder, "%s ALL=(root) NOPASSWD: %s systemd *\n", user, helperPath)
	return builder.String()
}

// Version is the helper's build identifier, overridable at link time.
var Version = "dev"
