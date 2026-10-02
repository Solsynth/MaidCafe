package privfs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One suite for the two grants that reach a host-wide operation rather than a
// file: packages and the firewall. Both turn a caller's argument into a root
// command, so the cases that matter are the ones where an argument could become
// something other than what it looks like.

func TestLoadSetValidatesPackageGrants(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"apt install and remove", "[packages]\nmanager = \"apt\"\nverbs = [\"install\", \"remove\"]\n", true},
		{"every manager", "[packages]\nmanager = \"xbps\"\nverbs = [\"refresh\", \"upgrade\"]\n", true},
		{"unknown manager", "[packages]\nmanager = \"nix\"\nverbs = [\"install\"]\n", false},
		{"missing manager", "[packages]\nverbs = [\"install\"]\n", false},
		{"homebrew is not grantable", "[packages]\nmanager = \"brew\"\nverbs = [\"install\"]\n", false},
		{"no verbs", "[packages]\nmanager = \"apt\"\n", false},
		{"unknown verb", "[packages]\nmanager = \"apt\"\nverbs = [\"purge\"]\n", false},
		{"manager is a path", "[packages]\nmanager = \"/usr/bin/apt-get\"\nverbs = [\"install\"]\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSet(writeProfile(t, tc.body), Options{})
			if (err == nil) != tc.ok {
				t.Fatalf("LoadSet = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestLoadSetValidatesFirewallGrants(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"ufw rules", "[firewall]\nbackend = \"ufw\"\nverbs = [\"allow\", \"delete\"]\n", true},
		{"firewalld toggle", "[firewall]\nbackend = \"firewalld\"\nverbs = [\"enable\", \"disable\"]\n", true},
		{"unknown backend", "[firewall]\nbackend = \"nftables\"\nverbs = [\"allow\"]\n", false},
		{"missing backend", "[firewall]\nverbs = [\"allow\"]\n", false},
		{"no verbs", "[firewall]\nbackend = \"ufw\"\n", false},
		{"unknown verb", "[firewall]\nbackend = \"ufw\"\nverbs = [\"flush\"]\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSet(writeProfile(t, tc.body), Options{})
			if (err == nil) != tc.ok {
				t.Fatalf("LoadSet = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// A file that declares only a package grant is usable on its own: every grant
// is independent, so an operator who wants packages does not have to declare a
// file profile to get them.
func TestLoadSetAcceptsASingleGrantType(t *testing.T) {
	path := writeProfile(t, "[packages]\nmanager = \"dnf\"\nverbs = [\"upgrade\"]\n")
	set, err := LoadSet(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if set.Packages() == nil || set.Firewall() != nil || len(set.Names()) != 0 {
		t.Fatalf("unexpected set: packages=%v firewall=%v names=%d", set.Packages(), set.Firewall(), len(set.Names()))
	}
}

// Every manager's invocation, pinned. The point of the table is the position of
// the name: it is always the last argument and always follows "--", so it can
// never be read as an option, and a verb with no name never carries one.
func TestPackageCommandShapes(t *testing.T) {
	want := map[string]map[string][]string{
		"apt": {
			"refresh": {"apt-get", "update"},
			"upgrade": {"apt-get", "-y", "upgrade"},
			"install": {"apt-get", "-y", "install", "--", "nginx"},
			"remove":  {"apt-get", "-y", "remove", "--", "nginx"},
		},
		"dnf": {
			"refresh": {"dnf", "makecache"},
			"upgrade": {"dnf", "-y", "upgrade"},
			"install": {"dnf", "-y", "install", "--", "nginx"},
			"remove":  {"dnf", "-y", "remove", "--", "nginx"},
		},
		"yum": {
			"refresh": {"yum", "makecache"},
			"upgrade": {"yum", "-y", "upgrade"},
			"install": {"yum", "-y", "install", "--", "nginx"},
			"remove":  {"yum", "-y", "remove", "--", "nginx"},
		},
		"pacman": {
			"refresh": {"pacman", "-Sy"},
			"upgrade": {"pacman", "--noconfirm", "-Syu"},
			"install": {"pacman", "--noconfirm", "-S", "--", "nginx"},
			"remove":  {"pacman", "--noconfirm", "-R", "--", "nginx"},
		},
		"zypper": {
			"refresh": {"zypper", "--non-interactive", "refresh"},
			"upgrade": {"zypper", "--non-interactive", "update"},
			"install": {"zypper", "--non-interactive", "install", "--", "nginx"},
			"remove":  {"zypper", "--non-interactive", "remove", "--", "nginx"},
		},
		"apk": {
			"refresh": {"apk", "update"},
			"upgrade": {"apk", "upgrade"},
			"install": {"apk", "add", "--", "nginx"},
			"remove":  {"apk", "del", "--", "nginx"},
		},
		"xbps": {
			"refresh": {"xbps-install", "-S"},
			"upgrade": {"xbps-install", "-yu"},
			"install": {"xbps-install", "-y", "--", "nginx"},
			// Removal is a different binary in the xbps family.
			"remove": {"xbps-remove", "-y", "--", "nginx"},
		},
	}
	for manager, verbs := range want {
		for verb, expected := range verbs {
			name := ""
			if verb == "install" || verb == "remove" {
				name = "nginx"
			}
			got, err := PackageCommand(manager, verb, name)
			if err != nil {
				t.Fatalf("%s %s: %v", manager, verb, err)
			}
			if strings.Join(got, " ") != strings.Join(expected, " ") {
				t.Fatalf("%s %s = %q, want %q", manager, verb, got, expected)
			}
			if name != "" {
				if last := got[len(got)-1]; last != name {
					t.Fatalf("%s %s: name is %q, not the last argument", manager, verb, last)
				}
				if sep := got[len(got)-2]; sep != "--" {
					t.Fatalf("%s %s: separator is %q, want --", manager, verb, sep)
				}
			}
		}
	}
}

// The name grammar is the boundary: everything that would let a caller name
// something other than the package the distribution calls by this name is
// refused before a manager ever sees it.
func TestPackageCommandRefusesNamesOutsideTheGrammar(t *testing.T) {
	for _, name := range []string{
		"./nginx.deb",                // a file, which apt would install as root
		"/tmp/nginx.deb",             // the same, absolute
		"http://evil.test/nginx.deb", // a URL: root execution of a remote file
		"nginx=1.2.3",                // a version pin, i.e. a different source
		"nginx:amd64",                // an architecture qualifier
		"nginx@1.2.3",                // pacman's version syntax
		"-y",                         // an option, whatever precedes it
		"--allow-unauthenticated",    // the same, spelled out
		"nginx extra",                // a second operand
		"nginx;id",                   // a shell metacharacter
		"$(id)",                      // the same
		"",                           // nothing at all
		"..",                         // not a name
		"nginx\n",                    // a newline
		strings.Repeat("n", 129),     // over the length bound
	} {
		if _, err := PackageCommand("apt", "install", name); err == nil {
			t.Fatalf("install %q was accepted", name)
		}
	}
	// The names that must survive: the distributions' ordinary spellings.
	for _, name := range []string{"nginx", "python3.12", "g++", "libssl-dev", "ca-certificates", "7zip", "lib32-gcc-libs"} {
		if _, err := PackageCommand("apt", "install", name); err != nil {
			t.Fatalf("install %q was refused: %v", name, err)
		}
	}
}

// A verb with no operand in its grammar refuses one, so a caller cannot smuggle
// a package name into a global upgrade.
func TestPackageCommandRefusesANameForNameLessVerbs(t *testing.T) {
	for _, verb := range []string{"refresh", "upgrade"} {
		if _, err := PackageCommand("apt", verb, "nginx"); err == nil {
			t.Fatalf("%s accepted a package name", verb)
		}
		if _, err := PackageCommand("apt", verb, ""); err != nil {
			t.Fatalf("%s was refused: %v", verb, err)
		}
	}
	// install and remove require one.
	for _, verb := range []string{"install", "remove"} {
		if _, err := PackageCommand("apt", verb, ""); err == nil {
			t.Fatalf("%s accepted an empty name", verb)
		}
	}
}

func TestPackageEnvKeepsAptNonInteractive(t *testing.T) {
	if env := PackageEnv("apt"); len(env) != 1 || env[0] != "DEBIAN_FRONTEND=noninteractive" {
		t.Fatalf("PackageEnv(apt) = %q", env)
	}
	for _, manager := range []string{"dnf", "pacman", "zypper", "apk", "xbps", "yum"} {
		if env := PackageEnv(manager); len(env) != 0 {
			t.Fatalf("PackageEnv(%s) = %q, want none", manager, env)
		}
	}
}

func TestParseFirewallRuleGrammar(t *testing.T) {
	ok := []struct {
		action                   string
		port, protocol, source   string
		wantProtocol, wantSource string
	}{
		{"allow", "80", "tcp", "any", "tcp", "any"},
		{"deny", "80", "", "", "any", "any"},
		{"allow", "8080:8090", "udp", "10.0.0.0/8", "udp", "10.0.0.0/8"},
		{"allow", "ssh", "any", "2001:db8::/32", "any", "2001:db8::/32"},
		{"deny", "443", "tcp", "192.168.1.5", "tcp", "192.168.1.5"},
	}
	for _, tc := range ok {
		rule, err := ParseFirewallRule(tc.action, tc.port, tc.protocol, tc.source)
		if err != nil {
			t.Fatalf("ParseFirewallRule(%q,%q,%q,%q): %v", tc.action, tc.port, tc.protocol, tc.source, err)
		}
		if rule.Protocol != tc.wantProtocol || rule.Source != tc.wantSource {
			t.Fatalf("normalized to protocol=%q source=%q, want %q %q", rule.Protocol, rule.Source, tc.wantProtocol, tc.wantSource)
		}
	}
	bad := [][4]string{
		{"allow", "0", "tcp", "any"},          // below the port range
		{"allow", "70000", "tcp", "any"},      // above it
		{"allow", "80:70000", "tcp", "any"},   // above it in a range
		{"allow", "80/../..", "tcp", "any"},   // not a port at all
		{"allow", "80", "icmp", "any"},        // not a protocol this grammar has
		{"allow", "80", "tcp", "10.0.0.0/33"}, // not a CIDR
		{"allow", "80", "tcp", "any;id"},      // not an address
		{"drop", "80", "tcp", "any"},          // not a rule action
		{"", "80", "tcp", "any"},
	}
	for _, tc := range bad {
		if _, err := ParseFirewallRule(tc[0], tc[1], tc[2], tc[3]); err == nil {
			t.Fatalf("ParseFirewallRule(%q) was accepted", tc)
		}
	}
}

func TestUfwCommands(t *testing.T) {
	rule := func(action, port, protocol, source string) FirewallRule {
		parsed, err := ParseFirewallRule(action, port, protocol, source)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	cases := []struct {
		name string
		verb string
		rule FirewallRule
		want []string
	}{
		{name: "enable", verb: "enable", want: []string{"ufw", "--force", "enable"}},
		{name: "disable", verb: "disable", want: []string{"ufw", "disable"}},
		{name: "allow", verb: "allow", rule: rule("allow", "80", "tcp", "any"), want: []string{"ufw", "allow", "80/tcp"}},
		{name: "allow any protocol", verb: "allow", rule: rule("allow", "80", "any", "any"), want: []string{"ufw", "allow", "80"}},
		{name: "deny", verb: "deny", rule: rule("deny", "22", "tcp", "any"), want: []string{"ufw", "deny", "22/tcp"}},
		{name: "allow a service", verb: "allow", rule: rule("allow", "ssh", "any", "any"), want: []string{"ufw", "allow", "ssh"}},
		{name: "allow from a source", verb: "allow", rule: rule("allow", "443", "tcp", "10.0.0.0/8"), want: []string{"ufw", "allow", "from", "10.0.0.0/8", "to", "any", "port", "443", "proto", "tcp"}},
		// A delete repeats the rule, so it has to name the action too.
		{name: "delete an allow", verb: "delete", rule: rule("allow", "80", "tcp", "any"), want: []string{"ufw", "--force", "delete", "allow", "80/tcp"}},
		{name: "delete a deny", verb: "delete", rule: rule("deny", "22", "tcp", "any"), want: []string{"ufw", "--force", "delete", "deny", "22/tcp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commands, err := FirewallCommand("ufw", tc.verb, tc.rule)
			if err != nil {
				t.Fatal(err)
			}
			if len(commands) != 1 || strings.Join(commands[0], " ") != strings.Join(tc.want, " ") {
				t.Fatalf("= %q, want %q", commands, tc.want)
			}
		})
	}
}

// firewalld writes the permanent configuration and then reloads it, so a rule
// change is always two commands in that order. A plain port rule and a rich
// rule are different objects, and the delete has to reproduce the add.
func TestFirewalldCommands(t *testing.T) {
	parse := func(action, port, protocol, source string) FirewallRule {
		rule, err := ParseFirewallRule(action, port, protocol, source)
		if err != nil {
			t.Fatal(err)
		}
		return rule
	}
	t.Run("enable starts the service", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "enable", FirewallRule{})
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) != 1 || strings.Join(commands[0], " ") != "systemctl start firewalld.service" {
			t.Fatalf("= %q", commands)
		}
	})
	t.Run("disable stops it", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "disable", FirewallRule{})
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) != 1 || strings.Join(commands[0], " ") != "systemctl stop firewalld.service" {
			t.Fatalf("= %q", commands)
		}
	})
	t.Run("a plain allow is a port rule, then a reload", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "allow", parse("allow", "80", "tcp", "any"))
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) != 2 {
			t.Fatalf("= %q, want a rule and a reload", commands)
		}
		if strings.Join(commands[0], " ") != "firewall-cmd --permanent --add-port=80/tcp" {
			t.Fatalf("add = %q", commands[0])
		}
		if strings.Join(commands[1], " ") != "firewall-cmd --reload" {
			t.Fatalf("reload = %q", commands[1])
		}
	})
	t.Run("deleting it removes the port rule", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "delete", parse("allow", "80", "tcp", "any"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(commands[0], " ") != "firewall-cmd --permanent --remove-port=80/tcp" {
			t.Fatalf("remove = %q", commands[0])
		}
	})
	// A deny cannot be a port rule: firewalld has no "closed port" object, so
	// it is a rich rule that drops the traffic.
	t.Run("a deny is a rich rule", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "deny", parse("deny", "80", "tcp", "any"))
		if err != nil {
			t.Fatal(err)
		}
		// Both families: a deny that leaves IPv6 open is not a deny.
		if len(commands) != 3 {
			t.Fatalf("= %q, want two rules and a reload", commands)
		}
		for i, family := range []string{"ipv4", "ipv6"} {
			got := strings.Join(commands[i], " ")
			if !strings.Contains(got, `family="`+family+`"`) || !strings.Contains(got, `protocol="tcp"`) || !strings.HasSuffix(got, "drop") {
				t.Fatalf("rule %d = %q", i, got)
			}
		}
	})
	t.Run("a source narrows the family", func(t *testing.T) {
		commands, err := FirewallCommand("firewalld", "allow", parse("allow", "443", "tcp", "2001:db8::/32"))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(commands[0], " ")
		if !strings.Contains(got, `family="ipv6"`) || !strings.Contains(got, `source address="2001:db8::/32"`) {
			t.Fatalf("rule = %q", got)
		}
		if len(commands) != 2 {
			t.Fatalf("= %q, want one rule and a reload", commands)
		}
	})
	// "any" would have to become two rules, and guessing is how a rule ends up
	// meaning something the operator did not ask for.
	t.Run("any protocol is refused", func(t *testing.T) {
		if _, err := FirewallCommand("firewalld", "allow", parse("allow", "80", "any", "any")); err == nil {
			t.Fatal("firewalld accepted a protocol-less rule")
		}
	})
}

