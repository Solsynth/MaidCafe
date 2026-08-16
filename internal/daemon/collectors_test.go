package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseProcessesHandlesFloatFieldsAndMultiWordCommand(t *testing.T) {
	input := "    1 root      0.1   0.2  12345 /sbin/init\n" +
		"  123 root      1.5   0.3  67890 nginx: worker process\n" +
		" 4567 jane     12.25  1.75 1048576 /usr/bin/python3 server.py --port 8080\n" +
		"garbage\n"
	procs := parseProcesses(input)
	if len(procs) != 3 {
		t.Fatalf("parsed %d processes, want 3: %#v", len(procs), procs)
	}
	if procs[0].PID != 1 || procs[0].User != "root" ||
		procs[0].CPUPercent != 0.1 || procs[0].MemoryPercent != 0.2 ||
		procs[0].RSSKb != 12345 || procs[0].Command != "/sbin/init" {
		t.Fatalf("first process parsed as %#v", procs[0])
	}
	if procs[1].PID != 123 || procs[1].Command != "nginx: worker process" {
		t.Fatalf("second process parsed as %#v", procs[1])
	}
	if procs[2].CPUPercent != 12.25 || procs[2].RSSKb != 1048576 ||
		procs[2].Command != "/usr/bin/python3 server.py --port 8080" {
		t.Fatalf("third process parsed as %#v", procs[2])
	}
}

func TestParseRuntimeProcessesGnuWithThreads(t *testing.T) {
	input := "  123 root      1.5   0.3  67890   45 java -Xmx2g -jar app.jar\n" +
		" 4567 jane     12.25  1.75 1048576   12 /usr/bin/python3 server.py --port 8080\n" +
		"garbage\n"
	procs := parseRuntimeProcesses(input, true)
	if len(procs) != 2 {
		t.Fatalf("parsed %d processes, want 2: %#v", len(procs), procs)
	}
	if procs[0].PID != 123 || procs[0].User != "root" ||
		procs[0].CPUPercent != 1.5 || procs[0].MemoryPercent != 0.3 ||
		procs[0].RSSKb != 67890 || procs[0].Threads == nil || *procs[0].Threads != 45 ||
		procs[0].Command != "java -Xmx2g -jar app.jar" {
		t.Fatalf("first process parsed as %#v", procs[0])
	}
	if procs[1].PID != 4567 || procs[1].Threads == nil || *procs[1].Threads != 12 ||
		procs[1].Command != "/usr/bin/python3 server.py --port 8080" {
		t.Fatalf("second process parsed as %#v", procs[1])
	}
}

func TestParseRuntimeProcessesBsdNoThreads(t *testing.T) {
	input := "  123 root      1.5   0.3  67890 java -Xmx2g -jar app.jar\n" +
		" 4567 jane     12.25  1.75 1048576 dotnet run --project app.csproj\n"
	procs := parseRuntimeProcesses(input, false)
	if len(procs) != 2 {
		t.Fatalf("parsed %d processes, want 2: %#v", len(procs), procs)
	}
	if procs[0].PID != 123 || procs[0].Threads != nil ||
		procs[0].Command != "java -Xmx2g -jar app.jar" {
		t.Fatalf("first process parsed as %#v", procs[0])
	}
	if procs[1].PID != 4567 || procs[1].Command != "dotnet run --project app.csproj" {
		t.Fatalf("second process parsed as %#v", procs[1])
	}
}

func TestParseRuntimeProcessesSkipsMalformed(t *testing.T) {
	input := "short\n" +
		"  123 root      1.5   0.3  67890   45 java -jar app.jar\n" +
		"  456 bad cpu 1.5 0.3 1000 5 nope\n" +
		"  789 root   abc   0.3  1000   5 java -version\n"
	procs := parseRuntimeProcesses(input, true)
	if len(procs) != 1 {
		t.Fatalf("parsed %d processes, want 1: %#v", len(procs), procs)
	}
	if procs[0].PID != 123 {
		t.Fatalf("process parsed as %#v", procs[0])
	}
}

