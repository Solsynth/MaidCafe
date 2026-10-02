package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Container update checks.
//
// "Is this container outdated?" is answered by comparing two digests: the one
// the runtime recorded when it pulled the container's image, and the one the
// image's registry publishes for that tag right now. Nothing is pulled or
// written, and a container the daemon cannot resolve is reported as unknown
// rather than guessed at — a wrong "up to date" badge is worse than a blank
// one, because it is the reason nobody looks.

const (
	// updateCheckStartupDelay keeps a host that restarts often from re-checking
	// every registry on every boot.
	updateCheckStartupDelay = 30 * time.Second
	// updateCheckRefreshFloor is the shortest gap between two on-demand checks
	// of the same container. A manual "check now" is meant to feel immediate,
	// but registries rate-limit manifest reads per address, so a button cannot
	// be allowed to drive one per click.
	updateCheckRefreshFloor = time.Minute
	// updateCheckContainerTimeout bounds one container's whole check: three
	// runtime reads and one registry request, each separately bounded.
	updateCheckContainerTimeout = time.Minute
)

// containerUpdateStatus is one container's published-image comparison. A nil
// Outdated means the question could not be answered, and Error says why.
type containerUpdateStatus struct {
	Container string    `json:"container"`
	Name      string    `json:"name"`
	Runtime   string    `json:"runtime"`
	Image     string    `json:"image"`
	CheckedAt time.Time `json:"checked_at"`
	// Outdated is true when the registry publishes an image the container is
	// not running, and false when the container is current. Null means unknown.
	Outdated *bool `json:"outdated"`
	// Pinned marks a container created from a digest-pinned reference, which is
	// never outdated: the digest is what the operator asked for.
	Pinned bool `json:"pinned"`
	// RestartRequired is true when the local image store already holds a newer
	// image than this container is running, so a recreate applies it without
	// anything to download.
	RestartRequired bool    `json:"restart_required"`
	LocalDigest     string  `json:"local_digest,omitempty"`
	RemoteDigest    string  `json:"remote_digest,omitempty"`
	Error           *string `json:"error,omitempty"`
}

// containerUpdatesPayload is the batch answer: every status the daemon holds,
// plus the cadence it refreshes them on, so a client can tell how old they are
// and when they will move.
type containerUpdatesPayload struct {
	IntervalSeconds int64                   `json:"interval_seconds"`
	Containers      []containerUpdateStatus `json:"containers"`
}

// updateChecker keeps the daemon's answers to the update question. Checks are
// network work, so a result is cached and reused: the cadence refreshes it,
// and an on-demand refresh is floored.
type updateChecker struct {
	// containers, inspect and image are the reads a check is built on. They are
	// fields so a test can answer without a runtime binary on PATH.
	containers func(ctx context.Context) ([]containerRef, error)
	inspect    func(ctx context.Context, path, id string) (containerInspect, error)
	image      func(ctx context.Context, path, ref string) (containerImageInfo, error)
	registry   *registryClient

	mu      sync.Mutex
	results map[string]containerUpdateStatus
}

func newUpdateChecker(
	containers func(ctx context.Context) ([]containerRef, error),
	registry *registryClient,
) *updateChecker {
	return &updateChecker{
		containers: containers,
		inspect:    inspectContainer,
		image:      imageInfo,
		registry:   registry,
		results:    map[string]containerUpdateStatus{},
	}
}

// Results returns the cached statuses, ordered by container name so repeated
// requests answer in the same order.
func (c *updateChecker) Results() []containerUpdateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]containerUpdateStatus, 0, len(c.results))
	for _, status := range c.results {
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Container < out[j].Container
	})
	return out
}

// Status returns one container's cached status.
func (c *updateChecker) Status(id string) (containerUpdateStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.results[id]
	return status, ok
}

// Refresh checks one container and records the outcome, unless it was checked
// within [minAge]. The cached status is returned either way, so a caller never
// has to distinguish "just checked" from "too recent to check again".
func (c *updateChecker) Refresh(ctx context.Context, ref containerRef, minAge time.Duration) containerUpdateStatus {
	if cached, ok := c.Status(ref.ID); ok {
		if age := time.Since(cached.CheckedAt); age < minAge {
			return cached
		}
	}
	status := c.check(ctx, ref)
	c.store(status)
	return status
}

