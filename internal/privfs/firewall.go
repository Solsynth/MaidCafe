package privfs

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Firewall operations. A firewall rule decides what the host accepts from the
// network, so this grant is about exposure rather than about a single program:
// an "allow" on an administrative port opens it to whoever can reach the host,
// and a "disable" removes every rule at once.
//
// The backend is declared here and never detected. Detection would mean running
// whichever tool happened to be installed, which is a binary choice made by the
// host state rather than by the operator — and the two backends do not even
// accept the same arguments.
//
// There is no way to reach a raw rule from a caller. A rule is a port, a
// protocol and a source, each validated against its own grammar, so the worst a
// caller can do within its grant is open or close a port — which is the whole
// point of granting it. It cannot name the `iptables` command, an option, a
// chain, a file or a command to run on reload.

// firewallBackends is every backend a grant may name.
var firewallBackends = map[string]bool{"ufw": true, "firewalld": true}

// allowedFirewallVerbs is every verb a grant may name. The set matches the
// daemon's native firewall operations.
var allowedFirewallVerbs = map[string]bool{
	"enable":  true,
	"disable": true,
	"allow":   true,
	"deny":    true,
	"delete":  true,
}

// firewallPortPattern is a port or an inclusive port range. A range is how ufw
// expresses one, and the bounds are checked numerically after the match.
var firewallPortPattern = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?$`)

// firewallServicePattern is a service name, which ufw resolves through
// /etc/services ("ufw allow ssh"). Only ufw accepts one: firewalld's port
// arguments are numeric, so a service name there is a refusal rather than a
// silently different rule.
var firewallServicePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

// firewallProtocols is every protocol a rule may name, with "any" meaning the
// backend's own "both" form.
var firewallProtocols = map[string]bool{"tcp": true, "udp": true, "any": true}

// FirewallGrant is one declared backend and the verbs it may receive.
type FirewallGrant struct {
	Backend string
	Verbs   map[string]bool
}

// String renders a rule the way an operator would read it, for the audit line.
func (r FirewallRule) String() string {
	source := ""
	if r.Source != "any" {
		source = " from " + r.Source
	}
	return fmt.Sprintf("%s %s/%s%s", r.Action, r.Port, r.Protocol, source)
}

// FirewallBackends lists the grantable backends, sorted.
func FirewallBackends() string {
	names := make([]string, 0, len(firewallBackends))
	for name := range firewallBackends {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// FirewallVerbs lists the grantable verbs, sorted.
func FirewallVerbs() string {
	verbs := make([]string, 0, len(allowedFirewallVerbs))
	for verb := range allowedFirewallVerbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ", ")
}

func validFirewallVerb(verb string) bool { return allowedFirewallVerbs[verb] }

// buildFirewallGrant validates the [firewall] table.
func buildFirewallGrant(backend string, verbs []string) (*FirewallGrant, error) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		return nil, fmt.Errorf("backend is required (one of: %s)", FirewallBackends())
	}
	if !firewallBackends[backend] {
		return nil, fmt.Errorf("backend %q is not one of: %s", backend, FirewallBackends())
	}
	if len(verbs) == 0 {
		return nil, fmt.Errorf("backend %q declares no verbs", backend)
	}
	allowed := make(map[string]bool, len(verbs))
	for _, verb := range verbs {
		verb = strings.ToLower(strings.TrimSpace(verb))
		if !validFirewallVerb(verb) {
			return nil, fmt.Errorf("verb %q is not one of: %s", verb, FirewallVerbs())
		}
		allowed[verb] = true
	}
	return &FirewallGrant{Backend: backend, Verbs: allowed}, nil
}

// verbList renders the granted verbs, sorted, for the `fs profiles` listing.
func (g *FirewallGrant) verbList() string {
	verbs := make([]string, 0, len(g.Verbs))
	for verb := range g.Verbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ",")
}

// CheckVerb reports whether the grant authorizes [verb].
func (g *FirewallGrant) CheckVerb(verb string) error {
	if !validFirewallVerb(verb) {
		return fmt.Errorf("verb %q is not one of: %s", verb, FirewallVerbs())
	}
	if !g.Verbs[verb] {
		granted := make([]string, 0, len(g.Verbs))
		for name := range g.Verbs {
			granted = append(granted, name)
		}
		sort.Strings(granted)
		return fmt.Errorf("verb %q is not granted for %s (granted: %s)", verb, g.Backend, strings.Join(granted, ", "))
	}
	return nil
}

// FirewallRule is one validated rule. Action is "allow" or "deny"; Protocol is
// "tcp", "udp" or "any"; Source is "any", an address or a CIDR block.
//
// Action is carried on the rule rather than only on the verb because ufw
// identifies a rule by its text, so a delete has to name the action it is
// deleting.
type FirewallRule struct {
	Action   string
	Port     string
	Protocol string
	Source   string
}

// ParseFirewallRule validates the three rule fields a caller supplies.
func ParseFirewallRule(action, port, protocol, source string) (FirewallRule, error) {
	rule := FirewallRule{
		Action:   strings.ToLower(strings.TrimSpace(action)),
		Port:     strings.TrimSpace(port),
		Protocol: strings.ToLower(strings.TrimSpace(protocol)),
		Source:   strings.TrimSpace(source),
	}
	if rule.Action != "allow" && rule.Action != "deny" {
		return rule, fmt.Errorf("rule action %q must be allow or deny", action)
	}
	if !firewallPortPattern.MatchString(rule.Port) && !firewallServicePattern.MatchString(rule.Port) {
		return rule, fmt.Errorf("port %q must be a number, a start:end range or a service name", port)
	}
	if firewallPortPattern.MatchString(rule.Port) {
		for _, bound := range strings.Split(rule.Port, ":") {
			value, err := strconv.Atoi(bound)
			if err != nil || value < 1 || value > 65535 {
				return rule, fmt.Errorf("port %q must be between 1 and 65535", port)
			}
		}
	}
	if rule.Protocol == "" {
		rule.Protocol = "any"
	}
	if !firewallProtocols[rule.Protocol] {
		return rule, fmt.Errorf("protocol %q must be tcp, udp or any", protocol)
	}
	if rule.Source == "" {
		rule.Source = "any"
	}
	if rule.Source != "any" && !validFirewallSource(rule.Source) {
		return rule, fmt.Errorf("source %q must be \"any\", an address or a CIDR block", source)
	}
	return rule, nil
}

// validFirewallSource accepts a bare address or a CIDR block, in either family.
// The parsed form is never used for the command: the text is passed through, so
// what the operator sees in the audit line is what the backend receives.
func validFirewallSource(source string) bool {
	if net.ParseIP(source) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(source)
	return err == nil
}

// FirewallCommands returns the commands that perform one verb, in order. A
// backend that keeps a permanent configuration and a running one needs two:
// firewalld writes the permanent rule and then reloads it.
//
// [rule] is unused by enable and disable, which is why it is passed separately:
// a grant for those two verbs does not also grant rule editing.
func (g *FirewallGrant) FirewallCommands(verb string, rule FirewallRule) ([][]string, error) {
	if err := g.CheckVerb(verb); err != nil {
		return nil, err
	}
	return FirewallCommand(g.Backend, verb, rule)
}

// FirewallCommand builds the commands for one firewall verb against a declared
// backend. Exported for the same reason as [PackageCommand]: the helper and the
// daemon's own fallback build from one table.
func FirewallCommand(backend, verb string, rule FirewallRule) ([][]string, error) {
	if !ValidFirewallBackend(backend) {
		return nil, fmt.Errorf("firewall backend %q is not one of: %s", backend, FirewallBackends())
	}
	if !ValidFirewallVerb(verb) {
		return nil, fmt.Errorf("firewall verb %q is not one of: %s", verb, FirewallVerbs())
	}
	if verb == "enable" || verb == "disable" {
		return toggleCommands(backend, verb)
	}
	return ruleCommands(backend, verb, rule)
}

// ValidFirewallBackend reports whether [backend] is a backend this package can
// build a command for.
func ValidFirewallBackend(backend string) bool { return firewallBackends[backend] }

// ValidFirewallVerb reports whether [verb] is a firewall verb.
func ValidFirewallVerb(verb string) bool { return allowedFirewallVerbs[verb] }

// FirewallBackendPreference is the order the daemon probes backends in when it
// is building its own command. ufw first: it is the one an operator who never
// chose a firewall has, and a host that runs both is a host where the ufw rules
// are the ones someone wrote.
func FirewallBackendPreference() []string { return []string{"ufw", "firewalld"} }

// FirewallBinaryFor returns the command a backend runs.
func FirewallBinaryFor(backend string) string {
	if backend == "firewalld" {
		return "firewall-cmd"
	}
	return "ufw"
}

// toggleCommands turns the whole firewall on or off.
//
// ufw asks before it enables (it may disrupt the connection it was reached
// over), and there is no terminal to answer with, so --force is what makes a
// non-interactive enable possible at all.
//
// firewalld's on/off state is its service state, so this reaches systemctl. The
// unit is fixed here and is not caller-selectable: a firewall grant is not a
// grant over units.
func toggleCommands(backend, verb string) ([][]string, error) {
	switch backend {
	case "ufw":
		if verb == "enable" {
			return [][]string{{"ufw", "--force", "enable"}}, nil
		}
		return [][]string{{"ufw", "disable"}}, nil
	default:
		action := "start"
		if verb == "disable" {
			action = "stop"
		}
		return [][]string{{"systemctl", action, "firewalld.service"}}, nil
	}
}

// ruleCommands adds or removes one rule.
func ruleCommands(backend, verb string, rule FirewallRule) ([][]string, error) {
	if backend == "ufw" {
		return ufwRuleCommands(verb, rule)
	}
	return firewalldRuleCommands(verb, rule)
}

// ufwRuleCommands is ufw's rule form.
//
// ufw deletes a rule by repeating it, so `delete` needs the action as well as
// the rule: `ufw delete allow 80/tcp` and `ufw delete deny 80` are different
// rules.
func ufwRuleCommands(verb string, rule FirewallRule) ([][]string, error) {
	spec := []string{}
	if rule.Source != "any" {
		spec = append(spec, "from", rule.Source, "to", "any", "port", rule.Port)
		if rule.Protocol != "any" {
			spec = append(spec, "proto", rule.Protocol)
		}
	} else {
		target := rule.Port
		if rule.Protocol != "any" {
			target = rule.Port + "/" + rule.Protocol
		}
		spec = append(spec, target)
	}
	switch verb {
	case "allow", "deny":
		return [][]string{append([]string{"ufw", verb}, spec...)}, nil
	default:
		// --force: ufw asks for confirmation on delete, and there is no
		// terminal to answer with.
		return [][]string{append([]string{"ufw", "--force", "delete", rule.Action}, spec...)}, nil
	}
}

// firewalldRuleCommands is firewalld's rule form.
//
// A plain port rule and a rich rule are different objects, and removing one
// does not remove the other, so the verb has to reproduce the same shape the
// add used. Allow is a plain port rule when it has no source, because that is
// what the rest of the ecosystem writes and what an operator will see in this
// host's configuration; everything else is a rich rule.
func firewalldRuleCommands(verb string, rule FirewallRule) ([][]string, error) {
	if rule.Protocol == "any" {
		return nil, fmt.Errorf("firewalld needs an explicit protocol for a port rule (tcp or udp); \"any\" would have to be two rules, which this helper does not guess at")
	}
	plain := rule.Action == "allow" && rule.Source == "any"
	if plain {
		portSpec := rule.Port + "/" + rule.Protocol
		flag := "--add-port="
		if verb == "delete" {
			flag = "--remove-port="
		}
		return reload([][]string{{"firewall-cmd", "--permanent", flag + portSpec}}), nil
	}
	rich, err := richRules(rule)
	if err != nil {
		return nil, err
	}
	flag := "--add-rich-rule="
	if verb == "delete" {
		flag = "--remove-rich-rule="
	}
	commands := make([][]string, 0, len(rich)+1)
	for _, one := range rich {
		commands = append(commands, []string{"firewall-cmd", "--permanent", flag + one})
	}
	return append(commands, reload(nil)...), nil
}

// richRules renders the rich rule(s) for a rule that a plain port rule cannot
// express.
//
// A rich rule for a port needs a family. With a source, the family is the
// source's. Without one, the rule would silently cover IPv4 alone, so both
// families are emitted — a deny that leaves IPv6 open is not a deny.
func richRules(rule FirewallRule) ([]string, error) {
	target := "accept"
	if rule.Action == "deny" {
		target = "drop"
	}
	render := func(family, source string) string {
		parts := []string{fmt.Sprintf("rule family=%q", family)}
		if source != "" {
			parts = append(parts, fmt.Sprintf("source address=%q", source))
		}
		parts = append(parts, fmt.Sprintf("port port=%q protocol=%q", rule.Port, rule.Protocol), target)
		return strings.Join(parts, " ")
	}
	if rule.Source != "any" {
		family := "ipv4"
		if ip := net.ParseIP(rule.Source); ip != nil && ip.To4() == nil {
			family = "ipv6"
		} else if _, block, err := net.ParseCIDR(rule.Source); err == nil {
			if block.IP.To4() == nil {
				family = "ipv6"
			}
		}
		return []string{render(family, rule.Source)}, nil
	}
	return []string{render("ipv4", ""), render("ipv6", "")}, nil
}

// reload appends firewalld's reload, which is what makes a permanent rule take
// effect. [commands] is the prefix, so a caller can build either shape with one
// expression.
//
// The append copies rather than extending in place: the caller's slice is
// built inline, and growing it would write past len into a fresh array only by
// luck.
func reload(commands [][]string) [][]string {
	out := make([][]string, 0, len(commands)+1)
	out = append(out, commands...)
	return append(out, []string{"firewall-cmd", "--reload"})
}

// firewallPaths is where a backend's binary is looked for, in order. Enabling
// or disabling firewalld is a service action, so systemctl's own list is
// reused rather than duplicated: two lists of the same paths would be two
// things to keep in step.
var firewallPaths = map[string][]string{
	"ufw":          {"/usr/sbin/ufw", "/sbin/ufw", "/usr/bin/ufw", "/bin/ufw"},
	"firewall-cmd": {"/usr/bin/firewall-cmd", "/bin/firewall-cmd"},
	"systemctl":    systemctlPaths,
}

// FindFirewallBinary returns the absolute path of a backend's command.
func FindFirewallBinary(binary string) (string, error) {
	candidates, ok := firewallPaths[binary]
	if !ok {
		return "", fmt.Errorf("%s is not a firewall command this helper runs", binary)
	}
	return findExecutable(candidates, binary)
}
