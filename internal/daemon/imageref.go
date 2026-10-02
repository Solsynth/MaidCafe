package daemon

import (
	"fmt"
	"regexp"
	"strings"
)

// Container image references.
//
// The daemon needs a reference for two things: to pull the image a container
// was created from, and to ask the image's registry whether a newer one is
// published. Both put the reference into an argv slot and a registry URL, so a
// reference is parsed and validated here before either happens — the daemon
// never passes a caller- or label-supplied string through unexamined.

// dockerHubRegistry is the registry a bare name resolves against, and
// dockerHubAPIHost is where its API actually lives: Docker Hub serves the
// registry API from registry-1.docker.io even though images are named
// docker.io.
const (
	dockerHubRegistry = "docker.io"
	dockerHubAPIHost  = "registry-1.docker.io"
)

// imageReference is a parsed image reference: the registry host, the
// repository inside it, and either a tag or a pinned digest. A reference
// pinned by digest carries no tag: the digest is what the caller asked for.
type imageReference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

var (
	// imageRepositoryPattern is the repository path grammar (lowercase
	// alphanumeric components separated by '/', '.' or '-'), without the
	// registry host. It is anchored, and a component may not start with a
	// separator, so "..", a leading '/' and an empty component are all
	// refused.
	imageRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*$`)
	imageTagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	imageDigestPattern     = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]{32,}$`)
	registryHostPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]*$`)
)

// parseImageReference parses a runtime image reference. The grammar is the
// runtime's own: a leading segment is a registry host only when it looks like
// one (it contains a dot or a port, or is "localhost"), a bare Docker Hub name
// gains the "library/" prefix, and a missing tag means "latest" unless the
// reference is pinned by digest.
func parseImageReference(raw string) (imageReference, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return imageReference{}, fmt.Errorf("empty image reference")
	}
	if strings.HasPrefix(raw, "-") {
		// A leading dash would reach the runtime's option parser as a flag.
		return imageReference{}, fmt.Errorf("invalid image reference %q", raw)
	}
	ref := imageReference{}
	name := raw
	if at := strings.IndexByte(name, '@'); at >= 0 {
		ref.Digest = name[at+1:]
		name = name[:at]
		if !imageDigestPattern.MatchString(ref.Digest) {
			return imageReference{}, fmt.Errorf("invalid image digest %q", ref.Digest)
		}
	}
	if slash := strings.IndexByte(name, '/'); slash >= 0 {
		if first := name[:slash]; strings.ContainsAny(first, ".:") || first == "localhost" {
			if !registryHostPattern.MatchString(first) {
				return imageReference{}, fmt.Errorf("invalid registry host %q", first)
			}
			ref.Registry = first
			name = name[slash+1:]
		}
	}
	// The tag is whatever follows the last ':' of what is left; a registry
	// port was already consumed above, so this cannot mistake one for a tag.
	if colon := strings.LastIndexByte(name, ':'); colon >= 0 {
		ref.Tag = name[colon+1:]
		name = name[:colon]
	}
	if name == "" {
		return imageReference{}, fmt.Errorf("missing repository in image reference %q", raw)
	}
	if ref.Registry == "" || ref.Registry == dockerHubRegistry || ref.Registry == "index.docker.io" {
		ref.Registry = dockerHubRegistry
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	if !imageRepositoryPattern.MatchString(name) {
		return imageReference{}, fmt.Errorf("invalid image repository %q", name)
	}
	ref.Repository = name
	if ref.Tag != "" && !imageTagPattern.MatchString(ref.Tag) {
		return imageReference{}, fmt.Errorf("invalid image tag %q", ref.Tag)
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref, nil
}

// Pinned reports whether the reference names a digest rather than a tag: such
// an image is what the operator asked for and never "outdated".
func (r imageReference) Pinned() bool { return r.Digest != "" }

// APIHost is the host serving the registry API for this reference.
func (r imageReference) APIHost() string {
	if r.Registry == dockerHubRegistry || r.Registry == "index.docker.io" {
		return dockerHubAPIHost
	}
	return r.Registry
}

// String renders the reference as a runtime would accept it.
func (r imageReference) String() string {
	out := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return out + "@" + r.Digest
	}
	return out + ":" + r.Tag
}

// imageName is one parsed "name@digest" pair, the form a runtime reports
// pulled images in (its RepoDigests list).
type imageName struct {
	Registry   string
	Repository string
	Digest     string
}

// parseImageNameDigest parses the "<registry>/<repository>@<digest>" form a
// runtime records for a pulled image.
func parseImageNameDigest(raw string) (imageName, error) {
	raw = strings.TrimSpace(raw)
	at := strings.LastIndexByte(raw, '@')
	if at <= 0 {
		return imageName{}, fmt.Errorf("image name %q has no digest", raw)
	}
	name, digest := raw[:at], raw[at+1:]
	if !imageDigestPattern.MatchString(digest) {
		return imageName{}, fmt.Errorf("image name %q has an invalid digest", raw)
	}
	registry, repository := dockerHubRegistry, name
	if slash := strings.IndexByte(name, '/'); slash >= 0 {
		if first := name[:slash]; strings.ContainsAny(first, ".:") || first == "localhost" {
			registry, repository = first, name[slash+1:]
		}
	}
	if registry == "index.docker.io" {
		registry = dockerHubRegistry
	}
	if registry == dockerHubRegistry && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return imageName{Registry: registry, Repository: repository, Digest: digest}, nil
}

// sameRepository reports whether an image name and a reference point at the
// same repository on the same registry, which is what makes two digests
// comparable.
func (n imageName) sameRepository(ref imageReference) bool {
	return n.Registry == ref.Registry && n.Repository == ref.Repository
}
