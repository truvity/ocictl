package sourcemaps

import (
	"context"
	"fmt"
	"log/slog"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/truvity/ocictl/pkg/ocipush"
)

// Artifact builds the deterministic OCI artifact for a packed layer. version
// is the release version as given (it is recorded unsanitised in the
// org.opencontainers.image.version annotation); the tag is its sanitised
// form.
func Artifact(packed *Packed, version string) (ocipush.Artifact, error) {
	tag, err := SanitizeTag(version)
	if err != nil {
		return ocipush.Artifact{}, err
	}

	return ocipush.Artifact{
		Layer:           packed.Layer,
		LayerMediaType:  LayerMediaType,
		Config:          EmptyConfig,
		ConfigMediaType: ConfigMediaType,
		ArtifactType:    ArtifactType,
		Tag:             tag,
		Annotations:     map[string]string{ocispec.AnnotationVersion: version},
	}, nil
}

// ManifestDigest returns the digest the artifact will have in a registry.
func ManifestDigest(artifact ocipush.Artifact) (string, error) {
	data, err := ocipush.BuildManifest(artifact)
	if err != nil {
		return "", err
	}

	return ocipush.DescriptorFromBytes(ocispec.MediaTypeImageManifest, data).Digest.String(), nil
}

// Push pushes the artifact to repository (host/path, no tag) under its
// version tag, using pkg/ocipush's registry authentication unless opts
// overrides it.
func Push(
	ctx context.Context,
	logger *slog.Logger,
	repository string,
	artifact ocipush.Artifact,
	opts ocipush.Options,
) (string, error) {
	res, err := ocipush.PushWithOptions(ctx, logger, repository, artifact, opts)
	if err != nil {
		return "", fmt.Errorf("push source maps: %w", err)
	}

	return res.Digest, nil
}
