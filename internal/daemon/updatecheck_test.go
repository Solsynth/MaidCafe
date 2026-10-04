package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// registryTestClient builds a registry client dialling [url] instead of TLS —
// production always dials TLS, which is exactly why the seam exists.
func registryTestClient(t *testing.T, url string) *registryClient {
	t.Helper()
	client := newRegistryClient()
	client.baseURLFor = func(string) string { return url }
	client.credentials = func(string) (string, string, bool) { return "", "", false }
	return client
}

func TestRegistryClientFollowsBearerChallenge(t *testing.T) {
	var server *httptest.Server
	var tokenQuery string
	manifestRequests := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			tokenQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"token":"test-token","expires_in":300}`))
		case strings.HasSuffix(r.URL.Path, "/manifests/1.25"):
			manifestRequests++
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+server.URL+`/token",service="registry.local",scope="repository:team/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", containerRemoteDigest)
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
		default:
			t.Errorf("unexpected registry request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := registryTestClient(t, server.URL)
	ref, err := parseImageReference("registry.local/team/app:1.25")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := client.manifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.Digest != containerRemoteDigest {
		t.Fatalf("digest = %q", manifest.Digest)
	}
	if manifestRequests != 2 {
		t.Fatalf("manifest requests = %d, want the unauthenticated attempt then the authenticated one", manifestRequests)
	}
	if !strings.Contains(tokenQuery, "service=registry.local") || !strings.Contains(tokenQuery, "scope=repository%3Ateam%2Fapp%3Apull") {
		t.Fatalf("token query = %q", tokenQuery)
	}

	// The token is cached for the repository it was issued for, so a second
	// check of the same image does not authenticate again.
	if _, err := client.manifest(context.Background(), ref); err != nil {
		t.Fatalf("cached manifest: %v", err)
	}
	if manifestRequests != 3 {
		t.Fatalf("manifest requests after a cached token = %d, want 3", manifestRequests)
	}
}

func TestRegistryClientDialTargets(t *testing.T) {
	// The seam tests replace the dial target; production must reach Docker Hub
	// on its API host, which is not the name images are known by.
	client := newRegistryClient()
	hub, err := parseImageReference("nginx:1.25")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.baseURL(hub.APIHost()); got != "https://registry-1.docker.io" {
		t.Fatalf("docker hub base URL = %q", got)
	}
	private, err := parseImageReference("registry.example.com:5000/team/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.baseURL(private.APIHost()); got != "https://registry.example.com:5000" {
		t.Fatalf("private registry base URL = %q", got)
	}
}

func TestTokenEndpointRefusesADowngrade(t *testing.T) {
	if _, err := tokenEndpoint("https://auth.example/token", "reg", "repository:team/app:pull", "https://registry.example"); err != nil {
		t.Fatalf("https token endpoint: %v", err)
	}
	endpoint, err := tokenEndpoint("https://auth.example/token", "reg", "repository:team/app:pull", "https://registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpoint, "service=reg") || !strings.Contains(endpoint, "scope=repository%3Ateam%2Fapp%3Apull") {
		t.Fatalf("endpoint = %q", endpoint)
	}
	// A registry dialled over TLS may not send the credential to a plain-HTTP
	// endpoint of its own choosing.
	if _, err := tokenEndpoint("http://auth.example/token", "reg", "scope", "https://registry.example"); err == nil {
		t.Fatal("a plain-HTTP token endpoint must be refused for a TLS registry")
	}
	// The same rule keeps the test seam usable: an http registry uses http.
	if _, err := tokenEndpoint("http://127.0.0.1/token", "reg", "scope", "http://127.0.0.1:5000"); err != nil {
		t.Fatalf("http token endpoint: %v", err)
	}
	if _, err := tokenEndpoint("not-a-url", "reg", "scope", "https://registry.example"); err == nil {
		t.Fatal("a relative token endpoint must be refused")
	}
}