// Every helper command is resolved from a fixed list before anything runs, so a
// missing backend is a refusal that changes nothing rather than a half-applied
// rule (firewalld's rule written, its reload missing).
// The resolvers only ever answer with a path from their own fixed list, so a
// name outside it is refused rather than searched for on PATH.
func TestTheResolversOnlyKnowTheirOwnList(t *testing.T) {
	for _, binary := range []string{"nft", "iptables", "sh", "systemctl ", "/bin/sh", ""} {
		if _, err := FindFirewallBinary(binary); err == nil {
			t.Fatalf("FindFirewallBinary(%q) resolved", binary)
		}
	}
	for _, manager := range []string{"nix", "apt", "brew", "sh", "/usr/bin/apt-get", ""} {
		if _, err := FindManager(manager); err == nil {
			t.Fatalf("FindManager(%q) resolved", manager)
		}
	}
	// A name that is on the list either resolves to an absolute path or is
	// absent from this host; it never resolves to a relative one.
	for _, binary := range []string{"ufw", "firewall-cmd", "systemctl"} {
		if path, err := FindFirewallBinary(binary); err == nil && !filepath.IsAbs(path) {
			t.Fatalf("FindFirewallBinary(%q) = %q, want an absolute path", binary, path)
		}
	}
}

func TestParseArgsAcceptsTheNewCommands(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		ok   bool
	}{
		{"packages refresh", []string{"packages", "refresh"}, true},
		{"packages upgrade", []string{"packages", "upgrade"}, true},
		{"packages install", []string{"packages", "install", "nginx"}, true},
		{"packages remove", []string{"packages", "remove", "nginx"}, true},
		{"packages install without a name", []string{"packages", "install"}, false},
		{"packages refresh with a name", []string{"packages", "refresh", "nginx"}, false},
		{"packages unknown verb", []string{"packages", "purge", "nginx"}, false},
		{"packages no verb", []string{"packages"}, false},
		{"firewall enable", []string{"firewall", "enable"}, true},
		{"firewall disable", []string{"firewall", "disable"}, true},
		{"firewall allow", []string{"firewall", "allow", "80", "tcp", "any"}, true},
		{"firewall deny", []string{"firewall", "deny", "80", "tcp", "any"}, true},
		{"firewall delete", []string{"firewall", "delete", "allow", "80", "tcp", "any"}, true},
		{"firewall allow short", []string{"firewall", "allow", "80", "tcp"}, false},
		{"firewall allow long", []string{"firewall", "allow", "80", "tcp", "any", "extra"}, false},
		{"firewall delete short", []string{"firewall", "delete", "allow", "80", "tcp"}, false},
		{"firewall enable with arguments", []string{"firewall", "enable", "80"}, false},
		{"firewall unknown verb", []string{"firewall", "flush"}, false},
		// An appended argument is not a reinterpretation.
		{"packages with a trailing option", []string{"packages", "install", "nginx", "-y"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseArgs(tc.argv)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseArgs(%q) = %v, want ok=%v", tc.argv, err, tc.ok)
			}
		})
	}
}

