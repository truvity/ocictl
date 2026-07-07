package goreleaserdist

import (
	"fmt"
	"path"
	"sort"

	"github.com/truvity/ocictl/pkg/helmctl"
)

// ManifestOptions controls Dist → helmctl.Manifest conversion.
type ManifestOptions struct {
	// Images filters (and validates) which images are included, by
	// component key. Empty = all images from the dist.
	Images []string
}

// Manifest converts the parsed dist into a helmctl release manifest:
// version/appVersion from the release, and a `values.images.{component}`
// entry per published image with {registry, repository, tag, digest}.
//
// The component key is the repository's last path segment
// ("url-shortener/redirect" → "redirect"). Two images resolving to the
// same key is an error — use Images to filter, or restructure repositories.
func (d *Dist) Manifest(opts ManifestOptions) (*helmctl.Manifest, error) {
	byKey := map[string]Image{}

	for _, img := range d.Images {
		key := path.Base(img.Repository)

		if existing, dup := byKey[key]; dup {
			return nil, fmt.Errorf(
				"component key %q is ambiguous: %s/%s and %s/%s — filter with --images or rename repositories",
				key, existing.Registry, existing.Repository, img.Registry, img.Repository)
		}

		byKey[key] = img
	}

	if len(opts.Images) > 0 {
		filtered := map[string]Image{}

		for _, key := range opts.Images {
			img, ok := byKey[key]
			if !ok {
				return nil, fmt.Errorf("image %q not found in dist (have: %v)", key, sortedKeys(byKey))
			}

			filtered[key] = img
		}

		byKey = filtered
	}

	images := map[string]any{}
	for key, img := range byKey {
		images[key] = map[string]any{
			"registry":   img.Registry,
			"repository": img.Repository,
			"tag":        img.Tag,
			"digest":     img.Digest,
		}
	}

	m := &helmctl.Manifest{
		Version:    d.Version,
		AppVersion: d.Version,
		Commit:     d.Commit,
	}

	if len(images) > 0 {
		m.Values = map[string]any{"images": images}
	}

	return m, nil
}

func sortedKeys(m map[string]Image) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
