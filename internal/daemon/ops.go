package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/privfs"
)

// Native host operations: typed, validated mutations the daemon executes
// directly — container lifecycle, process kill, systemd unit actions and
// compose project actions. They mirror the operations MaidKit performs over
// SSH, so a MaidCafe-managed host can be operated through the daemon: locally
// over HTTP, over the SSH stdio pipe, or remotely through the cloud relay —
// without a workstation SSH session.
//
// Safety: unlike script actions, native ops never interpolate caller input
// into a shell. Targets are validated against the same patterns MaidKit's SSH
// layer enforces, and commands run directly with exec.CommandContext (no
// shell wrapper), so a relayed request cannot inject commands. Root-owned
// resources are reached with a `sudo -n` retry mirroring the collectors;
// under the shipped systemd unit's NoNewPrivileges that retry is inert, so
// such ops fail with a clear error and MaidKit falls back to SSH.

// slowOpTimeout bounds the native operations that are slow by nature — compose
// pulls and recreates, and image pulls. The daemon-wide scriptTimeout stays
// the default for everything else.
const slowOpTimeout = 5 * time.Minute

// Native op validation patterns, kept identical to the MaidKit client so both
// channels accept exactly the same targets.
var (
	nativeContainerRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	// The first character must not be a dash: "-H.service" would reach
	// systemctl's option parser as the --host option rather than a unit name.
	nativeSystemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9:._@][A-Za-z0-9:._@\-]*\.service$`)
	nativeProjectPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	nativeDirectoryPattern   = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
)

// nativeContainerVerbs maps a container lifecycle action to the runtime CLI
// verb. remove uses `rm`, with -f when forced — exactly like the MaidKit SSH
// layer.
var nativeContainerVerbs = map[string]string{
	"start":   "start",
	"stop":    "stop",
	"restart": "restart",
	"pause":   "pause",
	"unpause": "unpause",
	"kill":    "kill",
	"remove":  "rm",
}

// nativeSystemdVerbs maps a systemd action to the systemctl verb.
var nativeSystemdVerbs = map[string]string{
	"start":   "start",
	"stop":    "stop",
	"restart": "restart",
	"reload":  "reload",
	"enable":  "enable",
	"disable": "disable",
}

// nativeComposeVerbs maps a compose project action to the CLI arguments
// (never built from caller input).
var nativeComposeVerbs = map[string]string{
	"up":       "up -d",
	"stop":     "stop",
	"restart":  "restart",
	"pull":     "pull",
	"recreate": "up -d --force-recreate",
}

// nativeOpDisplayNames maps every native slug to its human-readable label for
// the cloud action report and the audit log.
var nativeOpDisplayNames = map[string]string{
	"container.start":   "Start container",
	"container.stop":    "Stop container",
	"container.restart": "Restart container",
	"container.pause":   "Pause container",
	"container.unpause": "Unpause container",
	"container.kill":    "Kill container",
	"container.remove":  "Remove container",
	"container.pull":    "Pull container image",
	"container.update":  "Update container",
	"process.kill":      "Kill process",
	"systemd.start":     "Start systemd unit",
	"systemd.stop":      "Stop systemd unit",
	"systemd.restart":   "Restart systemd unit",
	"systemd.reload":    "Reload systemd unit",
	"systemd.enable":    "Enable systemd unit",
	"systemd.disable":   "Disable systemd unit",
	"package.refresh":   "Refresh package indexes",
	"package.upgrade":   "Upgrade packages",
	"package.install":   "Install package",
	"package.remove":    "Remove package",
	"firewall.enable":   "Enable firewall",
	"firewall.disable":  "Disable firewall",
	"firewall.allow":    "Allow firewall rule",
	"firewall.deny":     "Deny firewall rule",
	"firewall.delete":   "Delete firewall rule",
	"compose.up":        "Compose up",
	"compose.stop":      "Compose stop",
	"compose.restart":   "Compose restart",
	"compose.pull":      "Compose pull",
	"compose.recreate":  "Compose recreate",
	"compose.update":    "Update compose stack",
}

// isNativeOpSlug reports whether [name] is a built-in native operation. Used
// by the cloud relay to dispatch by name before the hook table.
func isNativeOpSlug(name string) bool {
	for _, slug := range config.NativeOpNames {
		if slug == name {
			return true
		}
	}
	return false
}

// nativeOpReport returns the built-in operations in the same shape as
// configured actions, so the cloud lists them as invocable for this daemon.
func nativeOpReport() []config.WebhookConfig {
	report := make([]config.WebhookConfig, 0, len(config.NativeOpNames))
	for _, slug := range config.NativeOpNames {
		report = append(report, config.WebhookConfig{
			Name:        slug,
			DisplayName: nativeOpDisplayNames[slug],
			Enabled:     true,
		})
	}
	return report
}

// opParams carries a native operation's validated targets. The slug encodes
// the operation family and verb; the params carry the identity (container id,
// unit name, compose project) and per-family options (force for container
// remove, working directory for compose).
type opParams struct {
	target    string
	pid       int
	force     bool
	directory string
	// name is the package for a package operation.
	name string
	// port, protocol and source are one firewall rule.
	port     string
	protocol string
	source   string
	// ruleAction is the allow/deny a firewall delete has to name. ufw
	// identifies a rule by its full text, so deleting an allow and deleting a
	// deny are different rules.
	ruleAction string
}

// nativeParamsFromValues builds opParams from a decoded JSON body (the cloud
// relay and stdio transports carry everything in the body; HTTP carries the
// identity in the path).
func nativeParamsFromValues(slug string, values map[string]any) opParams {
	var p opParams
	switch {
	case strings.HasPrefix(slug, "container."):
		p.target, _ = values["id"].(string)
		p.force, _ = values["force"].(bool)
	case slug == "process.kill":
		switch v := values["pid"].(type) {
		case float64:
			p.pid = int(v)
		case int:
			p.pid = v
		case string:
			p.pid, _ = strconv.Atoi(v)
		}
	case strings.HasPrefix(slug, "systemd."):
		p.target, _ = values["unit"].(string)
	case strings.HasPrefix(slug, "compose."):
		p.target, _ = values["project"].(string)
		p.directory, _ = values["directory"].(string)
	case strings.HasPrefix(slug, "package."):
		p.name, _ = values["name"].(string)
	case strings.HasPrefix(slug, "firewall."):
		p.port, _ = values["port"].(string)
		p.protocol, _ = values["protocol"].(string)
		p.source, _ = values["source"].(string)
		p.ruleAction, _ = values["rule_action"].(string)
	}
	return p
}

// opAttempt is one command the runner may try, in order.
type opAttempt struct {
	command string
	args    []string
	cwd     string
	// env is appended to the inherited environment. apt needs
	// DEBIAN_FRONTEND; without it a package with a conffile or debconf prompt
	// waits on a terminal that is not there, and the op hangs until it times
	// out with nothing to show for it.
	env []string
}

// opStage is one step of a native operation: the alternative attempts that
// accomplish it (the direct call, its `sudo -n` variant, the alternate
// runtime). Most operations are one stage; `container.update` is two — pull
// the image, then recreate the container on it — because the second step is
// only meaningful once the first has succeeded, which is a different relation
// from the alternatives within a stage.
type opStage struct {
	// label names the step in the terms its operation uses ("pull",
	// "recreate"). A task reports progress per stage, so the label is what an
	// operator watching one reads; a one-stage operation takes its label from
	// its slug's verb.
	label    string
	attempts []opAttempt
}

// nativeOpRunner executes native operations through the executor's
// concurrency slot, timeout, counters and audit log. Container runtimes are
// resolved with the shared runtime probe (podman first), falling back to the
// other runtime when the target is not found there.
// opPrivPolicy is how a native operation reaches root, if it may. It is read
// per request so a reload can switch it without a restart.
type opPrivPolicy struct {
	helper string
	runner *privRunner
	// systemd, packages and firewall route one family of operations through
	// the helper, and make the helper the only path for that family: see
	// [config.PrivConfig.Systemd].
	systemd  bool
	packages bool
	firewall bool
}

type nativeOpRunner struct {
	executor      *WebhookExecutor
	runtimes      func(ctx context.Context) map[string]string
	scriptTimeout atomic.Int64 // nanoseconds
	publisher     *atomic.Pointer[CloudPublisher]
	// priv is how an operation reaches root, when it may. Nil means the
	// pre-helper behavior (a direct call, then a blanket `sudo -n`).
	priv atomic.Pointer[opPrivPolicy]
	// composeStacks is the registry of stacks the operator assigned to this
	// daemon with a scan. A compose action that names no directory resolves
	// one here, and a container whose labels do not point at its project is
	// updated where that registry says it lives.
	composeStacks *composeStackStore
	// tasks is where the long operations run: the transports start one and
	// hand its id to the client instead of holding the request open.
	tasks *taskStore
}

// SetComposeStacks hands the runner the managed stack registry. Called at
// construction; the registry itself is long-lived and rewritten in place, so a
// reload does not replace it.
func (r *nativeOpRunner) SetComposeStacks(store *composeStackStore) {
	r.composeStacks = store
}

// composeStack returns the managed stack for [project], matched without regard
// to case.
func (r *nativeOpRunner) composeStack(project string) (composeStack, bool) {
	if r.composeStacks == nil {
		return composeStack{}, false
	}
	return r.composeStacks.Get(project)
}

// composeTargetDirectory fills in the project directory a container's labels
// did not record, from the stacks the operator assigned to this daemon.
//
// A container only reaches this when its labels name a project and a service
// but no usable directory. It is not guessed at: the daemon runs `compose up`
// where a scan found the project, or it refuses.
func (r *nativeOpRunner) composeTargetDirectory(target composeUpdateTarget) (composeUpdateTarget, error) {
	if target.Directory != "" {
		return target, nil
	}
	stack, ok := r.composeStack(target.Project)
	if !ok {
		return composeUpdateTarget{}, fmt.Errorf(
			"the container's compose labels record no usable working directory, and project %q is not a stack this daemon manages — run a compose scan to assign it",
			target.Project)
	}
	target.Directory = stack.Directory
	if len(target.Files) == 0 {
		target.Files = stack.Files
	}
	return target, nil
}

// composeDirectory is the directory a compose action runs in: the one the
// caller sent, or — when it sent none — the managed stack's own.
//
// A caller's directory keeps the stricter rule it always had, because that path
// comes from outside this host. A managed stack's directory was validated when
// the scan recorded it.
func (r *nativeOpRunner) composeDirectory(project, directory string) (string, []string, error) {
	if directory != "" {
		if !validNativeDirectory(directory) {
			return "", nil, fmt.Errorf("invalid compose directory")
		}
		return directory, nil, nil
	}
	stack, ok := r.composeStack(project)
	if !ok {
		return "", nil, fmt.Errorf(
			"project %q is not a stack this daemon manages; run a compose scan to assign it, or send the directory", project)
	}
	return stack.Directory, stack.Files, nil
}

// SetScriptTimeout updates the daemon-wide op timeout (hot reload).
func (r *nativeOpRunner) SetScriptTimeout(timeout time.Duration) {
	r.scriptTimeout.Store(int64(timeout))
}

// SetPrivilegedPolicy records how native operations may reach root. Called at
// construction and on every reload.
func (r *nativeOpRunner) SetPrivilegedPolicy(priv config.PrivConfig, runner *privRunner) {
	if runner == nil {
		r.priv.Store(nil)
		return
	}
	r.priv.Store(&opPrivPolicy{
		helper:   priv.Helper,
		runner:   runner,
		systemd:  priv.Systemd,
		packages: priv.Packages,
		firewall: priv.Firewall,
	})
}

// systemdHelperAttempt builds the attempt that runs a systemd action through the
// helper, or returns false when systemd actions are not routed there.
//
// The helper is the *only* path when it is configured for systemd: its grant
// file names the units and verbs, and falling back to a blanket `sudo -n
// systemctl` would silently keep whatever broader grant an operator had, which
// is the boundary this replaces.
func (r *nativeOpRunner) systemdHelperAttempt(verb, unit string) (opAttempt, bool) {
	policy := r.priv.Load()
	if policy == nil || !policy.systemd || policy.runner == nil {
		return opAttempt{}, false
	}
	argv := policy.runner.argv(policy.helper, "systemd", verb, unit)
	if len(argv) == 0 {
		return opAttempt{}, false
	}
	return opAttempt{command: argv[0], args: argv[1:]}, true
}