func TestGroupRuntimeProcessesFixedOrderAndCaps(t *testing.T) {
	mk := func(pid int, cmd string) runtimeProcessEntry {
		return runtimeProcessEntry{PID: pid, Command: cmd}
	}
	groups := groupRuntimeProcesses([]runtimeProcessEntry{
		mk(1, "python3 server.py"),
		mk(2, "dotnet run"),
		mk(3, "java -jar app.jar"),
		mk(4, "python3 worker.py"),
	}, 1, []string{"java", "dotnet", "python"})
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	for i, want := range []string{"java", "dotnet", "python"} {
		if groups[i].Runtime != want {
			t.Fatalf("group[%d].Runtime = %q, want %q", i, groups[i].Runtime, want)
		}
	}
	if !groups[0].Available || len(groups[0].Processes) != 1 || groups[0].Processes[0].PID != 3 {
		t.Fatalf("java group = %#v", groups[0])
	}
	if !groups[1].Available || len(groups[1].Processes) != 1 || groups[1].Processes[0].PID != 2 {
		t.Fatalf("dotnet group = %#v", groups[1])
	}
	// python matches 3 rows but the cap is 1; the first (CPU order) wins.
	if !groups[2].Available || len(groups[2].Processes) != 1 || groups[2].Processes[0].PID != 1 {
		t.Fatalf("python group = %#v", groups[2])
	}
}

func TestGroupRuntimeProcessesHonorsConfiguredOrder(t *testing.T) {
	mk := func(pid int, cmd string) runtimeProcessEntry {
		return runtimeProcessEntry{PID: pid, Command: cmd}
	}
	groups := groupRuntimeProcesses([]runtimeProcessEntry{
		mk(1, "node server.js"),
		mk(2, "java -jar app.jar"),
	}, 50, []string{"node", "java"})
	if len(groups) != 2 || groups[0].Runtime != "node" || groups[1].Runtime != "java" {
		t.Fatalf("configured order not honored: %#v", groups)
	}
	if !groups[0].Available || groups[0].Processes[0].PID != 1 {
		t.Fatalf("node group = %#v", groups[0])
	}
	if !groups[1].Available || groups[1].Processes[0].PID != 2 {
		t.Fatalf("java group = %#v", groups[1])
	}
}

func TestGroupWatchedProcessesMatchesCommPrefixSorted(t *testing.T) {
	mk := func(pid int, cmd string) runtimeProcessEntry {
		return runtimeProcessEntry{PID: pid, Command: cmd}
	}
	groups := groupWatchedProcesses([]runtimeProcessEntry{
		mk(1, "nginx: master process"),
		mk(2, "postgres: checkpointer"),
		mk(3, "nginx: worker process"),
		mk(4, "java -jar app.jar"),
	}, 50, []string{"nginx", "postgres"})
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2: %#v", len(groups), groups)
	}
	nginx, postgres := groups[0], groups[1]
	if nginx.Name != "nginx" || !nginx.Available || len(nginx.Processes) != 2 {
		t.Fatalf("nginx group = %#v", nginx)
	}
	if postgres.Name != "postgres" || !postgres.Available || len(postgres.Processes) != 1 {
		t.Fatalf("postgres group = %#v", postgres)
	}
}

func TestGroupWatchedProcessesAbsentAndEmptyList(t *testing.T) {
	mk := func(pid int, cmd string) runtimeProcessEntry {
		return runtimeProcessEntry{PID: pid, Command: cmd}
	}
	groups := groupWatchedProcesses([]runtimeProcessEntry{
		mk(1, "nginx: master process"),
	}, 50, []string{"redis"})
	if len(groups) != 1 || groups[0].Available || groups[0].Error == nil {
		t.Fatalf("absent watcher = %#v", groups)
	}
	if groups := groupWatchedProcesses(nil, 50, nil); len(groups) != 0 {
		t.Fatalf("empty watcher list = %#v", groups)
	}
}

