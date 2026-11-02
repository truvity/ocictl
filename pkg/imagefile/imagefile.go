// Package imagefile reads one file out of a remote image without a daemon:
// the file as the flattened image has it, honouring whiteouts.
package imagefile

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/truvity/ocictl/pkg/ocipush"
)

const (
	maxManifestBytes = 4 << 20

	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerLayerGzip    = "application/vnd.docker.image.rootfs.diff.tar.gzip"

	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// ErrNotFound means the path is not in the flattened image: it never existed,
// or a whiteout in a later layer removed it.
var ErrNotFound = errors.New("not found in the image")

// NotFoundError says which path was missing and, when a whiteout is why, where.
type NotFoundError struct {
	Path string
	// Whiteout names the layer digest whose whiteout hid the path, if any.
	Whiteout string
}

func (e *NotFoundError) Error() string {
	if e.Whiteout != "" {
		return fmt.Sprintf("%s: hidden by a whiteout in layer %s", e.Path, e.Whiteout)
	}

	return fmt.Sprintf("%s: %s", e.Path, ErrNotFound)
}

// Unwrap lets errors.Is(err, ErrNotFound) match.
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// ProcessExitCode is the exit status the command uses for a missing path (2).
func (e *NotFoundError) ProcessExitCode() int { return 2 }

// Image selects an image and, for an index, a platform.
type Image struct {
	// Repository is host/path, no tag or digest.
	Repository string
	// Digest is the manifest or index digest.
	Digest string
	// Platform is os/arch[/variant] for an index; empty is accepted only when
	// the index holds one image.
	Platform string
}

// Result describes the file found.
type Result struct {
	// Digest is sha256:<hex> of the file's content.
	Digest string
	Size   int64
	Mode   int64
	// Layer is the digest of the layer the file comes from.
	Layer string
}

// Open finds p in the flattened image and streams its content to w (which may
// be io.Discard). The path is absolute or relative to the root; it must name a
// regular file.
func Open(ctx context.Context, img Image, p string, w io.Writer, opts ocipush.Options) (*Result, error) {
	rel, err := cleanPath(p)
	if err != nil {
		return nil, err
	}

	repo, err := open(img.Repository, opts)
	if err != nil {
		return nil, err
	}

	manifest, err := resolveManifest(ctx, repo, img)
	if err != nil {
		return nil, err
	}

	hiddenBy := ""

	for i := len(manifest.Layers) - 1; i >= 0; i-- {
		layer := manifest.Layers[i]

		res, hides, err := scanLayer(ctx, repo, layer, rel, w)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", layer.Digest, err)
		}

		if res != nil {
			res.Layer = layer.Digest.String()

			return res, nil
		}

		if hides {
			hiddenBy = layer.Digest.String()

			break
		}
	}

	return nil, &NotFoundError{Path: "/" + rel, Whiteout: hiddenBy}
}

// cleanPath turns /usr/bin/x or usr/bin/x into usr/bin/x.
func cleanPath(p string) (string, error) {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == "" || rel == "." {
		return "", errors.New("--path names the root, not a file")
	}

	return rel, nil
}

// scanLayer looks for rel in one layer. It returns the file when the layer has
// it as a regular file (its content copied to w), or hides=true when the layer
// carries a whiteout that removes rel (or an ancestor) from the layers below.
func scanLayer(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, rel string, w io.Writer) (res *Result, hides bool, err error) {
	// Two passes over the layer only when the file is a hard link to another
	// entry; otherwise one.
	link, res, hides, err := scanOnce(ctx, repo, layer, rel, w)
	if err != nil || res != nil || hides || link == "" {
		return res, hides, err
	}

	_, res, _, err = scanOnce(ctx, repo, layer, link, w)
	if err == nil && res == nil {
		err = fmt.Errorf("hard link target %s is not in the same layer", link)
	}

	return res, false, err
}

// scanOnce reads the layer's tar for want. For a hard link it returns the
// link's target path instead of content.
func scanOnce(
	ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, want string, w io.Writer,
) (link string, res *Result, hides bool, err error) {
	rc, err := repo.Blobs().Fetch(ctx, layer)
	if err != nil {
		return "", nil, false, fmt.Errorf("fetch: %w", err)
	}

	defer rc.Close() //nolint:errcheck

	var r io.Reader = rc

	switch layer.MediaType {
	case ocispec.MediaTypeImageLayerGzip, mediaTypeDockerLayerGzip, ocispec.MediaTypeImageLayerNonDistributableGzip: //nolint:staticcheck // still seen in the wild
		gz, gerr := gzip.NewReader(rc)
		if gerr != nil {
			return "", nil, false, fmt.Errorf("gzip: %w", gerr)
		}

		defer gz.Close() //nolint:errcheck

		r = gz
	case ocispec.MediaTypeImageLayer:
	default:
		return "", nil, false, fmt.Errorf("unsupported layer media type %q (gzip and uncompressed tar are read)", layer.MediaType)
	}

	tr := tar.NewReader(r)

	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}

		if nerr != nil {
			return "", nil, false, fmt.Errorf("tar: %w", nerr)
		}

		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")

		switch {
		case base == opaqueMarker:
			// Everything under dir in lower layers is gone.
			if dir == "" || strings.HasPrefix(want, dir+"/") {
				hides = true
			}
		case strings.HasPrefix(base, whiteoutPrefix):
			gone := path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))
			if want == gone || strings.HasPrefix(want, gone+"/") {
				hides = true
			}
		case name == want:
			switch hdr.Typeflag {
			case tar.TypeReg:
				sum := sha256.New()

				n, cerr := io.Copy(io.MultiWriter(sum, w), tr)
				if cerr != nil {
					return "", nil, false, fmt.Errorf("read %s: %w", want, cerr)
				}

				return "", &Result{Digest: "sha256:" + hex.EncodeToString(sum.Sum(nil)), Size: n, Mode: hdr.Mode}, false, nil
			case tar.TypeLink:
				return strings.TrimPrefix(path.Clean("/"+hdr.Linkname), "/"), nil, false, nil
			case tar.TypeDir:
				return "", nil, false, fmt.Errorf("/%s is a directory", want)
			case tar.TypeSymlink:
				return "", nil, false, fmt.Errorf("/%s is a symlink to %s (links are not followed)", want, hdr.Linkname)
			default:
				return "", nil, false, fmt.Errorf("/%s is not a regular file (tar type %q)", want, string(hdr.Typeflag))
			}
		}
	}

	return "", nil, hides, nil
}

