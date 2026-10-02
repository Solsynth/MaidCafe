package privfs

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeProfile writes a profile file the loader can read. The ownership
// requirement is disabled in these tests: a test cannot create root-owned
// files, and the ownership check itself is covered where it belongs, in
// [TestLoadSetRefusesGroupWritable].
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "priv.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseMode(t *testing.T) {
	allowed := map[string]fs.FileMode{"640": 0o640, "644": 0o644, "600": 0o600, "750": 0o750, "755": 0o755, "700": 0o700, "0644": 0o644}
	for raw, want := range allowed {
		got, err := ParseMode(raw)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %v, %v", raw, got, err)
		}
	}
	// Setuid, setgid, sticky and group/other write are outside the permitted
	// set whatever their digit count, because the helper creates files root
	// executes and files other accounts can rewrite.
	for _, raw := range []string{"4755", "2755", "1777", "777", "666", "7777", "", "abc", "6444", "0o644"} {
		if _, err := ParseMode(raw); err == nil {
			t.Fatalf("ParseMode(%q) was accepted", raw)
		}
	}
}

func TestParseArgsIsPositionalAndExact(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		ok   bool
	}{
		{"write", []string{"fs", "write", "nginx", "sites/a.conf", "0644"}, true},
		{"mkdir", []string{"fs", "mkdir", "nginx", "snippets", "0755"}, true},
		{"remove", []string{"fs", "remove", "nginx", "sites/a.conf"}, true},
		{"profiles", []string{"fs", "profiles"}, true},
		{"sudoers default user", []string{"sudoers"}, true},
		{"sudoers named user", []string{"sudoers", "alice"}, true},
		{"version", []string{"version"}, true},
		{"config first", []string{"--config", "/tmp/x.toml", "fs", "remove", "p", "r"}, true},
		{"no command", nil, false},
		{"unknown command", []string{"explode"}, false},
		{"unknown fs verb", []string{"fs", "chown", "p", "r"}, false},
		{"write missing mode", []string{"fs", "write", "p", "r"}, false},
		{"write extra argument", []string{"fs", "write", "p", "r", "0644", "extra"}, false},
		{"remove missing path", []string{"fs", "remove", "p"}, false},
		{"remove with mode", []string{"fs", "remove", "p", "r", "0644"}, false},
		{"profiles with argument", []string{"fs", "profiles", "p"}, false},
		// A wildcard in the sudoers rule lets a caller append anything; the
		// helper's answer is a refusal, never a reinterpretation.
		{"trailing flag rejected", []string{"fs", "remove", "p", "r", "--root", "/"}, false},
		{"flag after command rejected", []string{"fs", "--config", "/tmp/x", "remove", "p", "r"}, false},
		{"duplicate config rejected", []string{"--config", "/a", "--config", "/b", "fs", "remove", "p", "r"}, false},
		{"config without value", []string{"--config"}, false},
		{"config without command", []string{"--config", "/tmp/x.toml"}, false},
		{"sudoers too many", []string{"sudoers", "a", "b"}, false},
		{"version with argument", []string{"version", "x"}, false},
		{"empty profile", []string{"fs", "remove", "  ", "r"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := ParseArgs(tc.argv)
			if tc.ok && err != nil {
				t.Fatalf("expected valid argv, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected refusal for %v (parsed %+v)", tc.argv, action)
			}
		})
	}
	// --config must land on the action, not be silently ignored.
	action, err := ParseArgs([]string{"--config", "/tmp/x.toml", "fs", "remove", "p", "r"})
	if err != nil || action.ConfigPath != "/tmp/x.toml" {
		t.Fatalf("config path = %q (%v)", action.ConfigPath, err)
	}
	// The default user is what the installer writes.
	if action, err := ParseArgs([]string{"sudoers"}); err != nil || action.User != "maidcafe" {
		t.Fatalf("default sudoers user = %q (%v)", action.User, err)
	}
}

