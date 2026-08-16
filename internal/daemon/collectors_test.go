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