// packageHelperAttempt builds the attempt that runs a package operation
// through the helper, or returns false when package operations are not routed
// there. The manager is the helper's to choose: the daemon passes a verb and a
// name and nothing else, so a caller cannot point a root invocation at a
// different package manager.
func (r *nativeOpRunner) packageHelperAttempt(verb, name string) (opAttempt, bool) {
	policy := r.priv.Load()
	if policy == nil || !policy.packages || policy.runner == nil {
		return opAttempt{}, false
	}
	args := []string{"packages", verb}
	if name != "" {
		args = append(args, name)
	}
	argv := policy.runner.argv(policy.helper, args...)
	if len(argv) == 0 {
		return opAttempt{}, false
	}
	return opAttempt{command: argv[0], args: argv[1:]}, true
}

// firewallHelperAttempt builds the attempt that runs a firewall operation
// through the helper, on the same terms as the package one: the backend and
// the rule grammar are the helper's to decide.
func (r *nativeOpRunner) firewallHelperAttempt(verb string, params opParams) (opAttempt, bool) {
	policy := r.priv.Load()
	if policy == nil || !policy.firewall || policy.runner == nil {
		return opAttempt{}, false
	}
	args := []string{"firewall", verb}
	if verb == "allow" || verb == "deny" || verb == "delete" {
		if verb == "delete" {
			args = append(args, params.ruleAction)
		}
		args = append(args, params.port, normalizeProtocol(params.protocol), normalizeSource(params.source))
	}
	argv := policy.runner.argv(policy.helper, args...)
	if len(argv) == 0 {
		return opAttempt{}, false
	}
	return opAttempt{command: argv[0], args: argv[1:]}, true
}

