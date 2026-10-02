// Package sourcemaps defines the source-map OCI artifact and builds it.
//
// An artifact is one OCI image manifest whose artifactType is
// [ArtifactType], with an empty config and exactly ONE layer of media type
// [LayerMediaType]: a tar.gz holding the `*.map` files of one frontend
// build, each at the path it is requested under (the script URL minus the
// site prefix, plus ".map").
//
// The artifact is addressed by VERSION TAG only: the repository names the
// application and the tag is the release version, sanitised by
// [SanitizeTag]. There is no referrer or subject link to the application
// image.
//
// Packing is deterministic: the same files yield the same layer bytes and
// therefore the same manifest digest, so re-running a release re-pushes an
// identical artifact.
package sourcemaps

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// ArtifactType is the manifest artifactType of a source-map artifact.
	ArtifactType = "application/vnd.ocictl.sourcemaps.v1"
	// LayerMediaType is the media type of the single layer.
	LayerMediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
	// ConfigMediaType is the media type of the (empty) config blob.
	ConfigMediaType = "application/vnd.oci.empty.v1+json"
	// MapSuffix is the only file suffix a served artifact keeps.
	MapSuffix = ".map"
)

// EmptyConfig is the content of the config blob.
var EmptyConfig = []byte("{}")

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// SanitizeTag turns a release version into an OCI tag. "+" (semver build
// metadata, invalid in a tag) becomes "_"; anything that is still not a
// valid tag is refused rather than rewritten, so two different releases can
// never silently share a tag.
//
// The Faro receiver does not escape the release it sends, so a server and a
// publisher must agree on this function; both use it.
func SanitizeTag(version string) (string, error) {
	tag := strings.ReplaceAll(version, "+", "_")
	if !tagPattern.MatchString(tag) {
		return "", fmt.Errorf("version %q is not a valid OCI tag after sanitising (%q); want %s", version, tag, tagPattern)
	}

	return tag, nil
}
