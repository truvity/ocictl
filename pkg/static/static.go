// Package static defines the static-site OCI artifact and builds it.
//
// A static artifact is a directory tree packed into ONE tar+gzip layer
// (files at their paths relative to the directory root, no filtering),
// published in one of two shapes:
//
//   - [ShapeArtifact]: artifactType [ArtifactType], empty config, one layer.
//   - [ShapeImage]: an image config (architecture, os, rootfs.diff_ids; no
//     created, no history) and one layer, so a consumer that accepts any
//     image manifest with a tar(+gzip) layer extracted at the root can use it.
//
// Packing is deterministic: the same tree yields the same layer, config and
// manifest digests, so a release re-run pushes an identical artifact (see
// [Push] for idempotency).
package static

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/truvity/ocictl/pkg/layer"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/sourcemaps"
)

// Shape selects the manifest form.
type Shape string

const (
	// ShapeArtifact is an OCI 1.1 artifact with an empty config.
	ShapeArtifact Shape = "artifact"
	// ShapeImage is an image manifest with an image config.
	ShapeImage Shape = "image"

	// ArtifactType is the manifest artifactType of the artifact shape.
	ArtifactType = "application/vnd.ocictl.static.v1"
	// LayerMediaType is the media type of the single layer.
	LayerMediaType = ocispec.MediaTypeImageLayerGzip
	// EmptyConfigMediaType is the config media type of the artifact shape.
	EmptyConfigMediaType = ocispec.MediaTypeEmptyJSON

	// DefaultArchitecture fills the image config when none is given.
	DefaultArchitecture = "amd64"
	// DefaultOS fills the image config when none is given.
	DefaultOS = "linux"
)

// ParseShape validates a --shape value.
func ParseShape(s string) (Shape, error) {
	switch Shape(s) {
	case ShapeArtifact, ShapeImage:
		return Shape(s), nil
	}

	return "", fmt.Errorf("--shape %q: want %q or %q", s, ShapeArtifact, ShapeImage)
}

// Options shapes the artifact built from a packed tree.
type Options struct {
	Shape Shape
	// Tag is the version tag, sanitised by sourcemaps.SanitizeTag. Empty is
	// allowed only for Describe-style use that never pushes.
	Tag string
	// Architecture and OS fill the image config (image shape only).
	Architecture, OS string
	// Annotations are manifest annotations.
	Annotations map[string]string
}

// Pack packs dir with no file filtering. It refuses a directory with no files.
func Pack(dir string) (*layer.Packed, error) {
	packed, err := layer.Pack(dir, nil)
	if err != nil {
		return nil, err
	}

	if len(packed.Files) == 0 {
		return nil, fmt.Errorf("%s holds no files", dir)
	}

	return packed, nil
}

// Artifact builds the OCI artifact for a packed tree.
func Artifact(packed *layer.Packed, o Options) (ocipush.Artifact, error) {
	tag := o.Tag

	if tag != "" {
		var err error
		if tag, err = sourcemaps.SanitizeTag(tag); err != nil {
			return ocipush.Artifact{}, err
		}
	}

	art := ocipush.Artifact{
		Layer:          packed.Layer,
		LayerMediaType: LayerMediaType,
		Tag:            tag,
		Annotations:    o.Annotations,
	}

	switch o.Shape {
	case ShapeArtifact, "":
		art.Config = sourcemaps.EmptyConfig
		art.ConfigMediaType = EmptyConfigMediaType
		art.ArtifactType = ArtifactType
	case ShapeImage:
		cfg, err := imageConfig(packed, o)
		if err != nil {
			return ocipush.Artifact{}, err
		}

		art.Config = cfg
		art.ConfigMediaType = ocispec.MediaTypeImageConfig
	default:
		return ocipush.Artifact{}, fmt.Errorf("unknown shape %q", o.Shape)
	}

	return art, nil
}

// imageConfig renders the minimal image config. Field order is fixed by the
// struct; there is no created, no history, no config section.
func imageConfig(packed *layer.Packed, o Options) ([]byte, error) {
	type rootfs struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}

	type config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		RootFS       rootfs `json:"rootfs"`
	}

	c := config{
		Architecture: o.Architecture,
		OS:           o.OS,
		RootFS:       rootfs{Type: "layers", DiffIDs: []string{packed.DiffID.String()}},
	}

	if c.Architecture == "" {
		c.Architecture = DefaultArchitecture
	}

	if c.OS == "" {
		c.OS = DefaultOS
	}

	return json.Marshal(c)
}

// Digests are the content addresses of a built artifact.
type Digests struct {
	Layer, Config, Manifest string
}

// Describe returns the digests the artifact will have in a registry.
func Describe(art ocipush.Artifact) (Digests, error) {
	manifest, err := ocipush.BuildManifest(art)
	if err != nil {
		return Digests{}, err
	}

	return Digests{
		Layer:    ocipush.DescriptorFromBytes(art.LayerMediaType, art.Layer).Digest.String(),
		Config:   ocipush.DescriptorFromBytes(art.ConfigMediaType, art.Config).Digest.String(),
		Manifest: ocipush.DescriptorFromBytes(ocispec.MediaTypeImageManifest, manifest).Digest.String(),
	}, nil
}

// Outcome says what Push did.
type Outcome int

const (
	// Pushed means the tag was absent and has been written.
	Pushed Outcome = iota
	// AlreadyPresent means the tag already pointed at this exact manifest.
	AlreadyPresent
)

// Push publishes the artifact to repository (host/path, no tag), idempotently
// against immutable registries: the tag is resolved first. Absent: push.
// Present with the same manifest digest: nothing is written and
// AlreadyPresent is returned. Present with another digest: an error naming
// both digests.
func Push(
	ctx context.Context,
	logger *slog.Logger,
	repository string,
	art ocipush.Artifact,
	opts ocipush.Options,
) (Outcome, string, error) {
	if art.Tag == "" {
		return 0, "", fmt.Errorf("a tag is required to push")
	}

	d, err := Describe(art)
	if err != nil {
		return 0, "", err
	}

	existing, found, err := ocipush.Resolve(ctx, repository, art.Tag, opts)
	if err != nil {
		return 0, "", err
	}

	if found {
		if existing != d.Manifest {
			return 0, "", fmt.Errorf("%s:%s already exists with manifest digest %s; this content has %s (tags are immutable: use a new version)",
				repository, art.Tag, existing, d.Manifest)
		}

		return AlreadyPresent, d.Manifest, nil
	}

	res, err := ocipush.PushWithOptions(ctx, logger, repository, art, opts)
	if err != nil {
		return 0, "", fmt.Errorf("push static: %w", err)
	}

	if res.Digest != d.Manifest {
		return 0, "", fmt.Errorf("pushed digest %s differs from the computed %s", res.Digest, d.Manifest)
	}

	return Pushed, res.Digest, nil
}