// rootOwnedProfile relaxes the root-ownership requirement for the duration of
// one test. The check is the point of the file in production and is covered in
// [TestLoadSetRefusesUntrustedProfileFile]; here it would only mask the grant
// logic under test, because a test cannot create a root-owned file.
func rootOwnedProfile(t *testing.T) {
	t.Helper()
	previous := runOptions
	runOptions = Options{}
	t.Cleanup(func() { runOptions = previous })
}

// stubResolver points the runners at a stand-in binary, which records the argv
// it was given. The real resolvers only accept absolute paths from a fixed
// list, and a test cannot install into /usr/bin.
func stubResolver(t *testing.T, log string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"manager", "firewall"} {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexit 0\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	managerPath := filepath.Join(dir, "manager")
	previousManager, previousFirewall := findManager, findFirewallCommand
	findManager = func(string) (string, error) { return managerPath, nil }
	findFirewallCommand = func(string) (string, error) { return filepath.Join(dir, "firewall"), nil }
	t.Cleanup(func() { findManager, findFirewallCommand = previousManager, previousFirewall })
}

func TestRunPackagesExecutesTheGrantedVerb(t *testing.T) {
	rootOwnedProfile(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubResolver(t, log)
	path := writeProfile(t, "[packages]\nmanager = \"apt\"\nverbs = [\"install\"]\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "packages", "install", "nginx"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0, Invoker: "maidcafe"})
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// argv[0] is the resolved stand-in; the rest is the manager's own shape.
	if got := strings.TrimSpace(string(argv)); got != "-y install -- nginx" {
		t.Fatalf("argv = %q", got)
	}
	// The audit line names the package and the manager, so a privileged install
	// is attributable without the caller's own logs.
	entry := lastEntry(t, stderr.String())
	if entry["package"] != "nginx" || entry["manager"] != "apt" || entry["ok"] != true {
		t.Fatalf("audit entry = %v", entry)
	}
}

func TestRunPackagesRefusesUngrantedAndUnconfigured(t *testing.T) {
	rootOwnedProfile(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubResolver(t, log)
	path := writeProfile(t, "[packages]\nmanager = \"apt\"\nverbs = [\"refresh\"]\n")

	var stdout, stderr bytes.Buffer
	// The verb is real but not granted.
	code := Run([]string{"--config", path, "packages", "upgrade"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0})
	if code != ExitRefused {
		t.Fatalf("ungranted verb exit = %d, want %d", code, ExitRefused)
	}
	if !strings.Contains(stderr.String(), "not granted") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	// A file with no package grant at all.
	other := writeProfile(t, "[[profiles]]\nname = \"nginx\"\npath = \""+t.TempDir()+"\"\nmodes = [\"0644\"]\n")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"--config", other, "packages", "refresh"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0})
	if code != ExitRefused || !strings.Contains(stderr.String(), "no [packages] grant") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a refused invocation ran something")
	}
}

