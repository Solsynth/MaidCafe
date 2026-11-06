package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

func TestTerminalResolveTmuxName(t *testing.T) {
	manager := newTerminalManager(nil, nil)
	policy := config.TerminalConfig{Shells: []string{"/bin/bash"}}
	for _, tc := range []struct {
		name    string
		tmux    string
		wantErr error
	}{
		{name: "no tmux", tmux: ""},
		{name: "valid name", tmux: "maidkit-x"},
		{name: "leading digit", tmux: "1abc"},
		{name: "dot and underscore", tmux: "a.b_c-1"},
		{name: "full length", tmux: strings.Repeat("a", 40)},
		{name: "rejects a dash first", tmux: "-x", wantErr: errTerminalTmuxInvalid},
		{name: "rejects 41 characters", tmux: strings.Repeat("a", 41), wantErr: errTerminalTmuxInvalid},
		{name: "rejects a space", tmux: "two words", wantErr: errTerminalTmuxInvalid},
		{name: "rejects a semicolon", tmux: "a;b", wantErr: errTerminalTmuxInvalid},
		{name: "rejects a colon", tmux: "a:b", wantErr: errTerminalTmuxInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manager.Resolve(policy, terminalRequest{Shell: "/bin/bash", Tmux: tc.tmux})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Resolve(tmux=%q) error = %v; want %v", tc.tmux, err, tc.wantErr)
			}
		})
	}
}

// stubTmux puts a fake `tmux` first on PATH and reports the argv it was called
// with.
func stubTmux(t *testing.T, exitCode int, stdout, stderr string) *string {
	t.Helper()
	dir := t.TempDir()
	recorded := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + recorded + "\n"
	if stdout != "" {
		script += "printf '%s' " + shellQuote(stdout) + "\n"
	}
	if stderr != "" {
		script += "printf '%s' " + shellQuote(stderr) + " >&2\n"
	}
	script += "exit " + itoa(exitCode) + "\n"
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing tmux stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &recorded
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

// normalizeTmuxBase replaces the resolved tmux path with bare "tmux", so an
// assertion compares the argv shape rather than this machine's temp directory.
func normalizeTmuxBase(argv []string) []string {
	normalized := make([]string, len(argv))
	for i, arg := range argv {
		if strings.HasSuffix(arg, string(os.PathSeparator)+"tmux") {
			arg = "tmux"
		}
		normalized[i] = arg
	}
	return normalized
}

func TestBuildTerminalCommandTmux(t *testing.T) {
	stubTmux(t, 0, "", "")
	policy := config.TerminalConfig{Shells: []string{"/bin/bash"}}

	cmd, err := buildTerminalCommand(policy, terminalRequest{Shell: "/bin/bash", Tmux: "maidkit-x"})
	if err != nil {
		t.Fatalf("buildTerminalCommand: %v", err)
	}
	want := []string{"tmux", "-CC", "new-session", "-A", "-s", "maidkit-x"}
	if got := normalizeTmuxBase(cmd.Args); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v; want %v", got, want)
	}

	withCwd, err := buildTerminalCommand(
		config.TerminalConfig{Shells: []string{"/bin/bash"}, Cwd: "/srv/app"},
		terminalRequest{Shell: "/bin/bash", Tmux: "maidkit-x"},
	)
	if err != nil {
		t.Fatalf("buildTerminalCommand: %v", err)
	}
	wantWithCwd := []string{"tmux", "-CC", "new-session", "-A", "-s", "maidkit-x", "-c", "/srv/app"}
	if got := normalizeTmuxBase(withCwd.Args); !reflect.DeepEqual(got, wantWithCwd) {
		t.Fatalf("argv = %v; want %v", got, wantWithCwd)
	}

	wrapped, err := buildTerminalCommand(policy, terminalRequest{Shell: "/bin/bash", User: "deploy", Tmux: "maidkit-x"})
	if err != nil {
		t.Fatalf("buildTerminalCommand: %v", err)
	}
	if len(wrapped.Args) < 5 || !reflect.DeepEqual(wrapped.Args[:4], []string{"sudo", "-H", "-u", "deploy"}) {
		t.Fatalf("argv = %v; want a sudo wrapper", wrapped.Args)
	}
	if got := normalizeTmuxBase(wrapped.Args); !reflect.DeepEqual(got[len(got)-6:], want) {
		t.Fatalf("argv = %v; want it to end with %v", got, want)
	}
}

