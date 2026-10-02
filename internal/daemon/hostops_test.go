package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// Host-wide operations the daemon executes natively: package management and
// firewall rules. They are the two families that reach root software
// installation and network exposure, so the cases that matter are the ones
// where a request is refused before anything runs, and the ones that pin which
// program is reached when the helper is not in the picture.

func hostOpsRunner(t *testing.T, priv config.PrivConfig) *nativeOpRunner {
	t.Helper()
	root := t.TempDir()
	cfg := config.DaemonConfig{
		ID: "host-ops", Transport: "http", Listen: "127.0.0.1:0",
		MetricsSecret: "metrics-secret", MetricsInterval: time.Hour,
		StreamInterval: time.Second, Runtimes: []string{"java"},
		ProcessesLimit: 50, RequestTimeout: 5 * time.Second,
		ScriptTimeout: 5 * time.Second, MaxBodyBytes: 65536, MaxConcurrentRuns: 1,
		AuditPath: filepath.Join(root, "audit.jsonl"),
		Priv:      priv,
	}
	executor := NewWebhookExecutor(cfg)
	executor.SetAuditLogger(NewAuditLogger(cfg.AuditPath, nil))
	runner := &nativeOpRunner{
		executor: executor,
		runtimes: func(context.Context) map[string]string { return nil },
	}
	runner.SetScriptTimeout(cfg.ScriptTimeout)
	runner.SetPrivilegedPolicy(priv, &privRunner{})
	return runner
}

// standInHelper writes a script that records its argv and answers as the helper
// would, so a test can see exactly which command the daemon reached.
func standInHelper(t *testing.T) (path, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	path = filepath.Join(dir, "maidkit-priv-stand-in")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> ` + argvLog + `
echo "{\"event\":\"privfs\",\"ok\":true}" >&2
echo "ran: $*"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argvLog
}

func readArgvLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// The manager is the helper's to choose, so the daemon passes a verb and a name
// and nothing else — no binary, no flags, no second operand.
func TestPackageOpRoutesThroughTheHelper(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Packages: true})

	response, status, requestErr := runner.dispatch(
		context.Background(), "package.install", opParams{name: "nginx"}, "http", "tester",
	)
	if requestErr != nil {
		t.Fatalf("request error: %v", requestErr.message)
	}
	if status != http.StatusOK || !response.OK {
		t.Fatalf("status = %d, response = %+v", status, response)
	}
	if got := readArgvLog(t, argvLog); got != "packages install nginx" {
		t.Fatalf("helper argv = %q", got)
	}
	if response.Stdout != "ran: packages install nginx\n" {
		t.Fatalf("stdout = %q", response.Stdout)
	}
}

// A verb that takes no package name carries none, so the helper's arity check
// sees exactly what it expects.
func TestPackageRefreshCarriesNoName(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Packages: true})

	if _, status, requestErr := runner.dispatch(
		context.Background(), "package.refresh", opParams{}, "http", "tester",
	); requestErr != nil || status != http.StatusOK {
		t.Fatalf("status = %d, request error = %v", status, requestErr)
	}
	if got := readArgvLog(t, argvLog); got != "packages refresh" {
		t.Fatalf("helper argv = %q", got)
	}
}

// The name grammar is enforced at the request too, so a name the helper would
// refuse never reaches it — and, more importantly, is never resolved into a
// command line by the daemon's own fallback path either.
func TestPackageOpRefusesNamesOutsideTheGrammar(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Packages: true})

	for _, name := range []string{"./x.deb", "nginx=1.2", "-y", "nginx:amd64", "a b"} {
		_, _, requestErr := runner.dispatch(
			context.Background(), "package.install", opParams{name: name}, "http", "tester",
		)
		if requestErr == nil || requestErr.status != http.StatusBadRequest {
			t.Fatalf("install %q: request error = %v, want a 400", name, requestErr)
		}
	}
	if _, err := os.Stat(argvLog); err == nil {
		t.Fatalf("a refused name ran something: %s", readArgvLog(t, argvLog))
	}
}

// A name on a verb that has no operand in its grammar is a refusal, not a
// silently ignored field.
func TestPackageOpRefusesANameOnNameLessVerbs(t *testing.T) {
	helper, _ := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Packages: true})
	_, _, requestErr := runner.dispatch(
		context.Background(), "package.upgrade", opParams{name: "nginx"}, "http", "tester",
	)
	if requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("request error = %v, want a 400", requestErr)
	}
}

