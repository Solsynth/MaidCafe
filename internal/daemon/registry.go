package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Registry metadata reads.
//
// Answering "is a newer image published for this container?" needs one fact
// from the image's registry: the digest its tag points at right now. That is a
// single manifest request over the Docker Registry HTTP API v2 — the same API
// podman and docker speak — and deliberately not a pull: pulling would replace
// the very image the question is about, download every layer, and make the
// daemon the host's image fetcher.

const (
	// registryRequestTimeout bounds one registry request, and
	// registryManifestBytes bounds the manifest body the daemon reads. A
	// multi-platform index is a few kilobytes of JSON; anything larger is not a
	// manifest answer.
	registryRequestTimeout = 10 * time.Second
	registryManifestBytes  = 1 << 20
	registryTokenBytes     = 64 << 10
	// registryTokenTTLMargin refreshes a token slightly before the registry's
	// own expiry, so a request is never sent with a token that expires on the
	// way.
	registryTokenTTLMargin = time.Minute
)

// manifestMediaTypes are offered to the registry in preference order: an index
// first, so a multi-platform tag comes back as an index rather than as
// whatever single platform the registry would otherwise pick for us.
var manifestMediaTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.v1+json",
}, ", ")

// registryManifest is the answer to one tag lookup: the digest the tag points
// at, and — when that digest is a multi-platform index — its member manifests,
// which is how a platform-specific local digest is recognised as current.
type registryManifest struct {
	Digest    string
	MediaType string
	Manifests []registryManifestRef
}

// registryManifestRef is one member manifest of an index.
type registryManifestRef struct {
	Digest      string
	MediaType   string
	PlatformKey string
}

// HasDigest reports whether [digest] is one of the index's member manifests,
// which means the platform-specific image a host holds is still the one the tag
// resolves to even though the index digest itself moved.
func (m registryManifest) HasDigest(digest string) bool {
	if digest == "" {
		return false
	}
	for _, entry := range m.Manifests {
		if entry.Digest == digest {
			return true
		}
	}
	return false
}

// registryClient reads manifest metadata from a registry. It keeps only bearer
// tokens (bounded, expiring, scoped to one repository) and never writes to the
// host's image store.
type registryClient struct {
	client *http.Client
	// baseURLFor returns the API base URL (scheme and host, no trailing slash)
	// for a registry. It is a field so tests can dial a local server;
	// production always dials TLS, so a credential is never sent in the clear.
	baseURLFor func(registry string) string
	// credentials resolves a Basic credential for a registry from the daemon
	// account's own runtime auth files. Nil, or no entry, means anonymous.
	credentials func(registry string) (username, password string, ok bool)

	mu            sync.Mutex
	authorization map[string]registryToken
}

func newRegistryClient() *registryClient {
	return &registryClient{
		client:        &http.Client{Timeout: registryRequestTimeout},
		baseURLFor:    func(registry string) string { return "https://" + registry },
		credentials:   readRegistryCredentials,
		authorization: map[string]registryToken{},
	}
}

// registryToken is a cached authorization header for one registry repository.
type registryToken struct {
	header  string
	expires time.Time
}

// manifest fetches the manifest [ref]'s tag currently resolves to. A 401 is
// answered by authenticating once against the realm the registry named — with
// the daemon account's own stored credential when there is one — and retrying,
// which is the whole of the authentication dance this needs.
func (c *registryClient) manifest(ctx context.Context, ref imageReference) (registryManifest, error) {
	if ref.Pinned() {
		// A digest-pinned image is what the operator asked for; there is nothing
		// to compare it against.
		return registryManifest{Digest: ref.Digest}, nil
	}
	// The API host is not the name in the reference for Docker Hub: images are
	// named docker.io but its registry API answers on registry-1.docker.io.
	endpoint := fmt.Sprintf("%s/v2/%s/manifests/%s", strings.TrimSuffix(c.baseURL(ref.APIHost()), "/"), ref.Repository, ref.Tag)
	authorization := c.cachedAuthorization(ref)
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return registryManifest{}, err
		}
		request.Header.Set("Accept", manifestMediaTypes)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := c.client.Do(request)
		if err != nil {
			return registryManifest{}, fmt.Errorf("registry request: %w", err)
		}
		if response.StatusCode == http.StatusUnauthorized {
			challenge := parseAuthChallenge(response.Header.Get("WWW-Authenticate"))
			drainAndClose(response.Body)
			if attempt > 0 {
				return registryManifest{}, fmt.Errorf("registry rejected the daemon's credentials for %s", ref.String())
			}
			fresh, err := c.authorize(ctx, ref, challenge)
			if err != nil {
				return registryManifest{}, err
			}
			authorization = fresh
			continue
		}
		if response.StatusCode != http.StatusOK {
			drainAndClose(response.Body)
			return registryManifest{}, registryStatusError(response.StatusCode, ref)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, registryManifestBytes))
		drainAndClose(response.Body)
		if err != nil {
			return registryManifest{}, fmt.Errorf("read registry manifest: %w", err)
		}
		return parseRegistryManifest(body, response.Header.Get("Content-Type"), response.Header.Get("Docker-Content-Digest"))
	}
	return registryManifest{}, fmt.Errorf("registry authentication failed for %s", ref.String())
}