// detectPackageManager returns the first manager installed on this host, in
// the preference order the managers themselves are ranked in.
//
// Only the first is taken. A second candidate would mean retrying a failed
// install with a different manager, and the same name is a different package
// on a different distribution — so the retry could install something nobody
// asked for.
func detectPackageManager() (string, bool) {
	for _, manager := range privfs.PackageManagerPreference() {
		argv, err := privfs.PackageCommand(manager, "refresh", "")
		if err != nil {
			continue
		}
		if _, err := exec.LookPath(argv[0]); err == nil {
			return manager, true
		}
	}
	return "", false
}

// detectFirewallBackend returns the first backend installed on this host.
func detectFirewallBackend() (string, bool) {
	for _, backend := range privfs.FirewallBackendPreference() {
		if _, err := exec.LookPath(privfs.FirewallBinaryFor(backend)); err == nil {
			return backend, true
		}
	}
	return "", false
}

// nativeOpPlan is a validated native operation, ready to run: the stages it
// executes in order, the label it targets, and the deadline it runs under.
// Planning is what needs a runtime probe and what can fail with something
// useful to say — including the container's own configuration — so it stays in
// the caller's request even when the execution itself is detached into a task.
type nativeOpPlan struct {
	stages  []opStage
	target  string
	timeout time.Duration
}

