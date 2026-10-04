package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestContainerUpdatesServesTheCacheWithoutChecking asserts the batch endpoint
// answers from what the daemon already knows: a client paints badges from it,
// and only the per-container request may reach a registry.
func TestContainerUpdatesServesTheCacheWithoutChecking(t *testing.T) {
	t.Setenv("FAKE_RUNTIME_CALLS", filepath.Join(t.TempDir(), "calls"))
	fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))

	app, err := NewApp(detailTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)

	status, body := detailRequest(t, "http://"+app.ListenAddr(), "/api/v1/updates")
	if status != http.StatusOK {
		t.Fatalf("updates status = %d: %s", status, body)
	}
	var updates containerUpdatesPayload
	if err := json.Unmarshal(body, &updates); err != nil {
		t.Fatalf("updates body %s: %v", body, err)
	}
	if updates.IntervalSeconds != int64((6 * time.Hour).Seconds()) {
		t.Fatalf("updates interval = %d, want the configured cadence", updates.IntervalSeconds)
	}
	if len(updates.Containers) != 0 {
		t.Fatalf("updates should be empty before any check: %+v", updates.Containers)
	}
}

// TestContainerUpdatesForgetsAReplacedContainer pins the daemon half of the
// update badge. An update recreates a compose container under the same name
// with a new id, and a client matches a status to a container by name when the
// ids differ, so the predecessor's answer must not survive the read — or the
// badge the update was meant to clear stays lit on the container that replaced
// it (which is exactly what "the update hint remains" is).
func TestContainerUpdatesForgetsAReplacedContainer(t *testing.T) {
	t.Setenv("FAKE_RUNTIME_CALLS", filepath.Join(t.TempDir(), "calls"))
	fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))

	app, err := NewApp(detailTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)

	outdated := true
	// The container the daemon saw before the recreate: same name, an id the
	// host no longer has. The live one is what the fake runtime still lists.
	app.updateCheck.store(containerUpdateStatus{
		Container: "04160c5e60afaf970ab48c112b03f65af7096bf03e571c5944feb4d0257dd641",
		Name:      "web", Runtime: "podman", Image: "nginx:1.25",
		CheckedAt: time.Now().UTC(), Outdated: &outdated,
	})
	app.updateCheck.store(containerUpdateStatus{
		Container: containerID, Name: "web", Runtime: "podman", Image: "nginx:1.25",
		CheckedAt: time.Now().UTC(), Outdated: &outdated,
	})

	status, body := detailRequest(t, "http://"+app.ListenAddr(), "/api/v1/updates")
	if status != http.StatusOK {
		t.Fatalf("updates status = %d: %s", status, body)
	}
	var updates containerUpdatesPayload
	if err := json.Unmarshal(body, &updates); err != nil {
		t.Fatalf("updates body %s: %v", body, err)
	}
	if len(updates.Containers) != 1 || updates.Containers[0].Container != containerID {
		t.Fatalf("updates = %+v, want only the live container", updates.Containers)
	}
}