// The firewall rule travels as three validated fields; the helper does the
// parsing again against its own backend, and the daemon's job is to hand it the
// same normalized text.
func TestFirewallOpRoutesThroughTheHelper(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})

	_, status, requestErr := runner.dispatch(
		context.Background(), "firewall.allow",
		opParams{port: "443", protocol: "tcp", source: "10.0.0.0/8"}, "http", "tester",
	)
	if requestErr != nil || status != http.StatusOK {
		t.Fatalf("status = %d, request error = %v", status, requestErr)
	}
	if got := readArgvLog(t, argvLog); got != "firewall allow 443 tcp 10.0.0.0/8" {
		t.Fatalf("helper argv = %q", got)
	}

}

// A toggle carries no rule at all, so the helper's argv is just the verb.
func TestFirewallToggleCarriesNoRule(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})

	if _, _, requestErr := runner.dispatch(
		context.Background(), "firewall.enable", opParams{}, "http", "tester",
	); requestErr != nil {
		t.Fatalf("enable: %v", requestErr.message)
	}
	if got := readArgvLog(t, argvLog); got != "firewall enable" {
		t.Fatalf("helper argv = %q", got)
	}
}

// An absent protocol and source become the explicit "any" the helper's grammar
// expects, so no argument in its argv is ever an empty string.
func TestFirewallRuleNormalizesAbsentFields(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})

	if _, _, requestErr := runner.dispatch(
		context.Background(), "firewall.allow", opParams{port: "80"}, "http", "tester",
	); requestErr != nil {
		t.Fatalf("allow: %v", requestErr.message)
	}
	if got := readArgvLog(t, argvLog); got != "firewall allow 80 any any" {
		t.Fatalf("helper argv = %q", got)
	}
}

// ufw identifies a rule by its full text, so a delete has to name the action
// it is deleting. The daemon passes it through rather than guessing.
func TestFirewallDeleteCarriesTheRuleAction(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})

	_, status, requestErr := runner.dispatch(
		context.Background(), "firewall.delete",
		opParams{ruleAction: "deny", port: "22", protocol: "tcp", source: "any"}, "http", "tester",
	)
	if requestErr != nil || status != http.StatusOK {
		t.Fatalf("status = %d, request error = %v", status, requestErr)
	}
	if got := readArgvLog(t, argvLog); got != "firewall delete deny 22 tcp any" {
		t.Fatalf("helper argv = %q", got)
	}
}

func TestFirewallOpRefusesMalformedRules(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})

	cases := [][4]string{
		{"80", "tcp", "10.0.0.0/33"}, // not a CIDR
		{"70000", "tcp", "any"},      // out of the port range
		{"80", "icmp", "any"},        // not a protocol this grammar has
		{"80", "tcp", "any;id"},      // not an address
	}
	for _, tc := range cases {
		_, _, requestErr := runner.dispatch(
			context.Background(), "firewall.allow",
			opParams{port: tc[0], protocol: tc[1], source: tc[2]}, "http", "tester",
		)
		if requestErr == nil || requestErr.status != http.StatusBadRequest {
			t.Fatalf("rule %q: request error = %v, want a 400", tc, requestErr)
		}
	}
	// A delete with no action is not a delete of anything.
	if _, _, requestErr := runner.dispatch(
		context.Background(), "firewall.delete", opParams{port: "80", protocol: "tcp", source: "any"}, "http", "tester",
	); requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("a delete without an action: request error = %v, want a 400", requestErr)
	}
	if _, err := os.Stat(argvLog); err == nil {
		t.Fatalf("a refused rule ran something: %s", readArgvLog(t, argvLog))
	}
}