// planNativeOp validates [slug] against [params] and resolves everything the
// run needs. It executes nothing.
func (r *nativeOpRunner) planNativeOp(
	ctx context.Context,
	slug string,
	params opParams,
) (nativeOpPlan, *requestError) {
	var attempts []opAttempt
	// stages carries an operation's steps. Single-stage operations fill
	// [attempts] and are wrapped below; a multi-stage operation (an update:
	// pull, then recreate) appends its stages here and leaves attempts empty.
	var stages []opStage
	targetLabel := ""
	timeout := time.Duration(r.scriptTimeout.Load())
	bad := func(message string) (nativeOpPlan, *requestError) {
		return nativeOpPlan{}, &requestError{status: http.StatusBadRequest, message: message}
	}
	failed := func(err error) (nativeOpPlan, *requestError) {
		return nativeOpPlan{}, &requestError{status: http.StatusBadGateway, message: err.Error()}
	}
	switch {
	case slug == "container.pull" || slug == "container.update":
		// Both need the container's own configuration, so they resolve it
		// first — which runtime holds it, what image it was created from, and
		// its compose identity — and are refused when that answer is missing
		// rather than attempted blindly.
		if !nativeContainerRefPattern.MatchString(params.target) {
			return bad("invalid container reference")
		}
		targetLabel = params.target
		if timeout < slowOpTimeout {
			timeout = slowOpTimeout
		}
		resolved, err := resolveOpContainer(ctx, r.runtimes(ctx), params.target)
		if err != nil {
			return failed(err)
		}
		// The reference is validated but not rewritten: `podman pull <ref>` has
		// to resolve a short name exactly as the container's own creation did,
		// which is the host's registry configuration's decision, not the
		// daemon's. (The update *check* uses the registry the image was actually
		// pulled from, which its digest records.)
		if _, err := parseImageReference(resolved.ImageRef); err != nil {
			return bad("container image reference is unusable: " + err.Error())
		}
		if slug == "container.pull" {
			// A pull is about the image, not the container it was read from.
			targetLabel = resolved.ImageRef
			store := composeStore{Runtime: resolved.Runtime, Path: resolved.Path, Elevated: resolved.Elevated}
			pullAttempts, err := r.runtimePullAttempts(ctx, store, resolved.ImageRef)
			if err != nil {
				return bad(err.Error())
			}
			attempts = pullAttempts
			break
		}
		compose, err := composeUpdateTargetFromLabels(resolved.Labels)
		if err == nil {
			compose, err = r.composeTargetDirectory(compose)
		}
		if err != nil {
			// Docker cannot recreate a plain `docker run` container from its own
			// configuration, and replaying inspect into a `run` argv would
			// silently drop whatever the daemon does not model — a container
			// that comes back missing a device, a sysctl or a network alias is
			// worse than one that was not touched. A container whose lifecycle
			// is declared in compose is recreated by compose, which is the
			// runtime's own answer to this and needs no reconstruction.
			return bad(fmt.Sprintf(
				"%s (runtime %s); the daemon recreates only compose-managed containers whose project it can find — pull the image with container.pull and recreate this one on the host",
				err.Error(), resolved.Runtime))
		}
		// The step runs in the store that holds the project, which is not
		// always the store the named container was read from: a container can
		// carry compose labels with its project's run elsewhere, and the
		// project is what `up -d --force-recreate` recreates.
		store, err := r.composeProjectStore(ctx, compose.Project, &composeStore{
			Runtime: resolved.Runtime, Path: resolved.Path, Elevated: resolved.Elevated,
		})
		if err != nil {
			return bad(err.Error())
		}
		pullAttempts, err := r.composeAttempts(ctx, store, compose, "pull", compose.Service)
		if err != nil {
			return bad(err.Error())
		}
		recreateAttempts, err := r.composeAttempts(ctx, store, compose, "up", "-d", "--force-recreate", compose.Service)
		if err != nil {
			return bad(err.Error())
		}
		stages = append(stages,
			opStage{label: "pull", attempts: pullAttempts},
			opStage{label: "recreate", attempts: recreateAttempts},
		)
	case strings.HasPrefix(slug, "container."):
		verb := strings.TrimPrefix(slug, "container.")
		cliVerb, known := nativeContainerVerbs[verb]
		if !known {
			return bad("unknown container action")
		}
		if !nativeContainerRefPattern.MatchString(params.target) {
			return bad("invalid container reference")
		}
		targetLabel = params.target
		args := []string{cliVerb}
		if verb == "remove" && params.force {
			args = append(args, "-f")
		}
		args = append(args, params.target)
		for _, runtime := range []string{"podman", "docker"} {
			path, ok := r.runtimes(ctx)[runtime]
			if !ok {
				continue
			}
			attempts = append(attempts, opAttempt{command: path, args: args})
			if elevated, elevatedOK := elevationAttempt(path, args...); elevatedOK {
				attempts = append(attempts, opAttempt{command: elevated[0], args: elevated[1:]})
			}
		}
	case slug == "process.kill":
		if params.pid <= 1 {
			return bad("invalid process id")
		}
		targetLabel = strconv.Itoa(params.pid)
		args := []string{"-s", "KILL", "--", strconv.Itoa(params.pid)}
		attempts = append(attempts, opAttempt{command: "kill", args: args})
		if sudo := r.sudoAttempt(); sudo != nil {
			attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{"kill"}, args...)...)})
		}
	case strings.HasPrefix(slug, "systemd."):
		verb := strings.TrimPrefix(slug, "systemd.")
		if _, known := nativeSystemdVerbs[verb]; !known {
			return bad("unknown systemd action")
		}
		unit := strings.TrimSpace(params.target)
		if unit != "" && !strings.Contains(unit, ".") {
			unit += ".service"
		}
		if !nativeSystemdUnitPattern.MatchString(unit) {
			return bad("invalid systemd unit")
		}
		targetLabel = unit
		args := []string{verb, unit}
		if helperAttempt, ok := r.systemdHelperAttempt(verb, unit); ok {
			// Routed: the helper decides, and nothing else is tried.
			attempts = append(attempts, helperAttempt)
			break
		}
		attempts = append(attempts, opAttempt{command: "systemctl", args: args})
		if sudo := r.sudoAttempt(); sudo != nil {
			attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{"systemctl"}, args...)...)})
		}
	case strings.HasPrefix(slug, "package."):
		verb := strings.TrimPrefix(slug, "package.")
		if !privfs.ValidPackageVerb(verb) {
			return bad("unknown package action")
		}
		name := strings.TrimSpace(params.name)
		switch verb {
		case "install", "remove":
			if !privfs.ValidPackageName(name) {
				return bad("invalid package name")
			}
		default:
			if name != "" {
				return bad("this package action takes no package name")
			}
		}
		targetLabel = name
		if targetLabel == "" {
			targetLabel = verb
		}
		if helperAttempt, ok := r.packageHelperAttempt(verb, name); ok {
			attempts = append(attempts, helperAttempt)
			break
		}
		manager, ok := detectPackageManager()
		if !ok {
			return bad("no package manager is installed on this host")
		}
		argv, err := privfs.PackageCommand(manager, verb, name)
		if err != nil {
			return bad("invalid package action")
		}
		env := privfs.PackageEnv(manager)
		if path, err := exec.LookPath(argv[0]); err == nil {
			attempts = append(attempts, opAttempt{command: path, args: argv[1:], env: env})
			if sudo := r.sudoAttempt(); sudo != nil {
				attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{path}, argv[1:]...)...), env: env})
			}
		}
	case strings.HasPrefix(slug, "firewall."):
		verb := strings.TrimPrefix(slug, "firewall.")
		if !privfs.ValidFirewallVerb(verb) {
			return bad("unknown firewall action")
		}
		var rule privfs.FirewallRule
		if verb == "allow" || verb == "deny" || verb == "delete" {
			action := verb
			if verb == "delete" {
				action = strings.TrimSpace(params.ruleAction)
			}
			parsed, err := privfs.ParseFirewallRule(action, params.port, params.protocol, params.source)
			if err != nil {
				return bad(err.Error())
			}
			rule = parsed
			targetLabel = rule.String()
		} else {
			targetLabel = verb
		}
		if helperAttempt, ok := r.firewallHelperAttempt(verb, params); ok {
			attempts = append(attempts, helperAttempt)
			break
		}
		backend, ok := detectFirewallBackend()
		if !ok {
			return bad("no supported firewall is installed on this host")
		}
		commands, err := privfs.FirewallCommand(backend, verb, rule)
		if err != nil {
			return bad(err.Error())
		}
		for _, argv := range commands {
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return bad(fmt.Sprintf("%s is not installed on this host", argv[0]))
			}
			attempts = append(attempts, opAttempt{command: path, args: argv[1:]})
			if sudo := r.sudoAttempt(); sudo != nil {
				attempts = append(attempts, opAttempt{command: sudo[0], args: append(sudo[1:], append([]string{path}, argv[1:]...)...)})
			}
		}
	case strings.HasPrefix(slug, "compose."):
		verb := strings.TrimPrefix(slug, "compose.")
		if !nativeProjectPattern.MatchString(params.target) {
			return bad("invalid compose project")
		}
		targetLabel = params.target
		if timeout < slowOpTimeout {
			timeout = slowOpTimeout
		}
		directory, files, err := r.composeDirectory(params.target, params.directory)
		if err != nil {
			return bad(err.Error())
		}
		compose := composeUpdateTarget{
			Project: params.target, Directory: directory, Files: files,
		}
		store, err := r.composeProjectStore(ctx, params.target, nil)
		if err != nil {
			return bad(err.Error())
		}
		if verb == "update" {
			// An upgrade is the same two steps a container update runs, for
			// every service the project declares: fetch the images, then
			// recreate on them.
			pullAttempts, err := r.composeAttempts(ctx, store, compose, "pull")
			if err != nil {
				return bad(err.Error())
			}
			recreateAttempts, err := r.composeAttempts(ctx, store, compose, "up", "-d", "--force-recreate")
			if err != nil {
				return bad(err.Error())
			}
			stages = append(stages,
				opStage{label: "pull", attempts: pullAttempts},
				opStage{label: "recreate", attempts: recreateAttempts},
			)
			break
		}
		cliArgs, known := nativeComposeVerbs[verb]
		if !known {
			return bad("unknown compose action")
		}
		verbAttempts, err := r.composeAttempts(ctx, store, compose, strings.Fields(cliArgs)...)
		if err != nil {
			return bad(err.Error())
		}
		attempts = append(attempts, verbAttempts...)
	default:
		return bad("unknown operation")
	}
	if len(stages) == 0 {
		if len(attempts) == 0 {
			return nativeOpPlan{}, &requestError{status: http.StatusBadGateway, message: "no container runtime available"}
		}
		stages = []opStage{{label: nativeOpVerb(slug), attempts: attempts}}
	}
	return nativeOpPlan{stages: stages, target: targetLabel, timeout: timeout}, nil
}

