package sourcemaps

import (
	"fmt"
	"strings"

	"github.com/truvity/ocictl/pkg/goreleaserdist"
)

// DefaultRepositoryTemplate places the maps of an image under a sibling
// "sourcemaps" repository of the same owner on the same registry.
const DefaultRepositoryTemplate = "{registry}/{owner}/sourcemaps/{app}"

// SelectImage finds the published image named by name in dist. name matches
// the image's full repository ("owner/web"), its "registry/repository" or its
// last path segment ("web"). Exactly one image must match.
func SelectImage(dist *goreleaserdist.Dist, name string) (goreleaserdist.Image, error) {
	var matches []goreleaserdist.Image

	for _, img := range dist.Images {
		last := img.Repository[strings.LastIndex(img.Repository, "/")+1:]
		if name == img.Repository || name == img.Registry+"/"+img.Repository || name == last {
			matches = append(matches, img)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		known := make([]string, 0, len(dist.Images))
		for _, img := range dist.Images {
			known = append(known, img.Registry+"/"+img.Repository)
		}

		return goreleaserdist.Image{}, fmt.Errorf("--image %q matches no published image (have: %s)", name, strings.Join(known, ", "))
	default:
		return goreleaserdist.Image{}, fmt.Errorf("--image %q is ambiguous (%d images); use registry/repository", name, len(matches))
	}
}

// ExpandRepository fills a repository template from an image. Variables:
//
//	{registry}    the image's registry host
//	{repository}  the image's full repository path
//	{owner}       the first path segment of the repository
//	{app}         app, or the last path segment of the repository
func ExpandRepository(template string, img goreleaserdist.Image, app string) (string, error) {
	segments := strings.Split(img.Repository, "/")
	if app == "" {
		app = segments[len(segments)-1]
	}

	if strings.Contains(template, "{owner}") && len(segments) < 2 {
		return "", fmt.Errorf("template uses {owner} but image repository %q has a single segment", img.Repository)
	}

	out := strings.NewReplacer(
		"{registry}", img.Registry,
		"{repository}", img.Repository,
		"{owner}", segments[0],
		"{app}", app,
	).Replace(template)

	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("repository template %q has an unknown variable", template)
	}

	return out, nil
}