func TestBuildTerminalCommandShellUnchanged(t *testing.T) {
	cmd, err := buildTerminalCommand(
		config.TerminalConfig{Shells: []string{"/bin/bash"}, Cwd: "/srv"},
		terminalRequest{Shell: "/bin/bash"},
	)
	if err != nil {
		t.Fatalf("buildTerminalCommand: %v", err)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"/bin/bash", "-l"}) {
		t.Fatalf("argv = %v; want the login shell", cmd.Args)
	}
	if cmd.Dir != "/srv" {
		t.Fatalf("dir = %q; want /srv", cmd.Dir)
	}
}

func TestBuildTerminalCommandTmuxMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := buildTerminalCommand(
		config.TerminalConfig{Shells: []string{"/bin/bash"}},
		terminalRequest{Shell: "/bin/bash", Tmux: "maidkit-x"},
	)
	if !errors.Is(err, errTerminalTmuxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errTerminalTmuxUnavailable)
	}
}

func TestListTmuxSessionsParsesRows(t *testing.T) {
	recorded := stubTmux(t, 0, "a\t2\t1700000000\t1\nb\t1\t1700000100\t0\n", "")
	sessions, installed, err := listTmuxSessions(context.Background(), config.TerminalConfig{}, "")
	if err != nil {
		t.Fatalf("listTmuxSessions: %v", err)
	}
	if !installed {
		t.Fatal("installed = false; want true")
	}
	want := []tmuxSessionView{
		{Name: "a", Windows: 2, Created: 1700000000, Attached: 1},
		{Name: "b", Windows: 1, Created: 1700000100, Attached: 0},
	}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions = %v; want %v", sessions, want)
	}
	argv, err := os.ReadFile(*recorded)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	if !strings.Contains(string(argv), "list-sessions -F "+tmuxListFormat) {
		t.Fatalf("argv = %q; want the listing format", string(argv))
	}
}

func TestListTmuxSessionsNoServer(t *testing.T) {
	stubTmux(t, 1, "", "no server running on /tmp/tmux-1000/default\n")
	sessions, installed, err := listTmuxSessions(context.Background(), config.TerminalConfig{}, "")
	if err != nil {
		t.Fatalf("listTmuxSessions: %v", err)
	}
	if !installed || len(sessions) != 0 {
		t.Fatalf("installed = %v, sessions = %v; want true, none", installed, sessions)
	}
}

func TestListTmuxSessionsWithoutTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	sessions, installed, err := listTmuxSessions(context.Background(), config.TerminalConfig{}, "")
	if err != nil {
		t.Fatalf("listTmuxSessions: %v", err)
	}
	if installed || sessions != nil {
		t.Fatalf("installed = %v, sessions = %v; want false, nil", installed, sessions)
	}
}

func TestParseTmuxSessionsSkipsBadRows(t *testing.T) {
	sessions := parseTmuxSessions("good\t1\t1700000000\t0\nbroken\nbad\tx\t1\t1\n\n")
	if len(sessions) != 1 || sessions[0].Name != "good" {
		t.Fatalf("sessions = %v; want only the good row", sessions)
	}
}

func TestKillTmuxSessionUsesName(t *testing.T) {
	recorded := stubTmux(t, 0, "", "")
	if err := killTmuxSession(context.Background(), config.TerminalConfig{}, "", "maidkit-x"); err != nil {
		t.Fatalf("killTmuxSession: %v", err)
	}
	argv, err := os.ReadFile(*recorded)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	if strings.TrimSpace(string(argv)) != "kill-session -t maidkit-x" {
		t.Fatalf("argv = %q; want kill-session -t maidkit-x", strings.TrimSpace(string(argv)))
	}
}