// nativeOpVerb is the verb part of a native slug: "pull" for container.pull.
// It is the label of a one-stage operation, and nothing else depends on it.
func nativeOpVerb(slug string) string {
	if _, verb, ok := strings.Cut(slug, "."); ok {
		return verb
	}
	return slug
}

// dispatch runs a native operation to completion inside the caller's request.
// The transports that can hold a request open — stdio over SSH, a scheduled
// job — use it; the ones whose client will not wait use [dispatchTask].
// Request-level failures return a requestError; execution outcomes return an
// executionResponse with the usual status codes (502 for non-zero exit, 504
// for timeout).
func (r *nativeOpRunner) dispatch(
	ctx context.Context,
	slug string,
	params opParams,
	source string,
	invokedBy string,
) (executionResponse, int, *requestError) {
	plan, requestErr := r.planNativeOp(ctx, slug, params)
	if requestErr != nil {
		return executionResponse{}, 0, requestErr
	}
	if !r.acquireSlot() {
		return executionResponse{Name: slug}, http.StatusTooManyRequests, nil
	}
	defer r.releaseSlot()
	runCtx, cancel := r.runContext(ctx, plan.timeout)
	defer cancel()
	response, status := r.runStages(runCtx, slug, nativeOpDisplayNames[slug], plan.target, plan.stages, source, invokedBy, nil)
	return response, status, nil
}

// taskNativeOp reports whether [slug] starts a task instead of holding the
// request it arrived on. It is exactly the set of operations that pull an image
// and the compose recreates that follow a pull: those are the ones measured in
// minutes, and no client's read timeout outlasts them.
func taskNativeOp(slug string) bool {
	switch slug {
	case "container.pull", "container.update", "compose.pull", "compose.update", "compose.recreate":
		return true
	}
	return false
}

// dispatchTask starts [slug] as a task and returns it before the work is done.
// The run has its own deadline and is detached from [ctx]: a caller that goes
// away — a browser tab, an HTTP client's read timeout, a phone that slept — no
// longer aborts an operation halfway through, which for a stack update is the
// difference between a pulled image and a half-recreated stack. What stops it
// instead is the operator (the cancel route) or the daemon shutting down.
func (r *nativeOpRunner) dispatchTask(
	ctx context.Context,
	slug string,
	params opParams,
	source string,
	invokedBy string,
) (*opTask, *requestError) {
	plan, requestErr := r.planNativeOp(ctx, slug, params)
	if requestErr != nil {
		return nil, requestErr
	}
	if !r.acquireSlot() {
		return nil, &requestError{
			status:  http.StatusTooManyRequests,
			message: "another native operation is already running; try again when it finishes",
		}
	}
	runCtx, cancel := r.runContext(context.WithoutCancel(ctx), plan.timeout)
	task := newOpTask(
		newTaskID(), slug, nativeOpDisplayNames[slug], plan.target,
		source, invokedBy, plan.stages, cancel,
	)
	r.tasks.add(task)
	go func() {
		defer r.releaseSlot()
		defer cancel()
		response, _ := r.runStages(
			runCtx, slug, task.displayName, task.target,
			plan.stages, source, invokedBy, task,
		)
		task.finish(response, runCtx.Err(), plan.timeout)
	}()
	return task, nil
}

// resolveOpContainer finds [target] on one of the probed runtimes (podman
// first) and reads its inspect payload. A container that no runtime has is
// reported as such: pulling the wrong image, or recreating the wrong
// container, is worse than a clear error.
func resolveOpContainer(ctx context.Context, runtimes map[string]string, target string) (opContainerResolution, error) {
	var lastErr error
	for _, runtime := range []string{"podman", "docker"} {
		path, ok := runtimes[runtime]
		if !ok {
			continue
		}
		info, err := inspectContainer(ctx, path, target)
		if err != nil {
			lastErr = err
			continue
		}
		if info.ImageRef == "" {
			// A container created from an image ID carries no reference, so
			// there is nothing to pull and nothing to compare against.
			return opContainerResolution{}, fmt.Errorf("container %s records no image reference", target)
		}
		return opContainerResolution{
			Runtime: runtime, Path: path, ImageRef: info.ImageRef, Labels: info.Labels,
			Elevated: info.Elevated,
		}, nil
	}
	if lastErr != nil {
		return opContainerResolution{}, fmt.Errorf("resolve container %s: %w", target, lastErr)
	}
	return opContainerResolution{}, fmt.Errorf("no container runtime available")
}

// opContainerResolution is what a pull or an update needs to know about the
// container it was asked for.
type opContainerResolution struct {
	Runtime  string
	Path     string
	ImageRef string
	Labels   map[string]string
	// Elevated records that root's store is where this container was found, so
	// a write built from it addresses the same store instead of creating a
	// second container in the daemon user's own.
	Elevated bool
}

// composeUpdateTarget is the compose identity a container's labels declare.
type composeUpdateTarget struct {
	Project string
	Service string
	// Directory is the project directory the labels record, when they record a
	// usable one. Empty means discovery has to find the project.
	Directory string
	// Files are the compose files the labels record, exactly as recorded:
	// absolute, or relative to [Directory]. They are resolved when the argv is
	// built, because a project found by discovery supplies the base a relative
	// entry needs.
	Files []string
}

// composeUpdateTargetFromLabels reads the compose identity out of a
// container's labels. docker compose records the project, service, working
// directory and config files; podman compose and the standalone podman-compose
// record the same facts, and record the config files as the paths the operator
// typed (`-f docker-compose.yml` stays `docker-compose.yml`).
//
// Everything is validated before it reaches an argv: these values come from the
// host's own container configuration, and a `compose up` in a directory of the
// labels' choosing is exactly the interpolation this package refuses to do.
func composeUpdateTargetFromLabels(labels map[string]string) (composeUpdateTarget, error) {
	target := composeUpdateTarget{
		Project: composeLabel(labels, "project"),
		Service: composeLabel(labels, "service"),
	}
	if target.Project == "" || target.Service == "" {
		return composeUpdateTarget{}, fmt.Errorf("the container is not compose-managed")
	}
	if !nativeProjectPattern.MatchString(target.Project) || !nativeProjectPattern.MatchString(target.Service) {
		return composeUpdateTarget{}, fmt.Errorf("the container's compose labels name an invalid project or service")
	}
	if directory := composeLabel(labels, "project.working_dir"); validComposeLabelDirectory(directory) {
		target.Directory = directory
	}
	for _, file := range strings.Split(composeLabel(labels, "project.config_files"), ",") {
		if file = strings.TrimSpace(file); file != "" && validComposeLabelFile(file) {
			target.Files = append(target.Files, file)
		}
	}
	return target, nil
}

