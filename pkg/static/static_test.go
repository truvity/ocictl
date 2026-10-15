package static_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/static"
)

// Golden digests of fixedTree. They pin the whole chain: tar layout, gzip
// header, and Go's compress/flate output. If a Go toolchain bump changes
// them, the layer bytes changed, not the content: see docs/static.md before
// updating, and re-publish nothing under an existing tag.
const (
	goldenLayer            = "sha256:eae820d044ac66753d0eda69fdcebd9723b0450e758c5f34018800c8928dbf40"
	goldenArtifactManifest = "sha256:857fa6d31e29cefac37a28fe07710824ec5512dbe33ed577a18175b56891bac1"
	goldenImageConfig      = "sha256:160078bfbf129c581d3704bd4b2d91b53de5a0e6b8dc7ea5d3d015f00935eb2c"
	goldenImageManifest    = "sha256:cac6e72471d9475cf1c115789206a04ef03cabb2b762369c73719b4edffa6d1b"
)

var fixedTree = map[string]string{
	"index.html":         "<!doctype html><title>example</title>\n",
	"assets/app.js":      "console.log('example.com');\n",
	"assets/css/app.css": "body{margin:0}\n",
	"robots.txt":         "User-agent: *\n",
}

func writeTree(t *testing.T, root string, files map[string]string, mtime time.Time) {
	t.Helper()

	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func describe(t *testing.T, dir string, o static.Options) static.Digests {
	t.Helper()

	packed, err := static.Pack(dir)
	if err != nil {
		t.Fatal(err)
	}

	art, err := static.Artifact(packed, o)
	if err != nil {
		t.Fatal(err)
	}

	d, err := static.Describe(art)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestGoldenDigests(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, fixedTree, time.Now())

	a := describe(t, dir, static.Options{Shape: static.ShapeArtifact, Tag: "1.0.0"})
	i := describe(t, dir, static.Options{Shape: static.ShapeImage, Tag: "1.0.0"})

	t.Logf("layer=%s artifactManifest=%s imageConfig=%s imageManifest=%s", a.Layer, a.Manifest, i.Config, i.Manifest)

	if a.Layer != goldenLayer || i.Layer != goldenLayer {
		t.Errorf("layer digest %s / %s, want %s", a.Layer, i.Layer, goldenLayer)
	}

	if a.Manifest != goldenArtifactManifest {
		t.Errorf("artifact manifest %s, want %s", a.Manifest, goldenArtifactManifest)
	}

	if i.Config != goldenImageConfig || i.Manifest != goldenImageManifest {
		t.Errorf("image config/manifest %s / %s, want %s / %s", i.Config, i.Manifest, goldenImageConfig, goldenImageManifest)
	}
}

func TestDeterministicAcrossMtimeAndCreationOrder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, fixedTree, time.Unix(1_000_000, 0))
	writeTree(t, b, fixedTree, time.Unix(2_000_000_000, 0))

	if describe(t, a, static.Options{}) != describe(t, b, static.Options{}) {
		t.Fatal("digests depend on mtime")
	}
}

func TestImageConfigShape(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, fixedTree, time.Now())

	packed, err := static.Pack(dir)
	if err != nil {
		t.Fatal(err)
	}

	art, err := static.Artifact(packed, static.Options{Shape: static.ShapeImage, Architecture: "arm64", OS: "linux"})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + packed.DiffID.String() + `"]}}`
	if string(art.Config) != want {
		t.Fatalf("config %s, want %s", art.Config, want)
	}

	if art.ArtifactType != "" || art.ConfigMediaType != "application/vnd.oci.image.config.v1+json" {
		t.Fatalf("image shape: %q %q", art.ArtifactType, art.ConfigMediaType)
	}

	// diff_id is the digest of the uncompressed tar.
	zr, err := gzip.NewReader(bytes.NewReader(packed.Layer))
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := io.ReadAll(zr)
	if ocipush.DescriptorFromBytes("x", raw).Digest != packed.DiffID {
		t.Fatal("diff_id is not the digest of the uncompressed tar")
	}
}

func TestArtifactShapeAndNoFilter(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, fixedTree, time.Now())

	packed, err := static.Pack(dir)
	if err != nil {
		t.Fatal(err)
	}

	art, _ := static.Artifact(packed, static.Options{Shape: static.ShapeArtifact, Tag: "1.0.0+7"})
	if art.Tag != "1.0.0_7" || art.ArtifactType != "application/vnd.ocictl.static.v1" ||
		art.ConfigMediaType != "application/vnd.oci.empty.v1+json" || string(art.Config) != "{}" {
		t.Fatalf("%+v", art)
	}

	zr, _ := gzip.NewReader(bytes.NewReader(packed.Layer))
	tr := tar.NewReader(zr)

	var names []string

	for {
		h, err := tr.Next()
		if err != nil {
			break
		}

		names = append(names, h.Name)
	}

	if strings.Join(names, ",") != "assets/app.js,assets/css/app.css,index.html,robots.txt" {
		t.Fatalf("entries %v", names)
	}
}

func TestPackRefusals(t *testing.T) {
	empty := t.TempDir()
	if _, err := static.Pack(empty); err == nil {
		t.Error("empty dir accepted")
	}

	dir := t.TempDir()
	writeTree(t, dir, fixedTree, time.Now())

	if err := os.Symlink("index.html", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := static.Pack(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink: %v", err)
	}
}

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func TestPushIsIdempotent(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	dir := t.TempDir()
	writeTree(t, dir, fixedTree, time.Now())

	packed, _ := static.Pack(dir)
	art, _ := static.Artifact(packed, static.Options{Shape: static.ShapeArtifact, Tag: "1.0.0"})
	repo := reg.Host() + "/org/site"
	opts := ocipush.Options{PlainHTTP: true, Credential: anonymous}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	out, dgst, err := static.Push(ctx, logger, repo, art, opts)
	if err != nil || out != static.Pushed {
		t.Fatalf("first push: %v %v", out, err)
	}

	want, _ := static.Describe(art)
	if dgst != want.Manifest {
		t.Fatalf("digest %s want %s", dgst, want.Manifest)
	}

	puts := reg.Count("PUT")

	out, dgst2, err := static.Push(ctx, logger, repo, art, opts)
	if err != nil || out != static.AlreadyPresent || dgst2 != dgst {
		t.Fatalf("second push: %v %v %v", out, dgst2, err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("an already-present push wrote to the registry")
	}

	// Same tag, different content: refused, naming both digests.
	changed := t.TempDir()
	writeTree(t, changed, map[string]string{"index.html": "changed\n"}, time.Now())
	p2, _ := static.Pack(changed)
	art2, _ := static.Artifact(p2, static.Options{Shape: static.ShapeArtifact, Tag: "1.0.0"})
	d2, _ := static.Describe(art2)

	_, _, err = static.Push(ctx, logger, repo, art2, opts)
	if err == nil || !strings.Contains(err.Error(), want.Manifest) || !strings.Contains(err.Error(), d2.Manifest) {
		t.Fatalf("different content under an existing tag: %v", err)
	}

	// The registry holds the original manifest, with the expected shape.
	var m struct {
		ArtifactType string `json:"artifactType"`
		Layers       []struct{ MediaType string }
	}

	resp, err := reg.Client().Get(reg.URL + "/v2/org/site/manifests/1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m.ArtifactType != static.ArtifactType || len(m.Layers) != 1 {
		t.Fatalf("manifest %+v %v", m, err)
	}
}
