// Package goreleaserdist parses a GoReleaser dist/ directory into the
// release facts needed for deterministic Helm chart packaging: the release
// version and the multi-arch image digests of every published image.
//
// It is the ONLY GoReleaser-aware piece of ocictl: everything downstream
// (helmctl) consumes the neutral Manifest produced from it, so builds not
// driven by GoReleaser can produce the same manifest by other means.
//
// Sources (both written by `goreleaser release`, complete once the process
// exits):
//
//   - artifacts.json — image artifacts with their published digests:
//     ko images appear as type "Docker Manifest" (name "repo@sha256:…"),
//     dockers_v2 images as type "Docker Image" per tag (name "repo:tag").
//     For multi-platform images both record the manifest-INDEX digest —
//     the correct reference for a chart. digests.txt is ignored: it lists
//     only dockers_v2 images.
//   - metadata.json — project version, git tag and commit.
package goreleaserdist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type (
	// Image is one published container image (multi-arch index).
	Image struct {
		// Registry host, e.g. "721506300184.dkr.ecr.eu-central-1.amazonaws.com".
		Registry string `json:"registry" yaml:"registry"`
		// Repository path within the registry, e.g. "url-shortener/redirect".
		Repository string `json:"repository" yaml:"repository"`
		// Tag is the human-readable version tag (never "latest" when a
		// version tag exists). May be empty for digest-only artifacts.
		Tag string `json:"tag" yaml:"tag"`
		// Digest is the manifest-index digest, e.g. "sha256:…". Always set.
		Digest string `json:"digest" yaml:"digest"`
	}

	// Dist is the parsed release information of one GoReleaser run.
	Dist struct {
		// Version is the release version without "v" prefix, e.g. "1.6.1".
		Version string
		// Tag is the git tag, e.g. "url-shortener/v1.6.1" or "v1.6.1".
		Tag string
		// Commit is the git commit SHA the release was built from.
		Commit string
		// Images are the published images, sorted by repository.
		Images []Image
	}

	// artifact mirrors the artifacts.json entries we care about.
	artifact struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Extra struct {
			Digest string `json:"Digest"`
		} `json:"extra"`
	}

	// metadata mirrors metadata.json.
	metadata struct {
		Version string `json:"version"`
		Tag     string `json:"tag"`
		Commit  string `json:"commit"`
	}
)

// Load parses a GoReleaser dist directory (artifacts.json + metadata.json).
// Every discovered image is guaranteed to carry a digest; images published
// under multiple tags are deduplicated, preferring a non-"latest" tag.
func Load(distDir string) (*Dist, error) {
	meta, err := loadMetadata(filepath.Join(distDir, "metadata.json"))
	if err != nil {
		return nil, err
	}

	images, err := loadImages(filepath.Join(distDir, "artifacts.json"), meta.Version)
	if err != nil {
		return nil, err
	}

	return &Dist{
		Version: meta.Version,
		Tag:     meta.Tag,
		Commit:  meta.Commit,
		Images:  images,
	}, nil
}

func loadMetadata(path string) (*metadata, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller-provided dist dir
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if meta.Version == "" {
		return nil, fmt.Errorf("%s: version is empty", path)
	}

	return &meta, nil
}

// loadImages extracts published images from artifacts.json. version is used
// as the Tag for digest-only artifacts (ko "Docker Manifest" entries carry
// no tag in their name; GoReleaser tags them with the release version).
func loadImages(path, version string) ([]Image, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller-provided dist dir
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var artifacts []artifact
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// ref (registry/repository) → best Image seen so far.
	byRef := map[string]Image{}

	for _, a := range artifacts {
		if a.Type != "Docker Image" && a.Type != "Docker Manifest" {
			continue
		}

		img, err := parseImageArtifact(a, version)
		if err != nil {
			return nil, fmt.Errorf("%s: artifact %q: %w", path, a.Name, err)
		}

		ref := img.Registry + "/" + img.Repository

		existing, seen := byRef[ref]
		if !seen {
			byRef[ref] = img
			continue
		}

		if existing.Digest != img.Digest {
			return nil, fmt.Errorf("%s: image %s published with conflicting digests (%s vs %s)",
				path, ref, existing.Digest, img.Digest)
		}

		// Prefer a concrete version tag over "latest" or empty.
		if betterTag(img.Tag, existing.Tag) {
			byRef[ref] = img
		}
	}

	images := make([]Image, 0, len(byRef))
	for _, img := range byRef {
		images = append(images, img)
	}

	sort.Slice(images, func(i, j int) bool { return images[i].Repository < images[j].Repository })

	return images, nil
}

// parseImageArtifact splits an artifact name into registry/repository/tag
// and pairs it with the recorded digest.
//
//	Docker Image:    "host/path/name:tag"          (dockers_v2, one per tag)
//	Docker Manifest: "host/path/name@sha256:…"     (ko, digest-only)
func parseImageArtifact(a artifact, version string) (Image, error) {
	if a.Extra.Digest == "" {
		return Image{}, fmt.Errorf("no digest recorded — was the image published?")
	}

	name := a.Name
	tag := ""

	if at := strings.Index(name, "@"); at >= 0 {
		// Digest-named (ko): the digest in the name must match extra.Digest.
		name = name[:at]
		tag = version
	} else if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		tag = name[colon+1:]
		name = name[:colon]
	}

	slash := strings.Index(name, "/")
	if slash < 0 {
		return Image{}, fmt.Errorf("image reference %q has no registry host", a.Name)
	}

	return Image{
		Registry:   name[:slash],
		Repository: name[slash+1:],
		Tag:        tag,
		Digest:     a.Extra.Digest,
	}, nil
}

// betterTag reports whether candidate is a better human-readable tag than
// current: any concrete tag beats "latest", which beats empty.
func betterTag(candidate, current string) bool {
	rank := func(t string) int {
		switch t {
		case "":
			return 0
		case "latest":
			return 1
		default:
			return 2
		}
	}

	return rank(candidate) > rank(current)
}