// validComposeLabelDirectory reports whether a working directory recorded in a
// container's labels is one this package will run a compose command in: an
// absolute path with no parent segment.
//
// It is deliberately looser than [validNativeDirectory], which also restricts
// the character set. That guard exists for a directory a *caller* names, where
// refusing anything unusual costs nothing; these paths are the host's own
// recorded configuration, they reach an argv and never a shell, and refusing
// the update of every project that lives under `/home/First Last/stack` would
// gain nothing.
func validComposeLabelDirectory(value string) bool {
	return value != "" && filepath.IsAbs(value) && validComposeLabelPath(value)
}

// validComposeLabelFile reports whether one recorded compose file is usable: an
// absolute path, or a path relative to the project directory.
func validComposeLabelFile(value string) bool {
	return value != "" && validComposeLabelPath(value)
}

// validComposeLabelPath rejects what must not reach an argv: a NUL, a newline,
// or a parent segment that would let the recorded path leave the directory the
// labels named.
func validComposeLabelPath(value string) bool {
	if strings.ContainsAny(value, "\x00\n\r") {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(value), "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// resolveComposeFile turns one recorded compose file into the absolute path
// compose is invoked with. A relative entry resolves against the project
// directory — it is still the file the container was created from, only named
// the way the operator named it on the command line.
func resolveComposeFile(directory, file string) (string, bool) {
	if filepath.IsAbs(file) {
		return file, true
	}
	if directory == "" {
		return "", false
	}
	return filepath.Join(directory, file), true
}

// composeLabel reads one compose fact, accepting either runtime's prefix.
func composeLabel(labels map[string]string, key string) string {
	for _, prefix := range []string{"com.docker.compose.", "io.podman.compose."} {
		if value := strings.TrimSpace(labels[prefix+key]); value != "" {
			return value
		}
	}
	return ""
}

// composeStore is the store a compose project's containers live in: the runtime
// that holds them, and whether reaching that store takes root.
//
// One runtime binary reaches two stores. `podman` run by the daemon user keeps
// its containers in that user's own store; the same binary through `sudo -n`
// keeps root's. A compose project belongs to exactly one of them, and a step
// run in the other store is not "the same operation with less privilege": it
// creates a second copy of the project — same project name, same container
// names — in a store nobody was looking at, where it then fights the original
// for its published ports. That is why [nativeOpRunner.composeAttempts] builds a
// step for one store and never offers the other as a fallback.
type composeStore struct {
	Runtime  string
	Path     string
	Elevated bool
}

// describe names a store the way an operator describes it: by whose containers
// it holds.
func (s composeStore) describe() string {
	if s.Elevated {
		return s.Runtime + " in root's store"
	}
	return s.Runtime + " in the daemon user's own store"
}

// runStoreCommand runs a runtime command in exactly [store], never falling back
// to the other one, which is the point of naming it.
func runStoreCommand(ctx context.Context, store composeStore, args ...string) ([]byte, error) {
	if !store.Elevated {
		return runCommand(ctx, store.Path, args...)
	}
	elevated, ok := elevationAttempt(store.Path, args...)
	if !ok {
		return nil, fmt.Errorf("root's store is out of reach for this daemon: `%s` is not installed", "sudo")
	}
	return runCommand(ctx, elevated[0], elevated[1:]...)
}

// sudoRuns reports whether `sudo -n` may run [command] on this host.
//
// It is what separates "this daemon can reach root's store" from "this daemon
// would only print a password prompt": a sudoers rule that grants the runtime
// binary does not grant the standalone compose tool, and a step that needs the
// latter has no business being attempted. `sudo -n -l <command>` answers the
// question without running anything.
func sudoRuns(ctx context.Context, command string) bool {
	prefix := elevationPrefix()
	if prefix == nil {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, collectorExecTimeout)
	defer cancel()
	return exec.CommandContext(probe, prefix[0], "-n", "-l", command).Run() == nil
}

// composeStores lists the stores of every available runtime, the daemon user's
// own next to root's, in probe order.
func composeStores(runtimes map[string]string) []composeStore {
	stores := make([]composeStore, 0, 4)
	for _, runtime := range []string{"podman", "docker"} {
		path, ok := runtimes[runtime]
		if !ok {
			continue
		}
		stores = append(stores, composeStore{Runtime: runtime, Path: path})
		if _, ok := elevationAttempt(path); ok {
			stores = append(stores, composeStore{Runtime: runtime, Path: path, Elevated: true})
		}
	}
	return stores
}

// storeProjectContainers lists the containers of [project] that live in one
// store. The whole listing is read and matched here rather than filtered by the
// runtime, because the two compose tools name the project under different label
// keys — podman-compose writes both, docker writes its own — so a filter would
// have to guess which one a given container carries.
func storeProjectContainers(ctx context.Context, store composeStore, project string) ([]containerEntry, error) {
	out, err := runStoreCommand(ctx, store, "ps", "-a", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	entries, err := parseContainerLines(out)
	if err != nil {
		return nil, err
	}
	matches := make([]containerEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.EqualFold(entry.ComposeProject, project) {
			matches = append(matches, entry)
		}
	}
	return matches, nil
}

// composeStorePresence is one store that holds containers of a project, with
// what it holds, for reporting the stores a project was found in.
type composeStorePresence struct {
	store      composeStore
	containers []containerEntry
}

// summarizeContainers names what a store holds, capped so a project with many
// services still reads as one line.
func summarizeContainers(containers []containerEntry) string {
	parts := make([]string, 0, len(containers))
	for index, entry := range containers {
		if index == 4 {
			parts = append(parts, fmt.Sprintf("and %d more", len(containers)-index))
			break
		}
		name := entry.Name
		if name == "" {
			name = entry.ID
			if len(name) > 12 {
				name = name[:12]
			}
		}
		state := entry.State
		if state == "" {
			state = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, state))
	}
	return strings.Join(parts, ", ")
}

// composeProjectStores finds every store that holds containers of [project].
//
// A store that does not answer is skipped, with one exception: when `sudo -n`
// may run that runtime — the grant the shipped sudoers rule sets up — an
// unreadable store is reported as a failure rather than as an absence. The
// daemon cannot then tell whether the project is there, and acting on that
// guess is how a project ends up with a second copy.
func (r *nativeOpRunner) composeProjectStores(ctx context.Context, project string) ([]composeStorePresence, error) {
	found := make([]composeStorePresence, 0, 2)
	for _, store := range composeStores(r.runtimes(ctx)) {
		containers, err := storeProjectContainers(ctx, store, project)
		if err != nil {
			if store.Elevated && sudoRuns(ctx, store.Path) {
				return nil, fmt.Errorf(
					"cannot read %s (%v), and a project that might be there must not be written a second time",
					store.describe(), err)
			}
			continue
		}
		if len(containers) > 0 {
			found = append(found, composeStorePresence{store: store, containers: containers})
		}
	}
	return found, nil
}

// composeProjectStore decides which store a step for [project] belongs in.
//
// One store holds the project: that store. More than one: an error naming them,
// because choosing silently is how a second copy of a stack appears — the
// operator removes the copy they do not want, and the message says which ones
// exist. Nowhere yet: [fallback] when the caller has one (the store the named
// container lives in), else the daemon's own store of the first available
// runtime, which is where a project this daemon creates belongs.
func (r *nativeOpRunner) composeProjectStore(ctx context.Context, project string, fallback *composeStore) (composeStore, error) {
	found, err := r.composeProjectStores(ctx, project)
	if err != nil {
		return composeStore{}, err
	}
	switch {
	case len(found) == 1:
		return found[0].store, nil
	case len(found) > 1:
		described := make([]string, 0, len(found))
		for _, presence := range found {
			described = append(described, presence.store.describe()+": "+summarizeContainers(presence.containers))
		}
		return composeStore{}, fmt.Errorf(
			"project %q exists in more than one store (%s); this daemon will not choose between them — remove the copy you do not want, then try again",
			project, strings.Join(described, "; "))
	}
	if fallback != nil {
		return *fallback, nil
	}
	for _, runtime := range []string{"podman", "docker"} {
		if path, ok := r.runtimes(ctx)[runtime]; ok {
			return composeStore{Runtime: runtime, Path: path}, nil
		}
	}
	return composeStore{}, fmt.Errorf("no container runtime available")
}

// composeAttempts builds the compose step for [target] in [store]: every tool
// that can run it, in the order that runtime prefers (see composeTools), each in
// the form that reaches that store.
//
// It returns an error instead of a shorter ladder when no tool can reach the
// store. A step that belongs in root's store has no unprivileged form worth
// running — the unprivileged form is the daemon user's own store, which is a
// different project on the same host — so the answer is a refusal that names
// the grant which would let the daemon run it, not a step that quietly creates
// the project a second time.
//
// The compose file list comes from the container's own labels when they record
// it, so the step reads the same file the container was created from even when
// the project directory holds several. A project whose files the labels do not
// name is run from its own directory, where compose reads the default file.
func (r *nativeOpRunner) composeAttempts(ctx context.Context, store composeStore, target composeUpdateTarget, args ...string) ([]opAttempt, error) {
	files := make([]string, 0, len(target.Files))
	for _, file := range target.Files {
		if resolved, ok := resolveComposeFile(target.Directory, file); ok {
			files = append(files, resolved)
		}
	}
	tools := composeTools(store.Path)
	if len(tools) == 0 {
		return nil, fmt.Errorf("no compose tool is installed for %s", store.Runtime)
	}
	attempts := make([]opAttempt, 0, len(tools))
	denied := make([]string, 0, len(tools))
	for _, tool := range tools {
		inner := append([]string{}, tool.prefix...)
		inner = append(inner, "-p", target.Project)
		for _, file := range files {
			inner = append(inner, "-f", file)
		}
		inner = append(inner, args...)
		if !store.Elevated {
			attempts = append(attempts, opAttempt{
				command: tool.command, args: inner, cwd: target.Directory,
			})
			continue
		}
		if !sudoRuns(ctx, tool.command) {
			denied = append(denied, tool.command)
			continue
		}
		attempts = append(attempts, opAttempt{
			command: "sudo", args: append([]string{"-n", tool.command}, inner...), cwd: target.Directory,
		})
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf(
			"project %q lives in %s, and `sudo -n` may not run %s on this host; grant one of them (for example `%s ALL=(root) NOPASSWD: %s` in a file under /etc/sudoers.d/) or run the step yourself as root",
			target.Project, store.describe(), strings.Join(denied, " or "),
			"maidcafe", strings.Join(denied, ", "))
	}
	return attempts, nil
}

// runtimePullAttempts builds the pull for [imageRef] in [store]. Only that
// store is asked: the image has to land where the container runs from, so the
// daemon user's own store is not a fallback for a root-owned container — it is
// a different store that would hold a second copy of the image while the
// container's own store kept the old one.
func (r *nativeOpRunner) runtimePullAttempts(ctx context.Context, store composeStore, imageRef string) ([]opAttempt, error) {
	args := []string{"pull", imageRef}
	if !store.Elevated {
		return []opAttempt{{command: store.Path, args: args}}, nil
	}
	if !sudoRuns(ctx, store.Path) {
		return nil, fmt.Errorf(
			"the container lives in %s, and `sudo -n` may not run %s on this host; grant it (for example `maidcafe ALL=(root) NOPASSWD: %s` in a file under /etc/sudoers.d/) or pull the image yourself",
			store.describe(), store.Path, store.Path)
	}
	return []opAttempt{{command: "sudo", args: append([]string{"-n", store.Path}, args...)}}, nil
}

// runStage runs one stage's alternative attempts in order and returns the
// first that succeeds. A stage where every attempt failed reports the first
// attempt's output and exit code: the `sudo -n` or alternate-runtime variant is
// an implementation detail, so the direct attempt's message is the one an
// operator should see.
func runStage(ctx context.Context, attempts []opAttempt, sink io.Writer) (string, string, int, error) {
	var stdout, stderr string
	var exitCode int
	var primaryErr error
	for _, attempt := range attempts {
		attemptStdout, attemptStderr, attemptExit, err := runOpOnce(ctx, attempt, sink)
		if err == nil {
			return attemptStdout, attemptStderr, 0, nil
		}
		if primaryErr == nil {
			primaryErr = err
			stdout, stderr, exitCode = attemptStdout, attemptStderr, attemptExit
		}
		if ctx.Err() == context.DeadlineExceeded {
			break
		}
	}
	return stdout, stderr, exitCode, primaryErr
}

// appendStageOutput joins one stage's output onto what earlier stages produced.
func appendStageOutput(builder *strings.Builder, next string) {
	if next == "" {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(next)
}

// sudoAttempt returns the sudo -n prefix when the daemon is not root and
// sudo is available, else nil. It serves the operations that are not runtime
// invocations — systemd units, packages, the firewall, processes — where root
// is simply the only authority that can do the work. Runtime commands go
// through elevationPrefix instead, which also asks what the runtime is: a
// rootless runtime is the daemon user's own and sudo would address root's
// separate container store rather than elevate it.
func (r *nativeOpRunner) sudoAttempt() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return nil
	}
	return []string{"sudo", "-n"}
}

// normalizeProtocol and normalizeSource turn an absent firewall field into the
// explicit "any" the helper's grammar expects, so no argument in the helper's
// argv is ever an empty string.
func normalizeProtocol(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "any"
	}
	return value
}