func TestLoadSetValidatesEveryEntry(t *testing.T) {
	root := t.TempDir()
	good := writeProfile(t, `
[[profiles]]
name = "nginx"
path = "`+root+`"
modes = ["0644", "0640"]

[[profiles]]
name = "units"
path = "`+root+`"
modes = ["0644"]
`)
	set, err := LoadSet(good, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Join(set.Names(), ","); names != "nginx,units" {
		t.Fatalf("names = %q", names)
	}
	profile, err := set.Lookup("nginx")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Path == "" || !profile.Modes[0o644] || !profile.Modes[0o640] || profile.Modes[0o755] {
		t.Fatalf("profile = %+v", profile)
	}
	if _, err := set.Lookup("absent"); err == nil || !strings.Contains(err.Error(), "nginx") {
		t.Fatalf("unknown profile error = %v", err)
	}

	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"no profiles", "# nothing here\n"},
		{"bad name", "[[profiles]]\nname = \"Nginx\"\npath = \"" + root + "\"\nmodes = [\"0644\"]\n"},
		{"relative path", "[[profiles]]\nname = \"a\"\npath = \"etc/nginx\"\nmodes = [\"0644\"]\n"},
		{"missing path", "[[profiles]]\nname = \"a\"\npath = \"/does/not/exist-xyz\"\nmodes = [\"0644\"]\n"},
		{"path is a file", "[[profiles]]\nname = \"a\"\npath = \"FILE\"\nmodes = [\"0644\"]\n"},
		{"no modes", "[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\n"},
		{"forbidden mode", "[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\nmodes = [\"0666\"]\n"},
		{"setuid mode", "[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\nmodes = [\"4755\"]\n"},
		{"duplicate name", "[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\nmodes = [\"0644\"]\n[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\nmodes = [\"0644\"]\n"},
	}
	file := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.ReplaceAll(tc.body, "FILE", file)
			path := writeProfile(t, body)
			if _, err := LoadSet(path, Options{}); err == nil {
				t.Fatalf("expected refusal for %s", tc.name)
			}
		})
	}
}

func TestLoadSetRefusesUntrustedProfileFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes and ownership are not available on Windows")
	}
	root := t.TempDir()
	body := "[[profiles]]\nname = \"a\"\npath = \"" + root + "\"\nmodes = [\"0644\"]\n"

	// Group- or other-writable: another account could rewrite the privileges.
	loose := filepath.Join(t.TempDir(), "loose.toml")
	if err := os.WriteFile(loose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The create mode is filtered by the umask, so the loose mode is set
	// explicitly; otherwise the test would pass on a host whose umask happens
	// to strip exactly the bits it is checking.
	if err := os.Chmod(loose, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(loose, Options{}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("group-writable profile accepted: %v", err)
	}

	// A symlink could be repointed at a file the daemon can write.
	link := filepath.Join(t.TempDir(), "link.toml")
	if err := os.Symlink(loose, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(link, Options{}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked profile accepted: %v", err)
	}

	// A directory is not a profile file.
	if _, err := LoadSet(t.TempDir(), Options{}); err == nil {
		t.Fatal("directory accepted as a profile file")
	}

	// The ownership requirement is what production turns on; a test cannot
	// create a root-owned file, so this only asserts the check is wired.
	clean := writeProfile(t, body)
	if _, err := LoadSet(clean, Options{RequireRootOwner: true}); err == nil {
		if os.Geteuid() != 0 {
			t.Fatal("RequireRootOwner accepted a file not owned by root")
		}
	}
}

func TestSetWriteRoundtripAndMode(t *testing.T) {
	root := t.TempDir()
	path := writeProfile(t, "[[profiles]]\nname = \"p\"\npath = \""+root+"\"\nmodes = [\"0644\", \"0755\"]\n")
	set, err := LoadSet(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := set.Lookup("p")

	if err := set.Write(profile, "app.conf", 0o644, []byte("first")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "app.conf"))
	if err != nil || string(content) != "first" {
		t.Fatalf("content = %q (%v)", content, err)
	}
	info, err := os.Stat(filepath.Join(root, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %04o", info.Mode().Perm())
	}

	// Replacing an existing file must not leave a temporary behind.
	if err := set.Write(profile, "app.conf", 0o644, []byte("second")); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(filepath.Join(root, "app.conf"))
	if string(content) != "second" {
		t.Fatalf("replaced content = %q", content)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries after a replace", len(entries))
	}

	// A mode the profile does not grant is refused, not downgraded.
	err = set.Write(profile, "script.sh", 0o700, []byte("#!/bin/sh\n"))
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("ungranted mode = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "script.sh")); statErr == nil {
		t.Fatal("a refused write created a file")
	}

	if err := set.Mkdir(profile, "sites/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	nested, err := os.Stat(filepath.Join(root, "sites", "nested"))
	if err != nil || !nested.IsDir() || nested.Mode().Perm() != 0o755 {
		t.Fatalf("mkdir result: %v %v", nested, err)
	}

	if err := set.Remove(profile, "app.conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "app.conf")); !os.IsNotExist(err) {
		t.Fatal("remove left the file behind")
	}
}

func TestSetRefusesEscapesAndDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeProfile(t, "[[profiles]]\nname = \"p\"\npath = \""+root+"\"\nmodes = [\"0644\"]\n")
	set, err := LoadSet(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := set.Lookup("p")

	// The requested path is inside the profile, but it resolves outside it: the
	// kernel-enforced root is what refuses this, not the string check.
	if err := set.Write(profile, "escape/planted", 0o644, []byte("owned")); err == nil {
		t.Fatal("a symlink escaped the profile")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("a file was created outside the profile")
	}

	for _, rel := range []string{"", ".", "..", "../escape", "a/../../b", "/etc/passwd", "a\x00b"} {
		if err := set.Write(profile, rel, 0o644, []byte("x")); err == nil {
			t.Fatalf("write accepted %q", rel)
		}
		if err := set.Remove(profile, rel); err == nil && rel != ".." {
			t.Fatalf("remove accepted %q", rel)
		}
	}

	// Directories are not removable: a recursive delete is not something a
	// profile should authorize.
	if err := set.Remove(profile, "adir"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory removal = %v", err)
	}
	// A destination that is a directory is refused for writes too.
	if err := set.Write(profile, "adir", 0o644, []byte("x")); err == nil {
		t.Fatal("a write replaced a directory")
	}
}

func TestSetWriteRejectsContentOverLimit(t *testing.T) {
	root := t.TempDir()
	path := writeProfile(t, "[[profiles]]\nname = \"p\"\npath = \""+root+"\"\nmodes = [\"0644\"]\n")
	set, err := LoadSet(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := set.Lookup("p")
	if err := set.Write(profile, "big", 0o644, bytes.Repeat([]byte("a"), MaxContentBytes+1)); err == nil {
		t.Fatal("oversized content accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "big")); err == nil {
		t.Fatal("oversized content left a file")
	}
}

func TestRunExitCodesAndAuditLine(t *testing.T) {
	root := t.TempDir()
	path := writeProfile(t, "[[profiles]]\nname = \"p\"\npath = \""+root+"\"\nmodes = [\"0644\"]\n")

	// Unprivileged invocation is an installation problem, and it says so.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "fs", "write", "p", "a", "0644"}, strings.NewReader("x"), &stdout, &stderr, Env{EUID: 501})
	if code != ExitNotRoot {
		t.Fatalf("unprivileged write exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "sudoers") {
		t.Fatalf("unprivileged message = %q", stderr.String())
	}

	// Usage errors are their own exit code.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"fs", "write"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 0}); code != ExitUsage {
		t.Fatalf("usage exit = %d", code)
	}

	// Reading is allowed without root and prints the configured profiles.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--config", path, "fs", "profiles"}, strings.NewReader(""), &stdout, &stderr, Env{EUID: 501}); code != ExitOK {
		t.Fatalf("profiles exit = %d (%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "p\t") || !strings.Contains(stdout.String(), "\t0644") {
		t.Fatalf("profiles output = %q", stdout.String())
	}

	// Running as root, the profile file must be root-owned; a test writes files
	// as its own user, so this is the refusal path unless the test itself runs
	// as root. Either way the exit code and the audit line must agree with what
	// happened — the write itself is covered by TestSetWriteRoundtripAndMode.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"--config", path, "fs", "write", "p", "app.conf", "0644"}, strings.NewReader("x"), &stdout, &stderr, Env{EUID: 0})
	wantCode, wantOK := ExitConfigBad, false
	if os.Geteuid() == 0 {
		wantCode, wantOK = ExitOK, true
	}
	if code != wantCode {
		t.Fatalf("root write exit = %d, want %d (%s)", code, wantCode, stderr.String())
	}
	assertAuditLine(t, stderr.String(), wantOK, "write")
}

// assertAuditLine checks that stderr carries the JSON audit record with the
// expected outcome.
func assertAuditLine(t *testing.T, stderr string, ok bool, verb string) {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		if entry.Event != "privfs" || entry.Verb != verb || entry.OK != ok {
			t.Fatalf("audit entry = %+v", entry)
		}
		return
	}
	t.Fatalf("no audit line in %q", stderr)
}

func TestSudoersRuleNamesTheBoundary(t *testing.T) {
	rule := SudoersRule("maidcafe", DefaultHelperPath)
	for _, verb := range []string{"write", "mkdir", "remove"} {
		want := "maidcafe ALL=(root) NOPASSWD: " + DefaultHelperPath + " fs " + verb + " *"
		if !strings.Contains(rule, want) {
			t.Fatalf("rule is missing %q:\n%s", want, rule)
		}
	}
	if !strings.Contains(rule, DefaultConfigPath) {
		t.Fatalf("rule does not name the boundary file:\n%s", rule)
	}
	// sudoers needs a trailing newline on every line.
	if !strings.HasSuffix(rule, "\n") {
		t.Fatal("rule does not end with a newline")
	}
}

func TestDigestIsStable(t *testing.T) {
	first, second := Digest([]byte("payload")), Digest([]byte("payload"))
	if first != second || len(first) != 64 {
		t.Fatalf("Digest = %q / %q", first, second)
	}
	if Digest([]byte("other")) == first {
		t.Fatal("Digest collided")
	}
}

func TestSafeRelNormalizes(t *testing.T) {
	if rel, err := safeRel("a//b/./c"); err != nil || rel != filepath.Join("a", "b", "c") {
		t.Fatalf("safeRel = %q, %v", rel, err)
	}
}