// resolveManifest returns the image manifest to read, following an index.
func resolveManifest(ctx context.Context, repo *remote.Repository, img Image) (*ocispec.Manifest, error) {
	if err := digest.Digest(img.Digest).Validate(); err != nil {
		return nil, fmt.Errorf("image digest %q: %w", img.Digest, err)
	}

	desc, body, err := fetchManifest(ctx, repo, img.Digest)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", img.Repository, img.Digest, err)
	}

	switch desc.MediaType {
	case ocispec.MediaTypeImageManifest, mediaTypeDockerManifest:
		return decodeManifest(body)
	case ocispec.MediaTypeImageIndex, mediaTypeDockerManifestList:
		var idx ocispec.Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, fmt.Errorf("parse index: %w", err)
		}

		child, err := pickPlatform(idx, img.Platform)
		if err != nil {
			return nil, err
		}

		_, cbody, err := fetchManifest(ctx, repo, child.Digest.String())
		if err != nil {
			return nil, fmt.Errorf("%s manifest %s: %w", img.Platform, child.Digest, err)
		}

		return decodeManifest(cbody)
	default:
		return nil, fmt.Errorf("%s@%s is a %s, not an image", img.Repository, img.Digest, desc.MediaType)
	}
}

func decodeManifest(body []byte) (*ocispec.Manifest, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	return &m, nil
}

// pickPlatform chooses the image of an index: the named platform, or the only
// real image when none is named. Attestation entries (unknown/unknown) are not images.
func pickPlatform(idx ocispec.Index, want string) (ocispec.Descriptor, error) {
	var (
		images []ocispec.Descriptor
		names  []string
	)

	for _, m := range idx.Manifests {
		if m.Platform == nil || m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
			continue
		}

		images = append(images, m)

		name := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			name += "/" + m.Platform.Variant
		}

		names = append(names, name)
	}

	sort.Strings(names)

	if want == "" {
		if len(images) == 1 {
			return images[0], nil
		}

		return ocispec.Descriptor{}, fmt.Errorf("the index holds %d images (%s): name one with --platform", len(images), strings.Join(names, ", "))
	}

	for i, m := range images {
		full := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			full += "/" + m.Platform.Variant
		}

		if want == full || (want == m.Platform.OS+"/"+m.Platform.Architecture && images[i].Platform.Variant == "") {
			return m, nil
		}
	}

	return ocispec.Descriptor{}, fmt.Errorf("the index has no %s (has %s)", want, strings.Join(names, ", "))
}

func fetchManifest(ctx context.Context, repo *remote.Repository, dgst string) (ocispec.Descriptor, []byte, error) {
	desc, rc, err := repo.Manifests().FetchReference(ctx, dgst)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return ocispec.Descriptor{}, nil, errors.New("not found in the registry")
		}

		return ocispec.Descriptor{}, nil, fmt.Errorf("fetch: %w", err)
	}

	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("read manifest: %w", err)
	}

	if got := digest.FromBytes(body); desc.Digest != "" && got != desc.Digest {
		return ocispec.Descriptor{}, nil, fmt.Errorf("registry served %s for %s", got, desc.Digest)
	}

	return desc, body, nil
}

func open(repoRef string, opts ocipush.Options) (*remote.Repository, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", repoRef, err)
	}

	repo.PlainHTTP = opts.PlainHTTP

	credential := opts.Credential
	if credential == nil {
		credential = readCredentials(repoRef)
	}

	repo.Client = &auth.Client{Credential: credential}

	return repo, nil
}

// readCredentials is the credential source for reading: GITHUB_TOKEN for
// ghcr.io when set (public images also work anonymously), the Docker credential
// store (for example the ECR credential helper) otherwise.
func readCredentials(repoRef string) func(ctx context.Context, hostport string) (auth.Credential, error) {
	if strings.Contains(repoRef, "ghcr.io") {
		return func(context.Context, string) (auth.Credential, error) {
			if token := os.Getenv("GITHUB_TOKEN"); token != "" {
				return auth.Credential{Username: "token", Password: token}, nil
			}

			return auth.EmptyCredential, nil
		}
	}

	return func(ctx context.Context, hostport string) (auth.Credential, error) {
		store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
		if err != nil {
			return auth.EmptyCredential, nil //nolint:nilerr // no credential store: read anonymously
		}

		return credentials.Credential(store)(ctx, hostport)
	}
}
