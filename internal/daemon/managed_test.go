package daemon

import "testing"

func TestMatchManagedContainer(t *testing.T) {
	// Empty allowlist matches everything (backward-compatible).
	if !matchManagedContainer("id1", "web", "p", nil, nil) {
		t.Fatal("empty allowlist should match all")
	}
	// Exact name match.
	if !matchManagedContainer("id1", "web", "p", []string{"web"}, nil) {
		t.Fatal("exact name should match")
	}
	// ID prefix match.
	if !matchManagedContainer("abc123", "web", "p", []string{"abc"}, nil) {
		t.Fatal("id prefix should match")
	}
	// Compose match.
	if !matchManagedContainer("id1", "web", "myapp", nil, []string{"myapp"}) {
		t.Fatal("compose should match")
	}
	// Non-managed container does not match.
	if matchManagedContainer("id1", "web", "p", []string{"db"}, []string{"otherapp"}) {
		t.Fatal("unmanaged should not match")
	}
	// Empty/whitespace managed entries are skipped.
	if matchManagedContainer("id1", "web", "p", []string{"", "  "}, nil) {
		t.Fatal("empty managed entries should be skipped")
	}
}
