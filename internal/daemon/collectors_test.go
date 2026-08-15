package daemon

import (
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