// Without the helper the operation falls back to the manager installed on the
// host, tried directly and then through sudo — the same shape the collectors
// and the container operations already use. PATH is narrowed to one stand-in so
// the choice is the test's, not the machine's.
func TestPackageOpFallsBackToTheDetectedManager(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argvLog + "\necho done\n"
	if err := os.WriteFile(filepath.Join(dir, "apt-get"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	runner := hostOpsRunner(t, config.PrivConfig{})
	response, status, requestErr := runner.dispatch(
		context.Background(), "package.install", opParams{name: "nginx"}, "http", "tester",
	)
	if requestErr != nil {
		t.Fatalf("request error: %v", requestErr.message)
	}
	if status != http.StatusOK || !response.OK {
		t.Fatalf("status = %d, response = %+v", status, response)
	}
	// The manager's own shape, with the name after the separator.
	if got := readArgvLog(t, argvLog); got != "-y install -- nginx" {
		t.Fatalf("manager argv = %q", got)
	}
}

// A host with neither the helper nor a manager says so, rather than reporting a
// command it never ran.
func TestPackageOpWithoutAManagerSaysSo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := hostOpsRunner(t, config.PrivConfig{})
	_, _, requestErr := runner.dispatch(
		context.Background(), "package.install", opParams{name: "nginx"}, "http", "tester",
	)
	if requestErr == nil || !strings.Contains(requestErr.message, "no package manager") {
		t.Fatalf("request error = %v", requestErr)
	}
	// The same for a host with no firewall.
	_, _, requestErr = runner.dispatch(
		context.Background(), "firewall.allow", opParams{port: "80", protocol: "tcp", source: "any"}, "http", "tester",
	)
	if requestErr == nil || !strings.Contains(requestErr.message, "no supported firewall") {
		t.Fatalf("firewall request error = %v", requestErr)
	}
}

// The unfired family is not reached: routing packages does not route systemd,
// and each switch only affects its own operations.
func TestRoutingIsPerFamily(t *testing.T) {
	helper, argvLog := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Packages: true})
	// A firewall op is not routed, and this host has no firewall.
	_, _, requestErr := runner.dispatch(
		context.Background(), "firewall.enable", opParams{}, "http", "tester",
	)
	if requestErr == nil {
		t.Fatal("a firewall op ran with only the package switch set")
	}
	if _, err := os.Stat(argvLog); err == nil {
		t.Fatalf("the helper was reached for an unrouted family: %s", readArgvLog(t, argvLog))
	}
}

// The new operations are reported to the cloud as invocable, and they are
// reserved names, so a webhook cannot shadow one.
func TestNewNativeOpsAreReservedAndReported(t *testing.T) {
	reported := map[string]bool{}
	for _, hook := range nativeOpReport() {
		reported[hook.Name] = true
	}
	for _, slug := range []string{
		"package.refresh", "package.upgrade", "package.install", "package.remove",
		"firewall.enable", "firewall.disable", "firewall.allow", "firewall.deny", "firewall.delete",
	} {
		if !reported[slug] {
			t.Fatalf("%s is not reported as invocable", slug)
		}
		if !isNativeOpSlug(slug) {
			t.Fatalf("%s is not reserved", slug)
		}
		encoded, err := json.Marshal(config.WebhookConfig{Name: slug})
		if err != nil || !strings.Contains(string(encoded), slug) {
			t.Fatalf("%s does not survive encoding: %s", slug, encoded)
		}
	}
}

// The HTTP route fills its params from the path and a `decorate` callback,
// while the relay, the stdio pipe and scheduled jobs fill them from a decoded
// JSON body through nativeParamsFromValues. A field missing from that parser
// would work over HTTP and silently arrive empty through the relay, so it is
// pinned here.
func TestNativeParamsFromValuesCarriesTheNewFields(t *testing.T) {
	packageParams := nativeParamsFromValues("package.install", map[string]any{"name": "nginx"})
	if packageParams.name != "nginx" {
		t.Fatalf("package name = %q", packageParams.name)
	}
	// A JSON number or a string both arrive from the wire.
	if got := nativeParamsFromValues("package.install", map[string]any{"name": 7}); got.name != "" {
		t.Fatalf("a non-string name became %q", got.name)
	}

	firewallParams := nativeParamsFromValues("firewall.delete", map[string]any{
		"rule_action": "deny", "port": "22", "protocol": "tcp", "source": "any",
	})
	if firewallParams.ruleAction != "deny" || firewallParams.port != "22" ||
		firewallParams.protocol != "tcp" || firewallParams.source != "any" {
		t.Fatalf("firewall params = %+v", firewallParams)
	}
	// The rule fields are not confused with the container or compose ones.
	if got := nativeParamsFromValues("firewall.allow", map[string]any{"port": "80"}); got.target != "" || got.directory != "" {
		t.Fatalf("firewall params leaked into another family: %+v", got)
	}
}

// A firewall delete whose body carries no action is refused by the dispatcher
// whichever transport delivered it, rather than deleting a rule the caller
// cannot name.
func TestFirewallDeleteWithoutAnActionIsRefused(t *testing.T) {
	helper, _ := standInHelper(t)
	runner := hostOpsRunner(t, config.PrivConfig{Helper: helper, Firewall: true})
	params := nativeParamsFromValues("firewall.delete", map[string]any{"port": "80", "protocol": "tcp", "source": "any"})
	_, _, requestErr := runner.dispatch(context.Background(), "firewall.delete", params, "relay", "tester")
	if requestErr == nil || requestErr.status != http.StatusBadRequest {
		t.Fatalf("request error = %v, want a 400", requestErr)
	}
}
