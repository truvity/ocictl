package ocipush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

// ReferrersMode says how a registry lists the referrers of a manifest.
type ReferrersMode string

const (
	// ModeAPI is the OCI 1.1 referrers API: GET /v2/<name>/referrers/<digest>.
	ModeAPI ReferrersMode = "referrers-api"
	// ModeTagSchema is the fallback of OCI distribution 1.1: an image index
	// tagged sha256-<hex of the subject digest> that lists the referrers. GHCR
	// has no referrers API (measured: 404), so everything attached there lives here.
	ModeTagSchema ReferrersMode = "tag-schema"
)

// EmptyConfig is the body of the OCI 1.1 empty descriptor.
var EmptyConfig = []byte("{}")

// ReferrersSettle is how long a tag-schema writer waits before it reads the
// index back to see that nobody overwrote its entry. A registry tag cannot be
// written conditionally, so two writers that read the index at the same time
// both write, and the second write loses the first writer's entry; the
// read-back is what finds that out, and the loser then repeats the
// read-modify-write. It is a variable only so that tests can shorten it.
var ReferrersSettle = 300 * time.Millisecond

// referrersAttempts bounds the read-modify-write loop of the tag schema.
const referrersAttempts = 10

// AttachResult is what Attach did.
type AttachResult struct {
	// Digest is the digest of the referrer manifest.
	Digest string
	// Subject is the manifest the artifact refers to.
	Subject ocispec.Descriptor
	// Mode says how the registry lists it.
	Mode ReferrersMode
}

// ReferrersTag is the tag under which a registry without the referrers API
// lists the referrers of subject: sha256-<hex>.
func ReferrersTag(subject digest.Digest) string {
	return subject.Algorithm().String() + "-" + subject.Encoded()
}

// Attach pushes art as a referrer of the manifest subjectDigest in repoRef
// (host/path, no tag): art's blobs, an empty config unless art has one, and a
// manifest whose subject is that manifest.
//
// On a registry with the referrers API the registry lists the new manifest by
// itself, and Attach reads the list to check. On one without it (GHCR) Attach
// adds the manifest to the sha256-<hex> index with a read-modify-write, reads
// the index back after ReferrersSettle, and repeats when another writer's
// update removed the entry, up to a bound. Attaching the same artifact again
// is a no-op.
func Attach(
	ctx context.Context,
	logger *slog.Logger,
	repoRef, subjectDigest string,
	art Artifact,
	opts Options,
) (*AttachResult, error) {
	subjectDgst, err := digest.Parse(subjectDigest)
	if err != nil {
		return nil, fmt.Errorf("subject %q: want a digest (sha256:...): %w", subjectDigest, err)
	}

	if art.ArtifactType == "" {
		return nil, errors.New("an artifact type is required to attach")
	}

	repo, err := newRepository(repoRef, opts)
	if err != nil {
		return nil, err
	}

	repo.SkipReferrersGC = true

	subject, err := repo.Resolve(ctx, subjectDgst.String())
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, fmt.Errorf("subject %s@%s not found in the registry", repoRef, subjectDgst)
		}

		return nil, fmt.Errorf("resolve subject %s@%s: %w", repoRef, subjectDgst, err)
	}

	subject = ocispec.Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size}
	art.Subject = &subject

	if len(art.Config) == 0 {
		art.Config, art.ConfigMediaType = EmptyConfig, ocispec.MediaTypeEmptyJSON
	}

	if len(art.Blobs) == 0 && len(art.Layer) == 0 {
		// OCI 1.1: an artifact without content has a single empty layer.
		art.Blobs = []Blob{{MediaType: ocispec.MediaTypeEmptyJSON, Data: EmptyConfig}}
	}

	manifestData, err := BuildManifest(art)
	if err != nil {
		return nil, err
	}

	for i, data := range art.layerData() {
		if err := pushBlob(ctx, repo, layerDescriptors(art)[i], data); err != nil {
			return nil, err
		}
	}

	if err := pushBlob(ctx, repo, DescriptorFromBytes(art.ConfigMediaType, art.Config), art.Config); err != nil {
		return nil, err
	}

	entry := DescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestData)
	entry.ArtifactType = art.ArtifactType

	var built ocispec.Manifest
	if err := json.Unmarshal(manifestData, &built); err != nil {
		return nil, fmt.Errorf("decode built manifest: %w", err)
	}

	entry.Annotations = built.Annotations

	if err := repo.Manifests().Push(ctx, entry, bytes.NewReader(manifestData)); err != nil {
		return nil, fmt.Errorf("push referrer manifest to %s: %w", repoRef, err)
	}

	api, err := referrersAPI(ctx, repo, subject.Digest)
	if err != nil {
		return nil, err
	}

	if api {
		listed, err := listAPI(ctx, repo, subject, "")
		if err != nil {
			return nil, err
		}

		if !contains(listed, entry.Digest) {
			return nil, fmt.Errorf("the registry accepted %s but does not list it as a referrer of %s", entry.Digest, subject.Digest)
		}

		logAttached(ctx, logger, subject, entry, ModeAPI)

		return &AttachResult{Digest: entry.Digest.String(), Subject: subject, Mode: ModeAPI}, nil
	}

	if err := ensureInTagSchema(ctx, repo, subject.Digest, entry); err != nil {
		return nil, err
	}

	logAttached(ctx, logger, subject, entry, ModeTagSchema)

	return &AttachResult{Digest: entry.Digest.String(), Subject: subject, Mode: ModeTagSchema}, nil
}

