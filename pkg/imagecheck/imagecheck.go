// Package imagecheck verifies that the images a release names exist in their
// registries and are multi-platform.
//
// A digest in a chart's values.yaml proves only that somebody wrote one down.
// Verify resolves registry/repository@digest, and, when platforms are named,
// requires an image index (or Docker manifest list) that lists each of them
// and whose per-platform manifests resolve too. It needs registry read access
// only.
package imagecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/ocipush"
)

const mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// maxManifestBytes bounds how much of a manifest is read; real ones are a few KiB.
const maxManifestBytes = 4 << 20

// Image is one entry of a chart's `images:` map.
type Image struct {
	// Key is the entry's name under `images:` (what an error names).
	Key string
	// Registry may be empty when Repository already carries the host.
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// Repo is the repository reference, host/path with no digest.
func (i Image) Repo() string {
	if i.Registry == "" {
		return i.Repository
	}

	return i.Registry + "/" + i.Repository
}

// Ref is the digest-pinned reference repository@digest.
func (i Image) Ref() string { return i.Repo() + "@" + i.Digest }

// Platform is os/architecture[/variant].
type Platform struct{ OS, Arch, Variant string }

// String renders os/arch or os/arch/variant.
func (p Platform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}

	return s
}

// ParsePlatforms parses os/arch[/variant] values; empty entries are skipped.
func ParsePlatforms(values []string) ([]Platform, error) {
	var out []Platform

	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}

		parts := strings.Split(v, "/")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
			return nil, fmt.Errorf("platform %q: want os/arch or os/arch/variant", v)
		}

		p := Platform{OS: parts[0], Arch: parts[1]}
		if len(parts) == 3 {
			p.Variant = parts[2]
		}

		out = append(out, p)
	}

	return out, nil
}

// FromValues reads the `images:` map of a values document (a chart's
// values.yaml or a release manifest's values), sorted by key. A document
// without `images:` yields none.
func FromValues(values map[string]any) ([]Image, error) {
	raw, ok := values["images"]
	if !ok || raw == nil {
		return nil, nil
	}

	entries, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("images: want a map of image entries")
	}

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	out := make([]Image, 0, len(keys))

	for _, k := range keys {
		e, ok := entries[k].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("images.%s: want a map with registry, repository and digest", k)
		}

		img := Image{Key: k, Registry: str(e["registry"]), Repository: str(e["repository"]), Tag: str(e["tag"]), Digest: str(e["digest"])}
		if img.Repository == "" {
			return nil, fmt.Errorf("images.%s has no repository", k)
		}

		out = append(out, img)
	}

	return out, nil
}

func str(v any) string {
	s, _ := v.(string)

	return s
}

// Result is the outcome for one image that verified.
type Result struct {
	Image Image
	// Index is true when the digest is an image index or manifest list.
	Index bool
	// Platforms lists the real platforms of an index (attestation entries,
	// whose platform is unknown/unknown, are left out), sorted.
	Platforms []string
}

// String is the one-line report of a verified image.
func (r Result) String() string {
	kind := "manifest"
	if r.Index {
		kind = "index " + strings.Join(r.Platforms, ",")
	}

	return fmt.Sprintf("verified: images.%s %s (%s)", r.Image.Key, r.Image.Ref(), kind)
}

// Verify checks every image and returns the ones that verified together with
// one error naming every image that did not (nil when all did). With no
// platforms an image only has to resolve.
func Verify(ctx context.Context, images []Image, platforms []Platform, opts ocipush.Options) ([]Result, error) {
	var (
		results  []Result
		failures []string
	)

	for _, img := range images {
		res, err := verifyOne(ctx, img, platforms, opts)
		if err != nil {
			failures = append(failures, fmt.Sprintf("images.%s %s: %v", img.Key, img.Ref(), err))

			continue
		}

		results = append(results, res)
	}

	if len(failures) > 0 {
		return results, fmt.Errorf("image verification failed:\n  %s", strings.Join(failures, "\n  "))
	}

	return results, nil
}

func verifyOne(ctx context.Context, img Image, platforms []Platform, opts ocipush.Options) (Result, error) {
	if img.Digest == "" {
		return Result{}, errors.New("has no digest")
	}

	if err := digest.Digest(img.Digest).Validate(); err != nil {
		return Result{}, fmt.Errorf("not a valid digest: %w", err)
	}

	repo, err := remote.NewRepository(img.Repo())
	if err != nil {
		return Result{}, fmt.Errorf("repository: %w", err)
	}

	repo.PlainHTTP = opts.PlainHTTP

	credential := opts.Credential
	if credential == nil {
		credential = ocipush.ReadCredentials(img.Repo(), opts.AWSProfile)
	}

	repo.Client = &auth.Client{Credential: credential}

	desc, body, err := fetch(ctx, repo, img.Digest)
	if err != nil {
		return Result{}, err
	}

	res := Result{Image: img}

	switch desc.MediaType {
	case ocispec.MediaTypeImageIndex, mediaTypeDockerManifestList:
		res.Index = true
	default:
		if len(platforms) > 0 {
			return Result{}, fmt.Errorf("is a single manifest (%s), not an index: want %s", desc.MediaType, platformList(platforms))
		}

		return res, nil
	}

	var index ocispec.Index
	if err := json.Unmarshal(body, &index); err != nil {
		return Result{}, fmt.Errorf("parse index: %w", err)
	}

	have := map[string]ocispec.Descriptor{}

	for _, m := range index.Manifests {
		if m.Platform == nil || m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
			continue // an attestation, not an image
		}

		p := Platform{OS: m.Platform.OS, Arch: m.Platform.Architecture, Variant: m.Platform.Variant}
		have[p.String()] = m
		res.Platforms = append(res.Platforms, p.String())
	}

	sort.Strings(res.Platforms)

	var missing []string

	for _, want := range platforms {
		child, ok := findPlatform(have, want)
		if !ok {
			missing = append(missing, want.String())

			continue
		}

		if _, _, err := fetch(ctx, repo, child.Digest.String()); err != nil {
			return Result{}, fmt.Errorf("%s manifest %s: %w", want, child.Digest, err)
		}
	}

	if len(missing) > 0 {
		return Result{}, fmt.Errorf("index lacks %s (has %s)", strings.Join(missing, ", "), orNone(res.Platforms))
	}

	return res, nil
}

// findPlatform matches os and architecture, and the variant only when asked for one.
func findPlatform(have map[string]ocispec.Descriptor, want Platform) (ocispec.Descriptor, bool) {
	if d, ok := have[want.String()]; ok {
		return d, true
	}

	if want.Variant != "" {
		return ocispec.Descriptor{}, false
	}

	for k, d := range have {
		if strings.HasPrefix(k, want.OS+"/"+want.Arch+"/") {
			return d, true
		}
	}

	return ocispec.Descriptor{}, false
}

func fetch(ctx context.Context, repo *remote.Repository, dgst string) (ocispec.Descriptor, []byte, error) {
	desc, rc, err := repo.Manifests().FetchReference(ctx, dgst)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return ocispec.Descriptor{}, nil, errors.New("not found in the registry")
		}

		return ocispec.Descriptor{}, nil, fmt.Errorf("fetch: %w", err)
	}

	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("read manifest: %w", err)
	}

	if got := digest.FromBytes(body); desc.Digest != "" && got != desc.Digest {
		return ocispec.Descriptor{}, nil, fmt.Errorf("registry served %s for %s", got, desc.Digest)
	}

	return desc, body, nil
}

func platformList(ps []Platform) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}

	return strings.Join(out, ", ")
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "no platform"
	}

	return strings.Join(s, ", ")
}