func (c *registryClient) baseURL(registry string) string {
	if c.baseURLFor != nil {
		return c.baseURLFor(registry)
	}
	return "https://" + registry
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, registryManifestBytes))
	_ = body.Close()
}

// registryStatusError turns a registry status code into something an operator
// can act on: a tag that no longer exists, a private repository read
// anonymously, or a registry that answered with an error.
func registryStatusError(status int, ref imageReference) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("registry has no tag %s", ref.String())
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("registry refused access to %s (log in with the runtime CLI on this host)", ref.String())
	default:
		return fmt.Errorf("registry answered %d for %s", status, ref.String())
	}
}

// authorize performs one authentication exchange: it asks the realm named in
// the challenge for a token scoped to this repository, sending the daemon
// account's stored credential for the registry when there is one. It returns
// the Authorization header value to use.
func (c *registryClient) authorize(ctx context.Context, ref imageReference, challenge authChallenge) (string, error) {
	switch challenge.scheme {
	case "basic":
		// The registry wants a Basic credential on the manifest request itself;
		// treating it as a cacheable header keeps the retry path uniform.
		username, password, ok := c.credentialFor(ref.Registry)
		if !ok {
			return "", fmt.Errorf("registry requires credentials for %s", ref.String())
		}
		authorization := basicAuthorization(username, password)
		c.storeAuthorization(ref, authorization, time.Hour)
		return authorization, nil
	case "bearer":
	default:
		return "", fmt.Errorf("registry requires an unsupported authentication scheme for %s", ref.String())
	}
	if challenge.realm == "" {
		return "", fmt.Errorf("registry sent no authentication endpoint for %s", ref.String())
	}
	scope := challenge.scope
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	endpoint, err := tokenEndpoint(challenge.realm, challenge.service, scope, c.baseURL(ref.APIHost()))
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if username, password, ok := c.credentialFor(ref.Registry); ok {
		request.SetBasicAuth(username, password)
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("registry token request: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		drainAndClose(response.Body)
		return "", fmt.Errorf("registry token request answered %d for %s", response.StatusCode, ref.String())
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, registryTokenBytes))
	drainAndClose(response.Body)
	if err != nil {
		return "", fmt.Errorf("read registry token: %w", err)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("registry token response: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry returned no token for %s", ref.String())
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl > registryTokenTTLMargin {
		ttl -= registryTokenTTLMargin
	} else {
		ttl = time.Minute
	}
	authorization := "Bearer " + token
	c.storeAuthorization(ref, authorization, ttl)
	return authorization, nil
}

func basicAuthorization(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func (c *registryClient) credentialFor(registry string) (string, string, bool) {
	if c.credentials == nil {
		return "", "", false
	}
	return c.credentials(registry)
}

// cachedAuthorization returns a stored authorization header for the
// reference's repository while it is still valid.
func (c *registryClient) cachedAuthorization(ref imageReference) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.authorization[tokenKey(ref)]
	if !ok || time.Now().After(cached.expires) {
		return ""
	}
	return cached.header
}

func (c *registryClient) storeAuthorization(ref imageReference, header string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authorization == nil {
		c.authorization = map[string]registryToken{}
	}
	c.authorization[tokenKey(ref)] = registryToken{header: header, expires: time.Now().Add(ttl)}
}

// tokenKey scopes a cached credential to the repository it was issued for, so
// it can never be replayed against another repository on the same registry.
func tokenKey(ref imageReference) string {
	return ref.Registry + "/" + ref.Repository
}

// authChallenge is a parsed WWW-Authenticate header.
type authChallenge struct {
	scheme  string
	realm   string
	service string
	scope   string
}

// parseAuthChallenge reads a WWW-Authenticate header. Both "Bearer" and
// "Basic" are recognised; any other scheme keeps its own name so the caller
// refuses it explicitly instead of silently retrying anonymously.
func parseAuthChallenge(header string) authChallenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return authChallenge{}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	challenge := authChallenge{scheme: strings.ToLower(strings.TrimSpace(scheme))}
	for _, part := range splitAuthParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			challenge.realm = value
		case "service":
			challenge.service = value
		case "scope":
			challenge.scope = value
		}
	}
	return challenge
}

// splitAuthParams splits the parameter list of a challenge on commas that are
// not inside a quoted value.
func splitAuthParams(value string) []string {
	parts := make([]string, 0, 3)
	var current strings.Builder
	quoted := false
	for _, r := range value {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ',' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, current.String())
	return parts
}

