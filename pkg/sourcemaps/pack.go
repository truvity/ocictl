package sourcemaps

import (
	"fmt"
	"path"
	"strings"

	"github.com/truvity/ocictl/pkg/layer"
)

// Packed is a ready-to-push layer.
type Packed = layer.Packed

// PackDir packs the files below dir into the deterministic layer.
//
// Only regular files ending in ".map" are accepted. Any other regular file
// makes PackDir fail unless it matches one of include (a path.Match pattern,
// tried against the slash-separated path relative to dir and, when the
// pattern has no "/", against the base name). Symlinks and other special
// files are always refused, and so is a directory holding no ".map" file.
func PackDir(dir string, include []string) (*Packed, error) {
	for _, pattern := range include {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("--include %q: %w", pattern, err)
		}
	}

	packed, err := layer.Pack(dir, func(rel string) (bool, error) {
		if !strings.HasSuffix(rel, MapSuffix) && !matchesAny(include, rel) {
			return false, fmt.Errorf("%s: not a %s file; allow it explicitly with --include", rel, MapSuffix)
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	for _, f := range packed.Files {
		if strings.HasSuffix(f, MapSuffix) {
			return packed, nil
		}
	}

	return nil, fmt.Errorf("%s holds no %s file", dir, MapSuffix)
}

func matchesAny(patterns []string, rel string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}

		if !strings.Contains(p, "/") {
			if ok, _ := path.Match(p, path.Base(rel)); ok {
				return true
			}
		}
	}

	return false
}