// The helper refuses before it elevates: running it unprivileged means the
// sudoers rule is missing, which is an installation problem with its own exit
// code, not a policy refusal.
func TestRunPackagesAndFirewallRefuseUnprivileged(t *testing.T) {
	rootOwnedProfile(t)
	path := writeProfile(t, "[packages]\nmanager = \"apt\"\nverbs = [\"install\"]\n\n[firewall]\nbackend = \"ufw\"\nverbs = [\"allow\"]\n")
	for _, argv := range [][]string{
		{"--config", path, "packages", "install", "nginx"},
		{"--config", path, "firewall", "allow", "80", "tcp", "any"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(argv, strings.NewReader(""), &stdout, &stderr, Env{EUID: 1000}); code != ExitNotRoot {
			t.Fatalf("%q exit = %d, want %d", argv, code, ExitNotRoot)
		}
	}
}

func TestRunFirewallExecutesTheRule(t *testing.T) {
	rootOwnedProfile(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubResolver(t, log)
	path := writeProfile(t, "[firewall]\nbackend = \"ufw\"\nverbs = [\"allow\", \"delete\"]\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "firewall", "allow", "443", "tcp", "10.0.0.0/8"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0})
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(argv)); got != "allow from 10.0.0.0/8 to any port 443 proto tcp" {
		t.Fatalf("argv = %q", got)
	}
	entry := lastEntry(t, stderr.String())
	if entry["backend"] != "ufw" || entry["rule"] != "allow 443/tcp from 10.0.0.0/8" {
		t.Fatalf("audit entry = %v", entry)
	}
}

// A delete has to name the action, because ufw identifies a rule by its text:
// deleting an allow and deleting a deny are different rules.
func TestRunFirewallDeleteNamesTheAction(t *testing.T) {
	rootOwnedProfile(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubResolver(t, log)
	path := writeProfile(t, "[firewall]\nbackend = \"ufw\"\nverbs = [\"delete\"]\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "firewall", "delete", "deny", "22", "tcp", "any"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0})
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(argv)); got != "--force delete deny 22/tcp" {
		t.Fatalf("argv = %q", got)
	}
}

// A rule that the backend cannot express is refused rather than silently
// turned into a different rule.
func TestRunFirewallRefusesAnUnrepresentableRule(t *testing.T) {
	rootOwnedProfile(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubResolver(t, log)
	path := writeProfile(t, "[firewall]\nbackend = \"firewalld\"\nverbs = [\"allow\"]\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "firewall", "allow", "80", "any", "any"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0})
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d", code, ExitRefused)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a refused rule ran something")
	}
}

// A failure downstream is reported as itself, not as a policy refusal, and the
// helper's own exit codes stay out of the way.
func TestRunExternalMapsFailures(t *testing.T) {
	rootOwnedProfile(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "failing")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := writeProfile(t, "[packages]\nmanager = \"apt\"\nverbs = [\"refresh\"]\n")
	previous := findManager
	findManager = func(string) (string, error) { return path, nil }
	t.Cleanup(func() { findManager = previous })

	var stdout, stderr bytes.Buffer
	// exit 3 is the command's own; the helper must not report it as its own
	// refusal code, which would read as a policy answer.
	if code := Run([]string{"--config", config, "packages", "refresh"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0}); code != ExitIO {
		t.Fatalf("exit = %d, want %d", code, ExitIO)
	}
	entry := lastEntry(t, stderr.String())
	if entry["ok"] != false || entry["error"] == "" {
		t.Fatalf("audit entry = %v", entry)
	}
}

func TestRunProfilesListsTheNewGrants(t *testing.T) {
	path := writeProfile(t, "[packages]\nmanager = \"dnf\"\nverbs = [\"upgrade\", \"refresh\"]\n\n[firewall]\nbackend = \"firewalld\"\nverbs = [\"allow\", \"delete\"]\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--config", path, "fs", "profiles"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 501}); code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	// Sorting makes two runs print the same text.
	if want := "refresh,upgrade"; !strings.Contains(stdout.String(), "packages\tdnf\tservice\t"+want) {
		t.Fatalf("stdout = %q, want the package grant with verbs %q", stdout.String(), want)
	}
	if want := "allow,delete"; !strings.Contains(stdout.String(), "firewall\tfirewalld\tservice\t"+want) {
		t.Fatalf("stdout = %q, want the firewall grant with verbs %q", stdout.String(), want)
	}
}

func TestSudoersRuleCoversTheNewCommands(t *testing.T) {
	rule := SudoersRule("maidcafe", "/usr/local/libexec/maidkit-priv")
	for _, want := range []string{
		"maidcafe ALL=(root) NOPASSWD: /usr/local/libexec/maidkit-priv packages *",
		"maidcafe ALL=(root) NOPASSWD: /usr/local/libexec/maidkit-priv firewall *",
	} {
		if !strings.Contains(rule, want) {
			t.Fatalf("rule is missing %q:\n%s", want, rule)
		}
	}
}

// lastEntry returns the JSON audit object on the last stderr line. A refusal
// writes its line before the command's own message would appear, so the entry
// is found by shape rather than by position.
func lastEntry(t *testing.T, stderr string) map[string]any {
	t.Helper()
	for _, line := range reverseLines(stderr) {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["event"] == "privfs" {
			return entry
		}
	}
	t.Fatalf("no audit entry in %q", stderr)
	return nil
}

func reverseLines(text string) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}