func TestRegistryClientReadsMultiPlatformIndex(t *testing.T) {
	index := `{"schemaVersion":2,"manifests":[` +
		`{"digest":"` + containerLocalDigest + `","mediaType":"application/vnd.oci.image.manifest.v1+json","platform":{"os":"linux","architecture":"amd64"}},` +
		`{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","mediaType":"application/vnd.oci.image.manifest.v1+json","platform":{"os":"linux","architecture":"arm64","variant":"v8"}}` +
		`]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", containerRemoteDigest)
		_, _ = w.Write([]byte(index))
	}))
	defer server.Close()

	client := registryTestClient(t, server.URL)
	ref, err := parseImageReference("docker.io/library/nginx:1.25")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := client.manifest(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Digest != containerRemoteDigest || len(manifest.Manifests) != 2 {
		t.Fatalf("index = %+v", manifest)
	}
	if !manifest.HasDigest(containerLocalDigest) {
		t.Fatal("index must report its member manifests")
	}
	if manifest.Manifests[1].PlatformKey != "linux/arm64/v8" {
		t.Fatalf("platform = %q", manifest.Manifests[1].PlatformKey)
	}
	if manifest.HasDigest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd") {
		t.Fatal("an unknown digest is not a member")
	}
}

func TestRegistryClientReportsRefusals(t *testing.T) {
	cases := map[int]string{
		http.StatusNotFound:   "no tag",
		http.StatusForbidden:  "refused access",
		http.StatusBadGateway: "answered 502",
	}
	for status, want := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		client := registryTestClient(t, server.URL)
		ref, err := parseImageReference("docker.io/library/nginx:1.25")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.manifest(context.Background(), ref)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: error = %v, want %q", status, err, want)
		}
	}
}

func TestRegistryCredentialsFromAuthFile(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("robot:secret"))
	body := `{"auths":{"ghcr.io":{"auth":"` + auth + `"},"https://index.docker.io/v1/":{"username":"hub","password":"pw"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("REGISTRY_AUTH_FILE", filepath.Join(dir, "missing.json"))
	t.Setenv("XDG_RUNTIME_DIR", "")

	username, password, ok := readRegistryCredentials("ghcr.io")
	if !ok || username != "robot" || password != "secret" {
		t.Fatalf("ghcr credential = %q/%q ok=%v", username, password, ok)
	}
	username, password, ok = readRegistryCredentials(dockerHubRegistry)
	if !ok || username != "hub" || password != "pw" {
		t.Fatalf("docker hub credential = %q/%q ok=%v", username, password, ok)
	}
	if _, _, ok := readRegistryCredentials("registry.example.com"); ok {
		t.Fatal("an unknown registry has no credential")
	}
	if username, password, ok := credentialsFromAuthFile(filepath.Join(dir, "nope.json"), "ghcr.io"); ok {
		t.Fatalf("a missing auth file must answer anonymously: %q/%q", username, password)
	}
}

// stubChecker builds an updateChecker whose runtime reads and registry are all
// answered in-process, so the comparison can be tested without a host.
func stubChecker(t *testing.T, inspect containerInspect, image containerImageInfo, registryURL string) *updateChecker {
	t.Helper()
	return &updateChecker{
		containers: func(context.Context) ([]containerRef, error) {
			return []containerRef{{Runtime: "podman", Path: "/usr/bin/podman", ID: containerID, Name: "web"}}, nil
		},
		inspect: func(context.Context, string, string) (containerInspect, error) { return inspect, nil },
		image:   func(context.Context, string, string) (containerImageInfo, error) { return image, nil },
		registry: &registryClient{
			client:        &http.Client{Timeout: time.Second},
			baseURLFor:    func(string) string { return registryURL },
			credentials:   func(string) (string, string, bool) { return "", "", false },
			authorization: map[string]registryToken{},
		},
		results: map[string]containerUpdateStatus{},
	}
}

// manifestServer answers every manifest request with [body] and [contentType].
func manifestServer(t *testing.T, contentType string, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Docker-Content-Digest", containerRemoteDigest)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUpdateCheckerComparesLocalAndPublishedImage(t *testing.T) {
	inspect := containerInspect{ImageRef: "nginx:1.25", ImageID: containerRunningID, Labels: map[string]string{}}
	local := containerImageInfo{
		ID:          containerLocalImageID,
		RepoDigests: []string{"docker.io/library/nginx@" + containerLocalDigest},
	}

	// A platform manifest that changed upstream is an update.
	changed := manifestServer(t, "application/vnd.oci.image.manifest.v1+json", `{"schemaVersion":2}`)
	checker := stubChecker(t, inspect, local, changed.URL)
	status := checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0)
	if status.Outdated == nil || !*status.Outdated {
		t.Fatalf("changed manifest: %+v", status)
	}
	if status.LocalDigest != containerLocalDigest || status.RemoteDigest != containerRemoteDigest {
		t.Fatalf("digests = %q / %q", status.LocalDigest, status.RemoteDigest)
	}
	if !status.RestartRequired {
		t.Fatal("the local image ID differs from the container's, so a recreate applies it")
	}

	// The same local digest published under a moved index is not an update for
	// this host: the tag gained another platform, not another image.
	index := manifestServer(t, "application/vnd.oci.image.index.v1+json",
		`{"manifests":[{"digest":"`+containerLocalDigest+`","platform":{"os":"linux","architecture":"amd64"}}]}`)
	checker = stubChecker(t, inspect, local, index.URL)
	status = checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0)
	if status.Outdated == nil || *status.Outdated {
		t.Fatalf("index member: %+v", status)
	}

	// A digest-pinned container is running what the operator asked for.
	pinned := containerInspect{ImageRef: "nginx@" + containerLocalDigest, ImageID: containerRunningID, Labels: map[string]string{}}
	checker = stubChecker(t, pinned, local, changed.URL)
	status = checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0)
	if status.Outdated == nil || *status.Outdated || !status.Pinned {
		t.Fatalf("pinned: %+v", status)
	}

	// An image that was built or imported here has no registry digest to
	// compare, and the honest answer is unknown — never "up to date".
	checker = stubChecker(t, inspect, containerImageInfo{ID: containerLocalImageID}, changed.URL)
	status = checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0)
	if status.Outdated != nil || status.Error == nil || !strings.Contains(*status.Error, "no registry digest") {
		t.Fatalf("locally built image: %+v", status)
	}

	// A registry that cannot be reached is reported, not guessed at.
	checker = stubChecker(t, inspect, local, "http://127.0.0.1:1")
	status = checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0)
	if status.Outdated != nil || status.Error == nil {
		t.Fatalf("unreachable registry: %+v", status)
	}
}

