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
// overrides it. The push is idempotent (see [ocipush.PushIdempotent]): the
// same content under an existing tag is left alone, different content is an
// error unless opts.AllowOverwrite is set. It returns what was done and the
// manifest digest.
func Push(
	ctx context.Context,
	logger *slog.Logger,
	repository string,
	artifact ocipush.Artifact,
	opts ocipush.Options,
) (ocipush.Outcome, string, error) {
	outcome, dgst, err := ocipush.PushIdempotent(ctx, logger, repository, artifact, opts)
	if err != nil {
		return 0, "", fmt.Errorf("push source maps: %w", err)
	}

	return outcome, dgst, nil
}