func TestParseJpsOutput(t *testing.T) {
	input := "12345 app.Main\n" +
		"67890\n" +
		"notapid garbage\n" +
		"99999 sun.tools.jps.Jps\n"
	jvms := parseJpsOutput(input)
	if len(jvms) != 3 {
		t.Fatalf("parsed %d jvms, want 3: %#v", len(jvms), jvms)
	}
	if jvms[0].PID != 12345 || jvms[0].MainClass != "app.Main" {
		t.Fatalf("first jvm parsed as %#v", jvms[0])
	}
	if jvms[1].PID != 67890 || jvms[1].MainClass != "" {
		t.Fatalf("second jvm parsed as %#v", jvms[1])
	}
	if jvms[2].PID != 99999 || jvms[2].MainClass != "sun.tools.jps.Jps" {
		t.Fatalf("third jvm parsed as %#v", jvms[2])
	}
}

func TestParseJstatGcutilOutput(t *testing.T) {
	output := "  S0     S1     E      O      M     CCS    YGC     YGCT    FGC    FGCT     GCT   \n" +
		"  0.00  57.14  45.00  23.40  95.20  90.00  12      0.400   0      0.000    0.400\n"
	oldPct, ygc, fgc, gct, err := parseJstatGcutilOutput(output)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if oldPct != 23.40 || ygc != 12 || fgc != 0 || gct != 0.400 {
		t.Fatalf("parsed (%v, %d, %d, %v)", oldPct, ygc, fgc, gct)
	}
	if _, _, _, _, err := parseJstatGcutilOutput("garbage\n"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestParseContainerLinesExtractsComposeProject(t *testing.T) {
	input := `{"Id":"abc123","Names":["web"],"Image":"nginx:1.25","State":"running","Status":"Up 2 hours","Labels":"com.docker.compose.project=myapp,maintainer=me"}` + "\n" +
		`{"ID":"def456","Names":["db"],"Image":"postgres:16","State":"exited","Status":"Exited (0) 3 hours ago","Labels":{"io.podman.compose.project":"stack","version":"1"}}` + "\n" +
		`{"Id":"ghi789","Names":null,"Image":"busybox","State":"created","Status":"","Labels":null}` + "\n"
	entries, err := parseContainerLines([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("parsed %d containers, want 3", len(entries))
	}
	first := entries[0]
	if first.ID != "abc123" || first.Name != "web" || first.Image != "nginx:1.25" ||
		first.State != "running" || first.Status != "Up 2 hours" || first.ComposeProject != "myapp" {
		t.Fatalf("first container parsed as %#v", first)
	}
	second := entries[1]
	if second.ID != "def456" || second.Name != "db" || second.ComposeProject != "stack" {
		t.Fatalf("second container parsed as %#v", second)
	}
	third := entries[2]
	if third.ID != "ghi789" || third.Name != "" || third.ComposeProject != "" {
		t.Fatalf("third container parsed as %#v", third)
	}
}

func TestParseContainerLabelsStringAndObjectForms(t *testing.T) {
	stringForm := parseContainerLabels([]byte(`"com.docker.compose.project=myapp,maintainer=me"`))
	if stringForm["com.docker.compose.project"] != "myapp" || stringForm["maintainer"] != "me" {
		t.Fatalf("string form parsed as %#v", stringForm)
	}
	objectForm := parseContainerLabels([]byte(`{"io.podman.compose.project":"stack","version":"1"}`))
	if objectForm["io.podman.compose.project"] != "stack" || objectForm["version"] != "1" {
		t.Fatalf("object form parsed as %#v", objectForm)
	}
	empty := parseContainerLabels(nil)
	if len(empty) != 0 {
		t.Fatalf("empty labels parsed as %#v", empty)
	}
}

func TestMergeSystemdUnitsIncludesEnabledButInactive(t *testing.T) {
	units := "UNIT LOAD ACTIVE SUB DESCRIPTION\n" +
		"nginx.service loaded active running A high performance web server\n" +
		"cron.service loaded active running Regular background program processing daemon\n" +
		"● postfix.service loaded active running Mail Transport Agent\n"
	files := "UNIT FILE STATE\n" +
		"nginx.service enabled\n" +
		"cron.service enabled\n" +
		"postfix.service enabled\n" +
		"backup.service disabled\n"
	merged := mergeSystemdUnits(units, files)
	if len(merged) != 4 {
		t.Fatalf("merged %d units, want 4: %#v", len(merged), merged)
	}
	byName := make(map[string]systemdUnit, len(merged))
	for _, unit := range merged {
		byName[unit.Name] = unit
	}
	backup, ok := byName["backup.service"]
	if !ok {
		t.Fatal("backup.service missing from merged units")
	}
	if backup.LoadState != "not-found" || backup.ActiveState != "inactive" ||
		backup.SubState != "dead" || backup.UnitFileState != "disabled" || backup.Description != "" {
		t.Fatalf("backup.service merged as %#v", backup)
	}
	nginx := byName["nginx.service"]
	if nginx.ActiveState != "active" || nginx.UnitFileState != "enabled" ||
		nginx.Description != "A high performance web server" {
		t.Fatalf("nginx.service merged as %#v", nginx)
	}
	postfix := byName["postfix.service"]
	if postfix.Description != "Mail Transport Agent" || postfix.UnitFileState != "enabled" {
		t.Fatalf("glyph-prefixed postfix.service merged as %#v", postfix)
	}
	// Sorted failed-first, then active, then inactive: the first three are
	// active, the file-only unit is last.
	for i, unit := range merged[:3] {
		if unit.ActiveState != "active" {
			t.Fatalf("merged[%d] = %s (%s), want active units first", i, unit.Name, unit.ActiveState)
		}
	}
	if merged[3].Name != "backup.service" {
		t.Fatalf("merged[3] = %s, want backup.service", merged[3].Name)
	}
}

func TestParseContainerLinesEmptyReturnsEmptySlice(t *testing.T) {
	entries, err := parseContainerLines(nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries == nil {
		t.Fatal("empty container list parsed as nil; must marshal to [] not null")
	}
	if len(entries) != 0 {
		t.Fatalf("parsed %d containers from empty input", len(entries))
	}
	if raw, marshalErr := json.Marshal(entries); marshalErr != nil || string(raw) != "[]" {
		t.Fatalf("empty container list marshals to %s, %v; want []", raw, marshalErr)
	}
}

func TestParseImageLinesDockerAndPodmanForms(t *testing.T) {
	input := `{"ID":"abc123def456","Repository":"nginx","Tag":"latest","Size":"192560829","Created":1730000000,"Digest":"<none>"}` + "\n" +
		`{"Id":"sha256:ffffffffffff","RepoTags":["docker.io/library/postgres:16","localhost/dev:edge"],"Size":98765432,"Created":1730000001,"Digest":"sha256:aaaa"}` + "\n" +
		`{"ID":"unused12345","Repository":"<none>","Tag":"<none>","Size":"123","Created":1730000002}` + "\n"
	entries, err := parseImageLines([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("parsed %d images, want 3", len(entries))
	}
	first := entries[0]
	if first.ID != "abc123def456" || len(first.Tags) != 1 || first.Tags[0] != "nginx:latest" ||
		first.Size != 192560829 || first.Created != 1730000000 || first.Digest != "" {
		t.Fatalf("docker image parsed as %#v", first)
	}
	second := entries[1]
	if second.ID != "sha256:ffffffffffff" || len(second.Tags) != 2 ||
		second.Tags[0] != "docker.io/library/postgres:16" || second.Tags[1] != "localhost/dev:edge" ||
		second.Size != 98765432 || second.Digest != "sha256:aaaa" {
		t.Fatalf("podman image parsed as %#v", second)
	}
	third := entries[2]
	if len(third.Tags) != 0 || third.Size != 123 {
		t.Fatalf("untagged image parsed as %#v", third)
	}
}

func TestParseImageLinesEmptyReturnsEmptySlice(t *testing.T) {
	entries, err := parseImageLines(nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries == nil || len(entries) != 0 {
		t.Fatalf("empty image list parsed as %#v; want non-nil empty slice", entries)
	}
	if raw, marshalErr := json.Marshal(entries); marshalErr != nil || string(raw) != "[]" {
		t.Fatalf("empty image list marshals to %s, %v; want []", raw, marshalErr)
	}
}

// TestIsPodmanDockerShim pins the docker-alias detection: distro
// podman-docker shims answer `docker --version` with podman's version
// string, while a real docker reports its own.
func TestIsPodmanDockerShim(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ctx := context.Background()

	shim := mk("docker", `printf '%s\n' 'podman version 4.9.0'`)
	if !isPodmanDockerShim(ctx, shim) {
		t.Fatal("podman shim not recognized")
	}
	real := mk("docker", `printf '%s\n' 'Docker version 26.1.3'`)
	if isPodmanDockerShim(ctx, real) {
		t.Fatal("real docker misdetected as podman shim")
	}
}

// TestProbeContainerRuntimesSkipsPodmanDockerShim pins the double-count
// guard: with real podman present, a podman `docker` shim is dropped from
// the probed runtime set so containers/images are listed once.
func TestProbeContainerRuntimesSkipsPodmanDockerShim(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mk("podman", `exit 0`)
	mk("docker", `printf '%s\n' 'podman version 4.9.0'`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	paths := probeContainerRuntimes(context.Background())
	if _, ok := paths["podman"]; !ok {
		t.Fatal("podman not probed")
	}
	if _, ok := paths["docker"]; ok {
		t.Fatal("podman docker shim reported as a separate docker runtime")
	}
}

// TestRunRuntimeListElevatedFallback pins the root-visibility retry: an
// empty direct listing is retried through `sudo -n` so containers/images
// owned by root stay visible to a non-root daemon with passwordless sudo,
// while a successful direct listing never escalates and a failed elevated
// retry is surfaced as an error instead of a misleading empty list.
func TestRunRuntimeListElevatedFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root test user to exercise the sudo retry")
	}
	dir := t.TempDir()
	mk := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Fake runtime: the "root-owned" container is only visible through the
	// elevated retry (env marker set by the fake sudo shim).
	runtimePath := mk("podman", `if [ "$MAIDCAFE_ROOT" = "1" ]; then echo '{"ID":"rootctr"}'; fi`)
	sudoMarker := filepath.Join(dir, "sudo-ran")
	mk("sudo", `shift; : > "`+sudoMarker+`"; MAIDCAFE_ROOT=1 exec "$@"`)
	mk("podman-visible", `echo '{"ID":"seenas-user"}'`)
	t.Setenv("PATH", dir)

	ctx := context.Background()

	// Direct success short-circuits the elevated retry.
	out, err := runRuntimeList(ctx, filepath.Join(dir, "podman-visible"), "ps")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("seenas-user")) {
		t.Fatalf("direct listing output = %q", out)
	}
	if _, statErr := os.Stat(sudoMarker); statErr == nil {
		t.Fatal("elevated retry ran although the direct listing succeeded")
	}

	// Empty direct listing retries through sudo and sees the root container.
	out, err = runRuntimeList(ctx, runtimePath, "ps")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("rootctr")) {
		t.Fatalf("elevated listing output = %q", out)
	}
	if _, statErr := os.Stat(sudoMarker); statErr != nil {
		t.Fatal("elevated retry did not run for an empty direct listing")
	}

	// Empty direct listing with a failing elevated retry surfaces the sudo
	// error instead of masking the empty direct result as "no containers".
	mk("sudo", `shift; exit 7`)
	out, err = runRuntimeList(ctx, runtimePath, "ps")
	if err == nil {
		t.Fatal("elevated retry failure was masked as a successful empty listing")
	}
	if len(out) != 0 {
		t.Fatalf("masked output = %q", out)
	}
}