func TestUpdateCheckerRoundSkipsFreshAndForgetsGoneContainers(t *testing.T) {
	inspect := containerInspect{ImageRef: "nginx:1.25", ImageID: containerRunningID}
	local := containerImageInfo{ID: containerRunningID, RepoDigests: []string{"docker.io/library/nginx@" + containerLocalDigest}}
	server := manifestServer(t, "application/vnd.oci.image.manifest.v1+json", `{"schemaVersion":2}`)
	checker := stubChecker(t, inspect, local, server.URL)

	checker.CheckAll(context.Background(), time.Hour)
	if len(checker.Results()) != 1 {
		t.Fatalf("results after a round = %+v", checker.Results())
	}
	first, _ := checker.Status(containerID)
	if first.Outdated == nil {
		t.Fatalf("round did not check the container: %+v", first)
	}

	// A second round within the interval leaves the answer alone.
	checker.containers = func(context.Context) ([]containerRef, error) { return nil, nil }
	checker.CheckAll(context.Background(), time.Hour)
	if len(checker.Results()) != 1 {
		t.Fatalf("a round that cannot list containers must keep the results: %+v", checker.Results())
	}

	// A round that lists nothing prunes the container that is gone.
	checker.containers = func(context.Context) ([]containerRef, error) {
		return []containerRef{{Runtime: "podman", ID: "other", Name: "other"}}, nil
	}
	checker.CheckAll(context.Background(), time.Hour)
	if _, ok := checker.Status(containerID); ok {
		t.Fatalf("a container that no longer exists must be forgotten: %+v", checker.Results())
	}
	if _, ok := checker.Status("other"); !ok {
		t.Fatalf("the container that replaced it must be checked: %+v", checker.Results())
	}
}

// TestUpdateCheckerForgetStalePrunesOnlyWhatIsGone pins the read-path prune
// that keeps a replaced container's answer off its replacement. A recreate
// keeps the container's name and mints a new id, and a client matches a status
// to a container by name when the ids differ, so the predecessor has to be
// dropped the moment it is gone — not at the next cadence, which is the only
// other place a prune ran.
func TestUpdateCheckerForgetStalePrunesOnlyWhatIsGone(t *testing.T) {
	inspect := containerInspect{ImageRef: "nginx:1.25", ImageID: containerRunningID}
	local := containerImageInfo{ID: containerRunningID, RepoDigests: []string{"docker.io/library/nginx@" + containerLocalDigest}}
	server := manifestServer(t, "application/vnd.oci.image.manifest.v1+json", `{"schemaVersion":2}`)
	checker := stubChecker(t, inspect, local, server.URL)

	// The container the daemon still sees, and the one a recreate replaced
	// under the same name.
	if status := checker.Refresh(context.Background(), containerRef{Runtime: "podman", ID: containerID, Name: "web"}, 0); status.Outdated == nil {
		t.Fatalf("seed live status = %+v", status)
	}
	checker.store(containerUpdateStatus{
		Container: "replaced", Name: "web", Runtime: "podman", CheckedAt: time.Now().UTC(),
	})

	checker.ForgetStale(context.Background())
	if _, ok := checker.Status("replaced"); ok {
		t.Fatal("a container the host no longer has must be forgotten on the read path")
	}
	if _, ok := checker.Status(containerID); !ok {
		t.Fatal("a container that is still here must be kept")
	}

	// A listing that fails must not empty the cache: a store that did not
	// answer this second is not a store whose containers are gone.
	checker.containers = func(context.Context) ([]containerRef, error) {
		return nil, errors.New("podman unavailable")
	}
	checker.ForgetStale(context.Background())
	if len(checker.Results()) != 1 {
		t.Fatalf("a failed listing emptied the cache: %+v", checker.Results())
	}

	// Nor may an empty listing, for the same reason.
	checker.containers = func(context.Context) ([]containerRef, error) { return nil, nil }
	checker.ForgetStale(context.Background())
	if len(checker.Results()) != 1 {
		t.Fatalf("an empty listing emptied the cache: %+v", checker.Results())
	}
}