func normalizeSource(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "any"
	}
	return value
}

// validNativeDirectory checks an absolute compose working directory with no
// traversal, mirroring MaidKit's SSH-layer check.
func validNativeDirectory(value string) bool {
	return value != "" && !strings.Contains(value, "..") && nativeDirectoryPattern.MatchString(value)
}

// runStages runs [stages] in order under the caller's concurrency slot and
// deadline, recording a single audit entry and updating the execution counters.
// Within a stage a failed attempt retries with the next (the sudo or
// alternate-runtime variant), and the first attempt's failure stays the primary
// signal when every one fails, mirroring the collectors; a stage that fails
// ends the operation, because a later stage depends on it. A multi-stage
// operation reports every stage's output, since what was downloaded and what
// was recreated are both worth showing.
//
// [task] is the run a client is watching, and the only difference between a
// task and a request-scoped run: it is told when a stage starts and ends, and
// the commands' output streams into it as it is written.
func (r *nativeOpRunner) runStages(
	ctx context.Context,
	slug string,
	displayName string,
	target string,
	stages []opStage,
	source string,
	invokedBy string,
	task *opTask,
) (executionResponse, int) {
	started := time.Now()
	response := executionResponse{Name: slug}
	var stdout, stderr strings.Builder
	var failure error
	for _, stage := range stages {
		if task != nil {
			task.stageStarted(stage.label)
		}
		stageStdout, stageStderr, exitCode, err := runStage(ctx, stage.attempts, outputSink(task))
		appendStageOutput(&stdout, stageStdout)
		appendStageOutput(&stderr, stageStderr)
		if task != nil {
			task.stageFinished(stage.label, err == nil)
		}
		if err != nil {
			failure = err
			response.ExitCode = exitCode
			break
		}
	}
	response.OK = failure == nil
	if failure == nil {
		response.ExitCode = 0
	}
	response.Stdout = stdout.String()
	response.Stderr = stderr.String()
	duration := time.Since(started)
	if r.executor.audit != nil {
		r.executor.audit.Record(auditEntry{
			Timestamp:   time.Now().UTC(),
			Name:        slug,
			DisplayName: displayName,
			Source:      source,
			InvokedBy:   invokedBy,
			OK:          response.OK,
			ExitCode:    response.ExitCode,
			DurationMS:  duration.Milliseconds(),
			Stdout:      response.Stdout,
			Stderr:      response.Stderr,
			Error:       auditErrorTail(response),
		})
	}
	status := http.StatusOK
	if failure != nil {
		r.executor.counts.failures.Add(1)
		status = http.StatusBadGateway
		if ctx.Err() == context.DeadlineExceeded {
			status = http.StatusGatewayTimeout
		}
	} else {
		r.executor.counts.successes.Add(1)
	}
	if failure != nil && source != "job" {
		r.publishFailure(slug, displayName, target, response, source, invokedBy, duration)
	}
	return response, status
}