// Referrers lists the manifests that refer to subjectDigest in repoRef, from
// the referrers API when the registry has it, else from the tag schema. A
// non-empty artifactType keeps only referrers of that type.
func Referrers(
	ctx context.Context,
	repoRef, subjectDigest, artifactType string,
	opts Options,
) ([]ocispec.Descriptor, ReferrersMode, error) {
	dgst, err := digest.Parse(subjectDigest)
	if err != nil {
		return nil, "", fmt.Errorf("subject %q: want a digest (sha256:...): %w", subjectDigest, err)
	}

	repo, err := newRepository(repoRef, opts)
	if err != nil {
		return nil, "", err
	}

	api, err := referrersAPI(ctx, repo, dgst)
	if err != nil {
		return nil, "", err
	}

	subject := ocispec.Descriptor{Digest: dgst}

	if api {
		out, err := listAPI(ctx, repo, subject, artifactType)

		return out, ModeAPI, err
	}

	out, err := readTagIndex(ctx, repo, ReferrersTag(dgst))
	if err != nil {
		return nil, "", err
	}

	return filterByType(out, artifactType), ModeTagSchema, nil
}

func logAttached(ctx context.Context, logger *slog.Logger, subject, entry ocispec.Descriptor, mode ReferrersMode) {
	logger.InfoContext(ctx, "attached",
		slog.String("subject", subject.Digest.String()), slog.String("digest", entry.Digest.String()), slog.String("mode", string(mode)))
}

func pushBlob(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, data []byte) error {
	exists, err := repo.Blobs().Exists(ctx, desc)
	if err != nil {
		return fmt.Errorf("check blob %s: %w", desc.Digest, err)
	}

	if exists {
		return nil
	}

	if err := repo.Blobs().Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("push blob %s: %w", desc.Digest, err)
	}

	return nil
}

