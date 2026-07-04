// Package ocipush provides deterministic OCI artifact push via oras-go.
//
// It wraps the ORAS library to produce reproducible OCI manifests:
// same content always yields the same manifest digest. This is achieved
// by omitting non-deterministic annotations (org.opencontainers.image.created)
// and using a fixed config blob.
//
// Supports both GHCR (Bearer token via GITHUB_TOKEN) and ECR
// (Docker credential helper via AWS_PROFILE).
package ocipush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

type (
	// Artifact describes an OCI artifact to push.
	Artifact struct {
		// Layer is the raw bytes of the single artifact layer (e.g., tar.gz).
		Layer []byte
		// LayerMediaType is the IANA media type for the layer.
		LayerMediaType string
		// Config is the raw bytes of the config blob.
		Config []byte
		// ConfigMediaType is the IANA media type for the config blob.
		ConfigMediaType string
		// Tag is the OCI tag to push (e.g., "1.7.0").
		Tag string
		// Annotations are optional manifest-level annotations.
		// org.opencontainers.image.created is always stripped for determinism.
		Annotations map[string]string
	}

	// PushResult contains the outcome of a successful push.
	PushResult struct {
		// Digest is the manifest digest (e.g., "sha256:abc123...").
		Digest string
	}
)

// Push pushes a deterministic OCI artifact to a remote registry.
//
// Authentication is selected by registry URL:
//   - ghcr.io: uses GITHUB_TOKEN env var as Bearer token
//   - *.dkr.ecr.*.amazonaws.com: uses Docker credential helpers (AWS_PROFILE)
func Push(
	ctx context.Context,
	logger *slog.Logger,
	repoRef string,
	artifact Artifact,
	awsProfile string,
) (*PushResult, error) {
	store := memory.New()

	// Push layer blob.
	layerDesc := DescriptorFromBytes(artifact.LayerMediaType, artifact.Layer)
	if err := store.Push(ctx, layerDesc, bytes.NewReader(artifact.Layer)); err != nil {
		return nil, fmt.Errorf("push layer to store: %w", err)
	}

	// Push config blob.
	configDesc := DescriptorFromBytes(artifact.ConfigMediaType, artifact.Config)
	if err := store.Push(ctx, configDesc, bytes.NewReader(artifact.Config)); err != nil {
		return nil, fmt.Errorf("push config to store: %w", err)
	}

	// Build deterministic manifest.
	manifestData, err := BuildManifest(artifact)
	if err != nil {
		return nil, err
	}

	manifestDesc := DescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestData)); err != nil {
		return nil, fmt.Errorf("push manifest to store: %w", err)
	}

	// Tag the manifest.
	if err := store.Tag(ctx, manifestDesc, artifact.Tag); err != nil {
		return nil, fmt.Errorf("tag manifest: %w", err)
	}

	// Configure remote repository with appropriate auth.
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("create remote repo %q: %w", repoRef, err)
	}

	repo.Client = &auth.Client{
		Credential: resolveCredentials(repoRef, awsProfile),
	}

	// Copy from memory store to remote.
	_, err = oras.Copy(ctx, store, artifact.Tag, repo, artifact.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return nil, fmt.Errorf("push to %s:%s: %w", repoRef, artifact.Tag, err)
	}

	logger.InfoContext(ctx, "OCI artifact pushed",
		slog.String("ref", fmt.Sprintf("%s:%s", repoRef, artifact.Tag)),
		slog.String("digest", manifestDesc.Digest.String()),
	)

	return &PushResult{Digest: manifestDesc.Digest.String()}, nil
}

// BuildManifest constructs a deterministic OCI manifest from an Artifact.
// Strips org.opencontainers.image.created from annotations.
func BuildManifest(artifact Artifact) ([]byte, error) {
	layerDesc := DescriptorFromBytes(artifact.LayerMediaType, artifact.Layer)
	configDesc := DescriptorFromBytes(artifact.ConfigMediaType, artifact.Config)

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}

	if len(artifact.Annotations) > 0 {
		filtered := make(map[string]string, len(artifact.Annotations))
		for k, v := range artifact.Annotations {
			if k == ocispec.AnnotationCreated {
				continue
			}
			filtered[k] = v
		}

		if len(filtered) > 0 {
			manifest.Annotations = filtered
		}
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	return data, nil
}

// DescriptorFromBytes creates an OCI descriptor from raw data.
func DescriptorFromBytes(mediaType string, data []byte) ocispec.Descriptor {
	hash := sha256.Sum256(data)
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.NewDigestFromBytes(digest.SHA256, hash[:]),
		Size:      int64(len(data)),
	}
}

// resolveCredentials selects the credential source based on registry URL.
func resolveCredentials(
	repoRef, awsProfile string,
) func(ctx context.Context, hostport string) (auth.Credential, error) {
	if strings.Contains(repoRef, "ghcr.io") {
		return ghcrCredential
	}

	// ECR: set AWS_PROFILE for docker-credential-ecr-login.
	if awsProfile != "" {
		os.Setenv("AWS_PROFILE", awsProfile) //nolint:errcheck
	}

	return dockerCredential
}

// ghcrCredential returns credentials from the GITHUB_TOKEN env var.
func ghcrCredential(_ context.Context, _ string) (auth.Credential, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return auth.Credential{}, fmt.Errorf("GITHUB_TOKEN is required for ghcr.io push")
	}

	return auth.Credential{
		Username: "token",
		Password: token,
	}, nil
}

// dockerCredential delegates to Docker's credential store (ecr-credential-helper, etc.).
func dockerCredential(ctx context.Context, hostport string) (auth.Credential, error) {
	credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return auth.Credential{}, fmt.Errorf("create credential store: %w", err)
	}

	return credentials.Credential(credStore)(ctx, hostport)
}