// tokenEndpoint builds the token URL the registry's challenge named. The realm
// must be an absolute URL over the same scheme as the registry itself: the
// answer to it is a credential, so a registry the daemon reached over TLS can
// never redirect that credential to a plain-HTTP endpoint of its choosing.
func tokenEndpoint(realm, service, scope, registryBase string) (string, error) {
	scheme, _, found := strings.Cut(realm, "://")
	if !found {
		return "", fmt.Errorf("registry token endpoint %q is not an absolute URL", realm)
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("registry token endpoint %q is not an http(s) URL", realm)
	}
	if registryScheme, _, ok := strings.Cut(registryBase, "://"); ok && strings.ToLower(registryScheme) != scheme {
		return "", fmt.Errorf("registry token endpoint %q is not reachable over %s", realm, registryScheme)
	}
	parsed, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("registry token endpoint %q: %w", realm, err)
	}
	query := parsed.Query()
	if service != "" {
		query.Set("service", service)
	}
	query.Set("scope", scope)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// parseRegistryManifest reads a manifest response: its digest, its media type,
// and — for a multi-platform index — the digests of the platform manifests it
// lists.
func parseRegistryManifest(body []byte, contentType, contentDigest string) (registryManifest, error) {
	manifest := registryManifest{
		Digest:    strings.TrimSpace(contentDigest),
		MediaType: normalizeMediaType(contentType),
	}
	if manifest.Digest == "" {
		// Without the header the body is the authority: a manifest is addressed
		// by the digest of its own bytes.
		sum := sha256.Sum256(body)
		manifest.Digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if !imageDigestPattern.MatchString(manifest.Digest) {
		return registryManifest{}, fmt.Errorf("registry returned an invalid manifest digest %q", manifest.Digest)
	}
	if !isIndexMediaType(manifest.MediaType) {
		return manifest, nil
	}
	var index struct {
		Manifests []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Platform  struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return registryManifest{}, fmt.Errorf("registry index: %w", err)
	}
	for _, entry := range index.Manifests {
		platform := entry.Platform.OS
		if entry.Platform.Architecture != "" {
			platform += "/" + entry.Platform.Architecture
		}
		if entry.Platform.Variant != "" {
			platform += "/" + entry.Platform.Variant
		}
		manifest.Manifests = append(manifest.Manifests, registryManifestRef{
			Digest: entry.Digest, MediaType: entry.MediaType, PlatformKey: platform,
		})
	}
	return manifest, nil
}

func normalizeMediaType(contentType string) string {
	value, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(value))
}

func isIndexMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
}

// readRegistryCredentials looks up a Basic credential for [registry] in the
// daemon account's own runtime auth files, in the order the runtimes
// themselves use. Reading them is how a private registry becomes checkable at
// all; nothing here is logged, and a missing or unreadable file simply means
// the check runs anonymously. A config that defers to an external credential
// helper (`credHelpers`/`credsStore`) holds no secret to read here, so the check
// reports the registry's refusal and the operator logs in as that account.
func readRegistryCredentials(registry string) (string, string, bool) {
	for _, path := range registryAuthPaths() {
		username, password, ok := credentialsFromAuthFile(path, registry)
		if ok {
			return username, password, true
		}
	}
	return "", "", false
}

func registryAuthPaths() []string {
	paths := make([]string, 0, 4)
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		paths = append(paths, filepath.Join(dir, "config.json"))
	}
	if file := strings.TrimSpace(os.Getenv("REGISTRY_AUTH_FILE")); file != "" {
		paths = append(paths, file)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, ".docker", "config.json"),
			filepath.Join(home, ".config", "containers", "auth.json"),
		)
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, "containers", "auth.json"))
	}
	return paths
}

// credentialsFromAuthFile reads one docker/containers auth file and returns the
// credential stored for [registry]. Both the base64 "auth" field and the plain
// username/password pair are understood, and every Docker Hub alias is matched,
// because a runtime may have logged in under any of them.
func credentialsFromAuthFile(path, registry string) (string, string, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var file struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return "", "", false
	}
	for _, key := range registryAuthKeys(registry) {
		entry, ok := file.Auths[key]
		if !ok {
			continue
		}
		if entry.Username != "" || entry.Password != "" {
			return entry.Username, entry.Password, true
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry.Auth))
		if err != nil {
			continue
		}
		username, password, ok := strings.Cut(string(decoded), ":")
		if !ok {
			continue
		}
		return username, password, true
	}
	return "", "", false
}

// registryAuthKeys lists the keys an auth file may have used for [registry],
// including the URL form docker writes for Docker Hub.
func registryAuthKeys(registry string) []string {
	if registry == dockerHubRegistry || registry == dockerHubAPIHost || registry == "index.docker.io" {
		return []string{
			"https://index.docker.io/v1/",
			"index.docker.io",
			dockerHubRegistry,
			dockerHubAPIHost,
		}
	}
	return []string{registry}
}
