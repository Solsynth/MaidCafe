package daemon

import (
	"path/filepath"
	"testing"
)

func TestWatchedProcessStoreSeedsFromConfigAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watched.json")
	store := newWatchedProcessStore(path, []string{"nginx", "postgres"})
	if got := store.List(); len(got) != 2 || got[0] != "nginx" || got[1] != "postgres" {
		t.Fatalf("seeded list = %#v", got)
	}
	// A fresh store reads the persisted file (authoritative over config).
	store2 := newWatchedProcessStore(path, []string{"redis"})
	if got := store2.List(); len(got) != 2 || got[0] != "nginx" || got[1] != "postgres" {
		t.Fatalf("persisted list = %#v", got)
	}
}

func TestWatchedProcessStoreAddRemove(t *testing.T) {
	store := newWatchedProcessStore(filepath.Join(t.TempDir(), "watched.json"), nil)
	if got, err := store.Add("nginx"); err != nil || len(got) != 1 {
		t.Fatalf("add = %#v, %v", got, err)
	}
	// Duplicate add is a no-op.
	if got, err := store.Add("nginx"); err != nil || len(got) != 1 {
		t.Fatalf("duplicate add = %#v, %v", got, err)
	}
	if got, err := store.Add("redis"); err != nil || len(got) != 2 || got[0] != "nginx" || got[1] != "redis" {
		t.Fatalf("second add = %#v, %v", got, err)
	}
	// Unknown remove is a no-op.
	if got, err := store.Remove("bogus"); err != nil || len(got) != 2 {
		t.Fatalf("unknown remove = %#v, %v", got, err)
	}
	if got, err := store.Remove("nginx"); err != nil || len(got) != 1 || got[0] != "redis" {
		t.Fatalf("remove = %#v, %v", got, err)
	}
}

func TestWatchedProcessStoreNormalizesInvalidNames(t *testing.T) {
	store := newWatchedProcessStore("", []string{"bad name!", "nginx", "", "..", "ok_name"})
	got := store.List()
	if len(got) != 2 || got[0] != "nginx" || got[1] != "ok_name" {
		t.Fatalf("normalized list = %#v", got)
	}
}