// stubRegistry serves one image manifest and counts the requests, so a test can
// assert both the digest comparison and that a cached result was reused.
func stubRegistry(t *testing.T, digest string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/manifests/1.25") {
			t.Errorf("unexpected registry request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestContainerUpdateCheckComparesRegistryDigests(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_RUNTIME_CALLS", calls)
	fakeRuntimeBinary(t, fakeRuntimeScript(composeLabels(t.TempDir())))
	registry, hits := stubRegistry(t, containerRemoteDigest)

	app, err := NewApp(detailTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon always dials TLS; the test dials its own registry instead.
	app.updateCheck.registry.baseURLFor = func(string) string { return registry.URL }
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer app.Shutdown(ctx)
	base := "http://" + app.ListenAddr()

	status, body := detailRequest(t, base, "/api/v1/containers/web/update-check")
	if status != http.StatusOK {
		t.Fatalf("update-check status = %d: %s", status, body)
	}
	var payload struct {
		Container containerUpdateStatus `json:"container"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("update-check body %s: %v", body, err)
	}
	result := payload.Container
	if result.Outdated == nil || !*result.Outdated {
		t.Fatalf("outdated = %v, want true (error %v)", result.Outdated, result.Error)
	}
	if result.Image != "docker.io/library/nginx:1.25" {
		t.Fatalf("image = %q", result.Image)
	}
	if result.LocalDigest != containerLocalDigest || result.RemoteDigest != containerRemoteDigest {
		t.Fatalf("digests = %q / %q", result.LocalDigest, result.RemoteDigest)
	}
	if !result.RestartRequired {
		t.Fatal("restart_required should be set: the local image is not what the container runs")
	}
	if result.Runtime != "podman" || result.Container != containerID {
		t.Fatalf("identity = %+v", result)
	}
	if hits.Load() != 1 {
		t.Fatalf("registry hits = %d, want 1", hits.Load())
	}

	// A second request within the refresh floor is answered from the cache, so
	// a client cannot drive registry traffic by asking repeatedly.
	if status, _ := detailRequest(t, base, "/api/v1/containers/web/update-check"); status != http.StatusOK {
		t.Fatalf("repeat update-check status = %d", status)
	}
	if hits.Load() != 1 {
		t.Fatalf("registry hits after a cached check = %d, want 1", hits.Load())
	}

	// The batch endpoint reports the same cached status without any registry
	// traffic at all.
	status, body = detailRequest(t, base, "/api/v1/updates")
	if status != http.StatusOK {
		t.Fatalf("updates status = %d: %s", status, body)
	}
	var updates containerUpdatesPayload
	if err := json.Unmarshal(body, &updates); err != nil {
		t.Fatal(err)
	}
	if len(updates.Containers) != 1 || updates.Containers[0].Outdated == nil || !*updates.Containers[0].Outdated {
		t.Fatalf("updates payload = %+v", updates.Containers)
	}
	if hits.Load() != 1 {
		t.Fatalf("registry hits after the batch read = %d, want 1", hits.Load())
	}
}

func TestParseImageReference(t *testing.T) {
	cases := []struct {
		raw      string
		registry string
		repo     string
		tag      string
		digest   string
		bad      bool
	}{
		{raw: "nginx", registry: "docker.io", repo: "library/nginx", tag: "latest"},
		{raw: "nginx:1.25", registry: "docker.io", repo: "library/nginx", tag: "1.25"},
		{raw: "solsynth/app:v2", registry: "docker.io", repo: "solsynth/app", tag: "v2"},
		{raw: "ghcr.io/solsynth/app:edge", registry: "ghcr.io", repo: "solsynth/app", tag: "edge"},
		{raw: "registry.local:5000/team/app", registry: "registry.local:5000", repo: "team/app", tag: "latest"},
		{raw: "localhost:5000/app:1", registry: "localhost:5000", repo: "app", tag: "1"},
		{raw: "index.docker.io/library/nginx:1", registry: "docker.io", repo: "library/nginx", tag: "1"},
		{raw: "nginx@" + containerLocalDigest, registry: "docker.io", repo: "library/nginx", digest: containerLocalDigest},
		{raw: "nginx:1.25@" + containerLocalDigest, registry: "docker.io", repo: "library/nginx", tag: "1.25", digest: containerLocalDigest},
		{raw: "", bad: true},
		{raw: "--help", bad: true},
		{raw: "-rf", bad: true},
		{raw: "nginx:1.25 --force", bad: true},
		{raw: "../etc/passwd", bad: true},
		{raw: "ghcr.io/", bad: true},
		{raw: "nginx@sha256:short", bad: true},
		{raw: "NGINX", bad: true},
	}
	for _, tc := range cases {
		ref, err := parseImageReference(tc.raw)
		if tc.bad {
			if err == nil {
				t.Errorf("parseImageReference(%q) = %+v, want an error", tc.raw, ref)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseImageReference(%q): %v", tc.raw, err)
			continue
		}
		if ref.Registry != tc.registry || ref.Repository != tc.repo || ref.Tag != tc.tag || ref.Digest != tc.digest {
			t.Errorf("parseImageReference(%q) = %+v", tc.raw, ref)
		}
	}

	if ref, err := parseImageReference("docker.io/library/nginx:1.25"); err != nil {
		t.Fatal(err)
	} else if ref.APIHost() != dockerHubAPIHost {
		t.Fatalf("docker hub API host = %q", ref.APIHost())
	}
	if ref, err := parseImageReference("ghcr.io/solsynth/app:edge"); err != nil {
		t.Fatal(err)
	} else if ref.APIHost() != "ghcr.io" || ref.String() != "ghcr.io/solsynth/app:edge" {
		t.Fatalf("ghcr reference = %+v", ref)
	}
}