// outputSink is the writer a stage's commands stream into, and nil when nobody
// is watching. A typed nil task must not become a non-nil io.Writer.
func outputSink(task *opTask) io.Writer {
	if task == nil {
		return nil
	}
	return task
}

// acquireSlot takes the executor's concurrency slot without waiting: a daemon
// already at its limit answers "too many runs" rather than queueing work it
// cannot start. The slot is held until the run ends, so it bounds tasks and
// script actions together, exactly as it did when every run lived inside a
// request.
func (r *nativeOpRunner) acquireSlot() bool {
	select {
	case r.executor.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *nativeOpRunner) releaseSlot() { <-r.executor.slots }

// runContext bounds a run. A zero timeout disables the deadline; the slow
// operations keep their explicit bound so they never run unbounded.
func (r *nativeOpRunner) runContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// publishFailure reports a failed native operation on its own notification
// channel (nativeop.failure) — distinct from the webhook.* channel script
// actions use — so container, process, systemd and compose operations can be
// routed or muted separately through notification preferences. Successes stay
// silent: routine operations notify nobody, and the invoker already received
// the synchronous result. Scheduled jobs are excluded because the job runner
// publishes job.failure for them.
func (r *nativeOpRunner) publishFailure(slug, displayName, target string, response executionResponse, source, invokedBy string, duration time.Duration) {
	if r.publisher == nil {
		return
	}
	p := r.publisher.Load()
	if p == nil {
		return
	}
	body := strings.TrimSpace(response.Stderr)
	if body == "" {
		body = strings.TrimSpace(response.Stdout)
	}
	if len(body) > 4096 {
		body = body[:4096]
	}
	metadata := map[string]any{
		"name":        slug,
		"exit_code":   response.ExitCode,
		"duration_ms": duration.Milliseconds(),
		"source":      source,
	}
	if target != "" {
		metadata["target"] = target
	}
	if invokedBy != "" {
		metadata["invoked_by"] = invokedBy
	}
	p.PublishNotification(context.Background(), notificationPayload{
		Kind:     "nativeop.failure",
		Title:    displayName + " failed",
		Body:     body,
		Metadata: metadata,
	})
}

// runOpOnce runs one attempt with bounded stdout/stderr capture and no shell.
// When [sink] is given — a task a client is watching — both streams are also
// written to it as they are produced, in the order the command printed them,
// because a pull that takes minutes is worth following while it runs; the
// bounded buffers still hold the attempt's own result.
func runOpOnce(ctx context.Context, attempt opAttempt, sink io.Writer) (stdout, stderr string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, attempt.command, attempt.args...)
	cmd.Dir = attempt.cwd
	if len(attempt.env) > 0 {
		cmd.Env = append(os.Environ(), attempt.env...)
	}
	cmd.WaitDelay = execPipeWaitDelay
	outBuf, errBuf := &limitedBuffer{limit: 8192}, &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	if sink != nil {
		cmd.Stdout = io.MultiWriter(outBuf, sink)
		cmd.Stderr = io.MultiWriter(errBuf, sink)
	}
	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}
