package imagefile_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/imagefile"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

const entryPath = "/usr/libexec/app/entry"

func noAuth(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func options() ocipush.Options { return ocipush.Options{PlainHTTP: true, Credential: noAuth} }

// entry is one tar entry of a test layer.
type entry struct {
	name, body string
	typeflag   byte
	link       string
	mode       int64
}

func file(name, body string) entry {
	return entry{name: name, body: body, typeflag: tar.TypeReg, mode: 0o755}
}

func layerBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Linkname: e.link}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// pushImage stores an image made of the given layers (bottom first) in repo
// and returns its manifest digest.
func pushImage(t *testing.T, reg *fakeregistry.Registry, repo, tag string, layers ...[]byte) string {
	t.Helper()

	m := ocispec.Manifest{}
	m.SchemaVersion = 2

	cfgBody := []byte(`{"os":"linux","architecture":"amd64"}`)
	m.Config = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(reg.PutBlob(cfgBody)), Size: int64(len(cfgBody))}

	for _, l := range layers {
		m.Layers = append(m.Layers, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.Digest(reg.PutBlob(l)), Size: int64(len(l)),
		})
	}

	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return reg.PutManifest(repo, tag, ocispec.MediaTypeImageManifest, body)
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))

	return "sha256:" + hex.EncodeToString(h[:])
}

func read(t *testing.T, reg *fakeregistry.Registry, d, platform, p string) (*imagefile.Result, string, error) {
	t.Helper()

	var out bytes.Buffer

	res, err := imagefile.Open(context.Background(), imagefile.Image{Repository: reg.Host() + "/org/app", Digest: d, Platform: platform}, p, &out, options())

	return res, out.String(), err
}

func TestUpperLayerWins(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	d := pushImage(t, reg, "org/app", "1",
		layerBytes(t, file("usr/libexec/app/entry", "old binary"), file("etc/other", "x")),
		layerBytes(t, file("usr/libexec/app/entry", "new binary")))

	res, content, err := read(t, reg, d, "", entryPath)
	if err != nil || res.Digest != sum("new binary") || content != "new binary" || res.Size != int64(len("new binary")) || res.Mode != 0o755 {
		t.Fatalf("%+v %q %v", res, content, err)
	}

	// A file only the lower layer has is still found.
	if res, _, err := read(t, reg, d, "", "etc/other"); err != nil || res.Digest != sum("x") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestWhiteoutsHideLowerLayers(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	base := layerBytes(t, file("usr/libexec/app/entry", "binary"), file("usr/libexec/app/helper", "h"))

	cases := map[string][]entry{
		"file whiteout":     {{name: "usr/libexec/app/.wh.entry", typeflag: tar.TypeReg}},
		"opaque directory":  {{name: "usr/libexec/app/.wh..wh..opq", typeflag: tar.TypeReg}, file("usr/libexec/app/new", "n")},
		"ancestor whiteout": {{name: "usr/.wh.libexec", typeflag: tar.TypeReg}},
	}

	for name, entries := range cases {
		d := pushImage(t, reg, "org/app", strings.ReplaceAll(name, " ", "-"), base, layerBytes(t, entries...))

		_, _, err := read(t, reg, d, "", entryPath)

		var nf *imagefile.NotFoundError
		if !errors.Is(err, imagefile.ErrNotFound) || !errors.As(err, &nf) || nf.Whiteout == "" || nf.ProcessExitCode() != 2 {
			t.Errorf("%s: want not found by a whiteout (exit 2), got %v", name, err)
		}
	}

	// A file the upper layer re-creates next to the whiteout is there.
	d := pushImage(t, reg, "org/app", "recreated", base,
		layerBytes(t, entry{name: "usr/libexec/app/.wh.entry", typeflag: tar.TypeReg}, file("usr/libexec/app/entry", "again")))

	if res, content, err := read(t, reg, d, "", entryPath); err != nil || content != "again" || res.Digest != sum("again") {
		t.Fatalf("%+v %q %v", res, content, err)
	}

	// A sibling the whiteout does not name stays.
	d = pushImage(t, reg, "org/app", "sibling", base, layerBytes(t, entry{name: "usr/libexec/app/.wh.entry", typeflag: tar.TypeReg}))
	if res, _, err := read(t, reg, d, "", "/usr/libexec/app/helper"); err != nil || res.Digest != sum("h") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNotThere(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	d := pushImage(t, reg, "org/app", "1", layerBytes(t, file("etc/a", "a"),
		entry{name: "usr/", typeflag: tar.TypeDir, mode: 0o755},
		entry{name: "bin/sh", typeflag: tar.TypeSymlink, link: "busybox"},
		entry{name: "bin/busybox", typeflag: tar.TypeReg, body: "bb", mode: 0o755},
		entry{name: "bin/ash", typeflag: tar.TypeLink, link: "bin/busybox"}))

	if _, _, err := read(t, reg, d, "", "/nope"); !errors.Is(err, imagefile.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}

	if _, _, err := read(t, reg, d, "", "/usr"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("a directory: %v", err)
	}

	if _, _, err := read(t, reg, d, "", "/bin/sh"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlink: %v", err)
	}

	if _, _, err := read(t, reg, d, "", "/"); err == nil {
		t.Error("the root as a file")
	}

	// A hard link reads as its target's content.
	if res, content, err := read(t, reg, d, "", "/bin/ash"); err != nil || content != "bb" || res.Digest != sum("bb") {
		t.Errorf("hard link: %+v %q %v", res, content, err)
	}
}

func TestIndexNeedsAPlatform(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd := pushImage(t, reg, "org/app", "amd64", layerBytes(t, file("usr/libexec/app/entry", "amd64 binary")))
	arm := pushImage(t, reg, "org/app", "arm64", layerBytes(t, file("usr/libexec/app/entry", "arm64 binary")))

	idx := ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(amd), Size: 1, Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(arm), Size: 1, Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(amd), Size: 1, Platform: &ocispec.Platform{OS: "unknown", Architecture: "unknown"}},
	}}
	idx.SchemaVersion = 2

	body, _ := json.Marshal(idx)
	d := reg.PutManifest("org/app", "multi", ocispec.MediaTypeImageIndex, body)

	if res, _, err := read(t, reg, d, "linux/arm64", entryPath); err != nil || res.Digest != sum("arm64 binary") {
		t.Fatalf("%+v %v", res, err)
	}

	if res, _, err := read(t, reg, d, "linux/amd64", entryPath); err != nil || res.Digest != sum("amd64 binary") {
		t.Fatalf("%+v %v", res, err)
	}

	if _, _, err := read(t, reg, d, "", entryPath); err == nil || !strings.Contains(err.Error(), "--platform") {
		t.Errorf("an index with two images and no platform: %v", err)
	}

	if _, _, err := read(t, reg, d, "linux/s390x", entryPath); err == nil || !strings.Contains(err.Error(), "no linux/s390x") {
		t.Errorf("a platform the index lacks: %v", err)
	}
}

func TestMissingImage(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	if _, _, err := read(t, reg, "sha256:"+strings.Repeat("d", 64), "", entryPath); err == nil || !strings.Contains(err.Error(), "not found in the registry") {
		t.Errorf("%v", err)
	}

	tag := imagefile.Image{Repository: reg.Host() + "/org/app", Digest: "latest"}
	if _, err := imagefile.Open(context.Background(), tag, entryPath, io.Discard, options()); err == nil {
		t.Error("a tag as the image digest")
	}
}