// CheckAll refreshes every container the daemon sees, one at a time: registries
// rate-limit per address, and a host with many containers should not arrive at
// one all at once. Containers checked within [minAge] are left alone, and a
// round that could not list containers keeps the results it has.
func (c *updateChecker) CheckAll(ctx context.Context, minAge time.Duration) {
	refs, err := c.containers(ctx)
	if err != nil || len(refs) == 0 {
		return
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ctx.Err() != nil {
			return
		}
		seen[ref.ID] = true
		if cached, ok := c.Status(ref.ID); ok && time.Since(cached.CheckedAt) < minAge {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, updateCheckContainerTimeout)
		status := c.check(checkCtx, ref)
		cancel()
		c.store(status)
	}
	c.forget(seen)
}

func (c *updateChecker) store(status containerUpdateStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results == nil {
		c.results = map[string]containerUpdateStatus{}
	}
	c.results[status.Container] = status
}

// forget drops results for containers that no longer exist, so the cache does
// not grow with every container the host has ever run.
func (c *updateChecker) forget(seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.results {
		if !seen[id] {
			delete(c.results, id)
		}
	}
}

// check resolves one container's image and compares it against its registry.
// Every way it can fail is recorded in the returned status: an unreachable
// registry, a runtime that cannot answer, or an image that was never pulled
// from one. That is deliberate — a failed check is a status a client can show,
// not an error that hides the container.
func (c *updateChecker) check(ctx context.Context, ref containerRef) containerUpdateStatus {
	status := containerUpdateStatus{
		Container: ref.ID,
		Name:      ref.Name,
		Runtime:   ref.Runtime,
		CheckedAt: time.Now().UTC(),
	}
	fail := func(format string, args ...any) containerUpdateStatus {
		message := fmt.Sprintf(format, args...)
		status.Error = &message
		return status
	}
	info, err := c.inspect(ctx, ref.Path, ref.ID)
	if err != nil {
		return fail("inspect container: %s", err)
	}
	status.Image = info.ImageRef
	if info.ImageRef == "" {
		return fail("the container records no image reference")
	}
	imageRef, err := parseImageReference(info.ImageRef)
	if err != nil {
		return fail("unusable image reference %q", info.ImageRef)
	}
	if imageRef.Pinned() {
		// A digest-pinned container is running exactly what was asked for.
		notOutdated := false
		status.Outdated = &notOutdated
		status.Pinned = true
		status.LocalDigest = imageRef.Digest
		status.RemoteDigest = imageRef.Digest
		return status
	}
	local, err := c.image(ctx, ref.Path, info.ImageRef)
	if err != nil {
		return fail("read the local image: %s", err)
	}
	query, localDigest, err := resolveUpdateQuery(imageRef, local)
	if err != nil {
		return fail("%s", err)
	}
	status.LocalDigest = localDigest
	status.RestartRequired = local.ID != "" && info.ImageID != "" && local.ID != info.ImageID
	manifest, err := c.registry.manifest(ctx, query)
	if err != nil {
		return fail("%s", err)
	}
	status.RemoteDigest = manifest.Digest
	// A platform-specific local digest that is still one of the index's
	// members is current even though the index digest itself moved: the tag
	// gained or lost another platform, not a new image for this host.
	outdated := manifest.Digest != localDigest && !manifest.HasDigest(localDigest)
	status.Outdated = &outdated
	return status
}

// resolveUpdateQuery decides what to ask the registry about. The tag comes
// from the reference the container was created with, while the repository
// comes from the digest the runtime recorded when it pulled — which is the
// runtime's own answer to "what is this image", and the only thing that
// survives short-name aliasing (podman may resolve "nginx" against a different
// registry than docker would) and a canonical re-tag.
func resolveUpdateQuery(ref imageReference, local containerImageInfo) (imageReference, string, error) {
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	names := make([]imageName, 0, len(local.RepoDigests))
	for _, raw := range local.RepoDigests {
		parsed, err := parseImageNameDigest(raw)
		if err != nil {
			continue
		}
		if parsed.sameRepository(ref) {
			return imageReference{Registry: parsed.Registry, Repository: parsed.Repository, Tag: tag}, parsed.Digest, nil
		}
		names = append(names, parsed)
	}
	switch len(names) {
	case 0:
		return imageReference{}, "", fmt.Errorf("the local image %q records no registry digest, so it was built or imported here", ref.String())
	case 1:
		// Exactly one name: the runtime pulled this image from somewhere else
		// than the reference spells out (a short-name alias, or a retag).
		return imageReference{Registry: names[0].Registry, Repository: names[0].Repository, Tag: tag}, names[0].Digest, nil
	default:
		repositoryNames := make([]string, 0, len(names))
		for _, name := range names {
			repositoryNames = append(repositoryNames, name.Registry+"/"+name.Repository)
		}
		return imageReference{}, "", fmt.Errorf(
			"the local image is known as %s and none of them is %s",
			strings.Join(repositoryNames, ", "), ref.Repository,
		)
	}
}
