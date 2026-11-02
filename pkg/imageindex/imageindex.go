// Package imageindex merges single-platform images into one OCI image index.
//
// It exists for builds that produce one image per architecture in separate
// jobs, with no daemon and no registry login step beyond a token: each input is
// a digest in a registry, the output is an index tagged under one version.
package imageindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/ocipush"
)

// maxManifestBytes bounds how much of a manifest or config is read.
const maxManifestBytes = 4 << 20

// Input is one image to put in the index.
type Input struct {
	// Repository is host/path of the image, no tag or digest.
	Repository string
	// Digest is the image manifest digest.
	Digest string
	// Platform is the platform to list it under; nil reads it from the
	// image's config.
	Platform *ocispec.Platform
}

var inputRE = regexp.MustCompile(`^([a-z0-9]+:[a-f0-9]{32,})(?::([^:@]+))?$`)

// ParseInput parses <host/path>@<digest>[:<os>/<arch>[/<variant>]]. A tag
// before the @ is dropped.
func ParseInput(s string) (Input, error) {
	repo, rest, ok := strings.Cut(s, "@")
	if !ok || repo == "" {
		return Input{}, fmt.Errorf("--manifest %q: want <host/path>@<digest>[:<os>/<arch>[/<variant>]]", s)
	}

	m := inputRE.FindStringSubmatch(rest)
	if m == nil {
		return Input{}, fmt.Errorf("--manifest %q: want a digest after @, then optionally :<os>/<arch>[/<variant>]", s)
	}

	if err := digest.Digest(m[1]).Validate(); err != nil {
		return Input{}, fmt.Errorf("--manifest %q: %w", s, err)
	}

	// A tag before the @ is ignored: the digest names the image.
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		if c := strings.Index(repo[i+1:], ":"); c >= 0 {
			repo = repo[:i+1+c]
		}
	}

	in := Input{Repository: repo, Digest: m[1]}

	if m[2] != "" {
		parts := strings.Split(m[2], "/")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
			return Input{}, fmt.Errorf("--manifest %q: platform %q: want os/arch or os/arch/variant", s, m[2])
		}

		in.Platform = &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) == 3 {
			in.Platform.Variant = parts[2]
		}
	}

	return in, nil
}

func platformKey(p *ocispec.Platform) string {
	k := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		k += "/" + p.Variant
	}

	return k
}

// Spec describes the index to create.
type Spec struct {
	// Repository and Tag say where the index goes.
	Repository, Tag string
	Inputs          []Input
	Annotations     map[string]string
	// Created, when non-zero, is the index's org.opencontainers.image.created.
	Created time.Time
}

// Result is what Create did.
type Result struct {
	Repository, Tag string
	// Digest is the index digest.
	Digest string
	// AlreadyPresent is true when the tag already held this exact index.
	AlreadyPresent bool
	// Platforms lists what the index holds, sorted.
	Platforms []string
}

// Build resolves the inputs and returns the index document and its digest, with
// nothing written to the registry. Each input must be a single-platform image
// manifest present in its registry; platforms must be unique.
func Build(ctx context.Context, spec Spec, opts ocipush.Options) ([]byte, ocispec.Descriptor, []ocispec.Descriptor, error) {
	if len(spec.Inputs) == 0 {
		return nil, ocispec.Descriptor{}, nil, errors.New("at least one --manifest is required")
	}

	var (
		entries   []ocispec.Descriptor
		platforms = map[string]string{}
		digests   = map[string]bool{}
	)

	for _, in := range spec.Inputs {
		if digests[in.Digest] {
			return nil, ocispec.Descriptor{}, nil, fmt.Errorf("%s is listed twice", in.Digest)
		}

		digests[in.Digest] = true

		repo, err := open(in.Repository, opts)
		if err != nil {
			return nil, ocispec.Descriptor{}, nil, err
		}

		desc, body, err := fetch(ctx, repo, in.Digest)
		if err != nil {
			return nil, ocispec.Descriptor{}, nil, fmt.Errorf("%s@%s: %w", in.Repository, in.Digest, err)
		}

		if desc.MediaType != ocispec.MediaTypeImageManifest && desc.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			return nil, ocispec.Descriptor{}, nil, fmt.Errorf("%s@%s is a %s: want a single-platform image manifest", in.Repository, in.Digest, desc.MediaType)
		}

		platform := in.Platform
		if platform == nil {
			if platform, err = platformFromConfig(ctx, repo, body); err != nil {
				return nil, ocispec.Descriptor{}, nil, fmt.Errorf("%s@%s: %w (name it with :<os>/<arch>)", in.Repository, in.Digest, err)
			}
		}

		key := platformKey(platform)
		if prev, dup := platforms[key]; dup {
			return nil, ocispec.Descriptor{}, nil, fmt.Errorf("platform %s appears twice: %s and %s", key, prev, in.Digest)
		}

		platforms[key] = in.Digest

		entries = append(entries, ocispec.Descriptor{
			MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size, Platform: platform,
		})
	}

	sort.Slice(entries, func(i, j int) bool { return platformKey(entries[i].Platform) < platformKey(entries[j].Platform) })

	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: entries,
	}

	annotations := make(map[string]string, len(spec.Annotations)+1)

	for k, v := range spec.Annotations {
		if k != ocispec.AnnotationCreated { // only from Created, for determinism
			annotations[k] = v
		}
	}

	if !spec.Created.IsZero() {
		annotations[ocispec.AnnotationCreated] = spec.Created.UTC().Truncate(time.Second).Format(time.RFC3339)
	}

	if len(annotations) > 0 {
		idx.Annotations = annotations
	}

	data, err := json.Marshal(idx)
	if err != nil {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("marshal index: %w", err)
	}

	return data, ocipush.DescriptorFromBytes(ocispec.MediaTypeImageIndex, data), entries, nil
}

