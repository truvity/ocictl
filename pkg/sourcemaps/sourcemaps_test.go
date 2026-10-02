package sourcemaps_test

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

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/goreleaserdist"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/sourcemaps"
)

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

func TestSanitizeTag(t *testing.T) {
	ok := map[string]string{
		"1.2.3":                  "1.2.3",
		"1.2.3+build.5":          "1.2.3_build.5",
		"v0.1.0-rc.1":            "v0.1.0-rc.1",
		"_x":                     "_x",
		strings.Repeat("a", 128): strings.Repeat("a", 128),
	}
	for in, want := range ok {
		got, err := sourcemaps.SanitizeTag(in)
		if err != nil || got != want {
			t.Errorf("SanitizeTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{"", ".hidden", "-x", "a/b", "a b", "a:b", "a@b", strings.Repeat("a", 129), "ä"} {
		if got, err := sourcemaps.SanitizeTag(in); err == nil {
			t.Errorf("SanitizeTag(%q) = %q, want a refusal", in, got)
		}
	}
	// "+" alone sanitises to a valid tag character; it is accepted as "_".
	if got, err := sourcemaps.SanitizeTag("1.2.3+"); err != nil || got != "1.2.3_" {
		t.Errorf("trailing plus: %q, %v", got, err)
	}
}

func TestPackIsDeterministic(t *testing.T) {
	files := map[string]string{
		"assets/index-ab.js.map":  `{"version":3,"file":"a"}`,
		"assets/vendor-cd.js.map": `{"version":3,"file":"b"}`,
		"main.js.map":             `{"version":3}`,
	}

	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, files, time.Unix(1_000_000, 0))
	writeTree(t, b, files, time.Unix(2_000_000_000, 0))

	pa, err := sourcemaps.PackDir(a, nil)
	if err != nil {
		t.Fatal(err)
	}

	pb, err := sourcemaps.PackDir(b, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(pa.Layer, pb.Layer) {
		t.Fatal("same files, different layer bytes")
	}

	want := []string{"assets/index-ab.js.map", "assets/vendor-cd.js.map", "main.js.map"}
	if strings.Join(pa.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", pa.Files, want)
	}

	art, err := sourcemaps.Artifact(pa, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	d1, _ := sourcemaps.ManifestDigest(art)
	art2, _ := sourcemaps.Artifact(pb, "1.0.0")
	d2, _ := sourcemaps.ManifestDigest(art2)

	if d1 != d2 {
		t.Fatalf("manifest digests differ: %s vs %s", d1, d2)
	}

	// The layer carries no ownership, time or mode noise.
	gz, err := gzip.NewReader(bytes.NewReader(pa.Layer))
	if err != nil {
		t.Fatal(err)
	}

	if !gz.ModTime.IsZero() || gz.Name != "" {
		t.Errorf("gzip header not neutral: %v %q", gz.ModTime, gz.Name)
	}

	tr := tar.NewReader(gz)

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.ModTime.Unix() != 0 || h.Mode != 0o644 {
			t.Errorf("%s: header not normalised: %+v", h.Name, h)
		}
	}

	// A change in content changes the digest.
	writeTree(t, b, map[string]string{"main.js.map": `{"version":3,"x":1}`}, time.Unix(5, 0))

	pc, _ := sourcemaps.PackDir(b, nil)
	art3, _ := sourcemaps.Artifact(pc, "1.0.0")

	if d3, _ := sourcemaps.ManifestDigest(art3); d3 == d1 {
		t.Fatal("different content, same digest")
	}
}

func TestPackRefusals(t *testing.T) {
	empty := t.TempDir()
	if _, err := sourcemaps.PackDir(empty, nil); err == nil {
		t.Error("empty dir accepted")
	}

	onlyOther := t.TempDir()
	writeTree(t, onlyOther, map[string]string{"app.js": "x"}, time.Now())

	if _, err := sourcemaps.PackDir(onlyOther, []string{"*.js"}); err == nil {
		t.Error("a dir with no .map accepted")
	}

	mixed := t.TempDir()
	writeTree(t, mixed, map[string]string{"a.js.map": "{}", "a.js": "x", "LICENSE": "l"}, time.Now())

	if _, err := sourcemaps.PackDir(mixed, nil); err == nil || !strings.Contains(err.Error(), "--include") {
		t.Errorf("non-.map file accepted without --include: %v", err)
	}

	p, err := sourcemaps.PackDir(mixed, []string{"*.js", "LICENSE"})
	if err != nil || len(p.Files) != 3 {
		t.Errorf("include patterns not honoured: %v %v", p, err)
	}

	linked := t.TempDir()
	writeTree(t, linked, map[string]string{"a.js.map": "{}"}, time.Now())

	if err := os.Symlink("/etc/passwd", filepath.Join(linked, "b.js.map")); err != nil {
		t.Fatal(err)
	}

	if _, err := sourcemaps.PackDir(linked, nil); err == nil {
		t.Error("symlink accepted")
	}

	if _, err := sourcemaps.PackDir(mixed, []string{"["}); err == nil {
		t.Error("bad pattern accepted")
	}
}

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func TestPushToRegistry(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"assets/a.js.map": `{"version":3}`}, time.Now())

	packed, err := sourcemaps.PackDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	art, err := sourcemaps.Artifact(packed, "1.2.3+build.7")
	if err != nil {
		t.Fatal(err)
	}

	repo := reg.Host() + "/org/sourcemaps/web"
	opts := ocipush.Options{PlainHTTP: true, Credential: anonymous}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	digest, err := sourcemaps.Push(context.Background(), logger, repo, art, opts)
	if err != nil {
		t.Fatal(err)
	}

	want, _ := sourcemaps.ManifestDigest(art)
	if digest != want {
		t.Fatalf("pushed %s, computed %s", digest, want)
	}

	// Pushing again yields the same digest.
	if again, err := sourcemaps.Push(context.Background(), logger, repo, art, opts); err != nil || again != digest {
		t.Fatalf("second push: %s, %v", again, err)
	}

	// Read the manifest back from the registry under the sanitised tag.
	resp, err := reg.Client().Get(reg.URL + "/v2/org/sourcemaps/web/manifests/1.2.3_build.7")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close() //nolint:errcheck

	var m ocispec.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}

	if m.ArtifactType != sourcemaps.ArtifactType || len(m.Layers) != 1 || m.Layers[0].MediaType != sourcemaps.LayerMediaType {
		t.Fatalf("manifest shape: %+v", m)
	}

	if m.Annotations[ocispec.AnnotationVersion] != "1.2.3+build.7" || m.Annotations[ocispec.AnnotationCreated] != "" {
		t.Fatalf("annotations: %v", m.Annotations)
	}

	if m.Layers[0].Digest.String() != fakeregistry.Digest(packed.Layer) {
		t.Fatal("layer digest is not the packed layer")
	}
}

func TestSelectImageAndTemplate(t *testing.T) {
	dist := &goreleaserdist.Dist{Images: []goreleaserdist.Image{
		{Registry: "ghcr.io", Repository: "example-org/web", Digest: "sha256:aa"},
		{Registry: "ghcr.io", Repository: "example-org/api", Digest: "sha256:bb"},
	}}

	img, err := sourcemaps.SelectImage(dist, "web")
	if err != nil || img.Repository != "example-org/web" {
		t.Fatalf("%v %v", img, err)
	}

	if _, err := sourcemaps.SelectImage(dist, "nope"); err == nil {
		t.Error("unknown image accepted")
	}

	got, err := sourcemaps.ExpandRepository(sourcemaps.DefaultRepositoryTemplate, img, "")
	if err != nil || got != "ghcr.io/example-org/sourcemaps/web" {
		t.Errorf("default template: %q %v", got, err)
	}

	got, _ = sourcemaps.ExpandRepository("{registry}/maps/{repository}", img, "")
	if got != "ghcr.io/maps/example-org/web" {
		t.Errorf("custom template: %q", got)
	}

	if _, err := sourcemaps.ExpandRepository("{registry}/{nope}", img, ""); err == nil {
		t.Error("unknown variable accepted")
	}
}