func TestResolveUpdateQueryPrefersThePulledRepository(t *testing.T) {
	ref, err := parseImageReference("nginx:1.25")
	if err != nil {
		t.Fatal(err)
	}
	// A repository that matches the reference wins.
	query, digest, err := resolveUpdateQuery(ref, containerImageInfo{
		RepoDigests: []string{"registry.example.com/other@" + containerLocalDigest, "docker.io/library/nginx@" + containerRemoteDigest},
	})
	if err != nil || query.Repository != "library/nginx" || query.Tag != "1.25" || digest != containerRemoteDigest {
		t.Fatalf("query = %+v digest = %q err = %v", query, digest, err)
	}
	// A single recorded name is what the runtime actually pulled, even when a
	// short name would resolve elsewhere.
	query, digest, err = resolveUpdateQuery(ref, containerImageInfo{
		RepoDigests: []string{"registry.fedoraproject.org/nginx@" + containerLocalDigest},
	})
	if err != nil || query.Registry != "registry.fedoraproject.org" || query.Tag != "1.25" || digest != containerLocalDigest {
		t.Fatalf("short-name query = %+v digest = %q err = %v", query, digest, err)
	}
	// Several recorded names and none matching is ambiguous, and guessing would
	// compare the wrong image.
	if _, _, err := resolveUpdateQuery(ref, containerImageInfo{
		RepoDigests: []string{"a.example/app@" + containerLocalDigest, "b.example/app@" + containerRemoteDigest},
	}); err == nil {
		t.Fatal("an ambiguous image must not be checked")
	}
}

func TestParseAuthChallenge(t *testing.T) {
	challenge := parseAuthChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if challenge.scheme != "bearer" || challenge.realm != "https://auth.docker.io/token" ||
		challenge.service != "registry.docker.io" || challenge.scope != "repository:library/nginx:pull" {
		t.Fatalf("challenge = %+v", challenge)
	}
	if basic := parseAuthChallenge("Basic realm=\"registry\""); basic.scheme != "basic" || basic.realm != "registry" {
		t.Fatalf("basic challenge = %+v", basic)
	}
	if unknown := parseAuthChallenge("Negotiate"); unknown.scheme != "negotiate" {
		t.Fatalf("unknown scheme = %+v", unknown)
	}
	// A comma inside a quoted value must not split the parameter list.
	withComma := parseAuthChallenge(`Bearer realm="https://auth.example/token",scope="repository:team/app:pull,push"`)
	if withComma.scope != "repository:team/app:pull,push" {
		t.Fatalf("quoted comma = %+v", withComma)
	}
}

func TestParseImageNameDigest(t *testing.T) {
	name, err := parseImageNameDigest("docker.io/library/nginx@" + containerLocalDigest)
	if err != nil || name.Registry != dockerHubRegistry || name.Repository != "library/nginx" || name.Digest != containerLocalDigest {
		t.Fatalf("name = %+v err = %v", name, err)
	}
	name, err = parseImageNameDigest("ghcr.io/solsynth/app@" + containerLocalDigest)
	if err != nil || name.Registry != "ghcr.io" || name.Repository != "solsynth/app" {
		t.Fatalf("name = %+v err = %v", name, err)
	}
	if _, err := parseImageNameDigest("nginx:latest"); err == nil {
		t.Fatal("a name without a digest must not parse")
	}
}

func TestUpdateStatusMarshalsUnknownAsNull(t *testing.T) {
	body, err := json.Marshal(containerUpdateStatus{Container: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"outdated":null`) {
		t.Fatalf("payload = %s", body)
	}
}