// referrersAPI reports whether the registry serves GET /v2/<name>/referrers/<digest>.
// A registry that does returns an image index (200), even for a subject with no
// referrers; one that does not answers 404.
func referrersAPI(ctx context.Context, repo *remote.Repository, subject digest.Digest) (bool, error) {
	scheme := "https"
	if repo.PlainHTTP {
		scheme = "http"
	}

	url := fmt.Sprintf("%s://%s/v2/%s/referrers/%s", scheme, repo.Reference.Registry, repo.Reference.Repository, subject)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	resp, err := repo.Client.Do(req)
	if err != nil {
		return false, fmt.Errorf("probe the referrers API: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), ocispec.MediaTypeImageIndex):
		return true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusOK:
		return false, nil
	default:
		return false, fmt.Errorf("probe the referrers API: %s", resp.Status)
	}
}

func listAPI(ctx context.Context, repo *remote.Repository, subject ocispec.Descriptor, artifactType string) ([]ocispec.Descriptor, error) {
	var out []ocispec.Descriptor

	err := repo.Referrers(ctx, subject, artifactType, func(refs []ocispec.Descriptor) error {
		out = append(out, refs...)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list referrers: %w", err)
	}

	return out, nil
}

// readTagIndex reads the referrers index at tag; a missing tag is an empty list.
func readTagIndex(ctx context.Context, repo *remote.Repository, tag string) ([]ocispec.Descriptor, error) {
	desc, rc, err := repo.Manifests().FetchReference(ctx, tag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("read referrers index %s: %w", tag, err)
	}

	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read referrers index %s: %w", tag, err)
	}

	var idx ocispec.Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("decode referrers index %s (%s): %w", tag, desc.MediaType, err)
	}

	return idx.Manifests, nil
}

// ensureInTagSchema makes entry part of the referrers index of subject and
// confirms it by reading the index back.
func ensureInTagSchema(ctx context.Context, repo *remote.Repository, subject digest.Digest, entry ocispec.Descriptor) error {
	tag := ReferrersTag(subject)

	for attempt := 1; attempt <= referrersAttempts; attempt++ {
		current, err := readTagIndex(ctx, repo, tag)
		if err != nil {
			return err
		}

		merged := mergeReferrers(current, entry)
		if len(merged) != len(current) {
			if err := writeTagIndex(ctx, repo, tag, merged); err != nil {
				return err
			}
		}

		if err := sleep(ctx, ReferrersSettle+time.Duration(rand.Int64N(int64(ReferrersSettle/2)+1))); err != nil {
			return err
		}

		after, err := readTagIndex(ctx, repo, tag)
		if err != nil {
			return err
		}

		if containsAll(after, merged) {
			return nil
		}
		// Another writer replaced the index between our write and the
		// read-back, dropping an entry: read, merge and write again.
	}

	return fmt.Errorf("could not confirm %s in the referrers index %s after %d attempts: other writers kept replacing it", entry.Digest, tag, referrersAttempts)
}

func writeTagIndex(ctx context.Context, repo *remote.Repository, tag string, refs []ocispec.Descriptor) error {
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: refs,
	}

	data, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshal referrers index: %w", err)
	}

	desc := DescriptorFromBytes(ocispec.MediaTypeImageIndex, data)

	if err := repo.Manifests().PushReference(ctx, desc, bytes.NewReader(data), tag); err != nil {
		return fmt.Errorf("write referrers index %s: %w", tag, err)
	}

	return nil
}

// mergeReferrers returns current plus entry (when absent), sorted by digest so
// the index is the same whatever the order of the writers.
func mergeReferrers(current []ocispec.Descriptor, entry ocispec.Descriptor) []ocispec.Descriptor {
	out := make([]ocispec.Descriptor, 0, len(current)+1)
	seen := false

	for _, d := range current {
		if d.Digest == entry.Digest {
			seen = true
		}

		out = append(out, d)
	}

	if !seen {
		out = append(out, entry)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })

	return out
}

func contains(list []ocispec.Descriptor, d digest.Digest) bool {
	for _, x := range list {
		if x.Digest == d {
			return true
		}
	}

	return false
}

func containsAll(list, want []ocispec.Descriptor) bool {
	for _, w := range want {
		if !contains(list, w.Digest) {
			return false
		}
	}

	return true
}

func filterByType(list []ocispec.Descriptor, artifactType string) []ocispec.Descriptor {
	if artifactType == "" {
		return list
	}

	var out []ocispec.Descriptor

	for _, d := range list {
		if d.ArtifactType == artifactType {
			out = append(out, d)
		}
	}

	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