// Create builds the index and publishes it as Repository:Tag, idempotently:
// absent, push; present with the same digest, write nothing; present with
// another digest, an error naming both. Inputs from another repository are
// copied into Repository first (blobs by cross-repository mount where the
// registry allows it).
func Create(ctx context.Context, logger *slog.Logger, spec Spec, opts ocipush.Options) (*Result, error) {
	if spec.Tag == "" || spec.Repository == "" {
		return nil, errors.New("--repository and --tag are required")
	}

	data, desc, entries, err := Build(ctx, spec, opts)
	if err != nil {
		return nil, err
	}

	res := &Result{Repository: spec.Repository, Tag: spec.Tag, Digest: desc.Digest.String()}
	for _, e := range entries {
		res.Platforms = append(res.Platforms, platformKey(e.Platform))
	}

	existing, found, err := ocipush.Resolve(ctx, spec.Repository, spec.Tag, opts)
	if err != nil {
		return nil, err
	}

	if found {
		if existing != res.Digest {
			return nil, fmt.Errorf("%s:%s already exists with manifest digest %s; this index has %s (tags are immutable: use a new version)",
				spec.Repository, spec.Tag, existing, res.Digest)
		}

		res.AlreadyPresent = true

		return res, nil
	}

	dst, err := open(spec.Repository, opts)
	if err != nil {
		return nil, err
	}

	for _, in := range spec.Inputs {
		if in.Repository == spec.Repository {
			continue
		}

		if err := copyInto(ctx, logger, dst, in, opts); err != nil {
			return nil, err
		}
	}

	if err := dst.Manifests().PushReference(ctx, desc, bytes.NewReader(data), spec.Tag); err != nil {
		return nil, fmt.Errorf("push index to %s:%s: %w", spec.Repository, spec.Tag, err)
	}

	logger.InfoContext(ctx, "OCI image index pushed", slog.String("ref", spec.Repository+":"+spec.Tag), slog.String("digest", res.Digest))

	return res, nil
}

// copyInto copies the image in (and what it references) into dst by digest.
func copyInto(ctx context.Context, logger *slog.Logger, dst *remote.Repository, in Input, opts ocipush.Options) error {
	src, err := open(in.Repository, opts)
	if err != nil {
		return err
	}

	copyOpts := oras.DefaultCopyOptions
	copyOpts.MountFrom = func(context.Context, ocispec.Descriptor) ([]string, error) {
		// Only same-registry sources can be mounted; others fall back to a copy.
		if src.Reference.Registry != dst.Reference.Registry {
			return nil, nil
		}

		return []string{src.Reference.Repository}, nil
	}

	if _, err := oras.Copy(ctx, src, in.Digest, dst, in.Digest, copyOpts); err != nil {
		return fmt.Errorf("copy %s@%s into %s: %w", in.Repository, in.Digest, dst.Reference.Repository, err)
	}

	logger.InfoContext(ctx, "copied image into the index repository", slog.String("from", in.Repository), slog.String("digest", in.Digest))

	return nil
}

func open(repoRef string, opts ocipush.Options) (*remote.Repository, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", repoRef, err)
	}

	repo.PlainHTTP = opts.PlainHTTP

	credential := opts.Credential
	if credential == nil {
		credential = ocipush.WriteCredentials(repoRef, opts.AWSProfile)
	}

	repo.Client = &auth.Client{Credential: credential}

	return repo, nil
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

// platformFromConfig reads os, architecture and variant from an image
// manifest's config blob.
func platformFromConfig(ctx context.Context, repo *remote.Repository, manifestBody []byte) (*ocispec.Platform, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(manifestBody, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	rc, err := repo.Blobs().Fetch(ctx, m.Config)
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}

	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}

	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.OS == "" || cfg.Architecture == "" {
		return nil, errors.New("the image config names no os/architecture")
	}

	return &ocispec.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}, nil
}
