package imagecheck_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/imagecheck"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func digestOf(s string) digest.Digest { return digest.Digest(s) }

func options() ocipush.Options { return ocipush.Options{PlainHTTP: true, Credential: anonymous} }

// put stores a single-platform image manifest (with its config) and returns its digest.
func put(t *testing.T, reg *fakeregistry.Registry, name, seed string) string {
	t.Helper()

	cfg := reg.PutBlob([]byte(`{"seed":"` + seed + `"}`))
	m := ocispec.Manifest{
		Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digestOf(cfg), Size: 1},
	}
	m.SchemaVersion = 2

	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return reg.PutManifest(name, "c-"+seed, ocispec.MediaTypeImageManifest, body)
}

// putIndex stores an image index naming the given platform -> child digest.
func putIndex(t *testing.T, reg *fakeregistry.Registry, name, tag string, children map[string]string) string {
	t.Helper()

	idx := ocispec.Index{}
	idx.SchemaVersion = 2

	for platform, d := range children {
		os, arch, _ := strings.Cut(platform, "/")
		idx.Manifests = append(idx.Manifests, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest, Digest: digestOf(d), Size: 10,
			Platform: &ocispec.Platform{OS: os, Architecture: arch},
		})
	}

	body, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}

	return reg.PutManifest(name, tag, ocispec.MediaTypeImageIndex, body)
}

func TestVerify(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd := put(t, reg, "org/web", "amd64")
	arm := put(t, reg, "org/web", "arm64")
	both := putIndex(t, reg, "org/web", "1.0.0", map[string]string{"linux/amd64": amd, "linux/arm64": arm, "unknown/unknown": amd})
	onlyAmd := putIndex(t, reg, "org/web", "amd-only", map[string]string{"linux/amd64": amd})
	single := put(t, reg, "org/web", "single")
	// An index whose arm64 manifest was never pushed.
	dangling := putIndex(t, reg, "org/web", "dangling", map[string]string{"linux/amd64": amd, "linux/arm64": "sha256:" + strings.Repeat("0", 64)})

	both2 := []imagecheck.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	image := func(key, d string) imagecheck.Image {
		return imagecheck.Image{Key: key, Registry: reg.Host(), Repository: "org/web", Digest: d}
	}

	t.Run("multi-arch index passes", func(t *testing.T) {
		res, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("web", both)}, both2, options())
		if err != nil || len(res) != 1 || !res[0].Index || strings.Join(res[0].Platforms, ",") != "linux/amd64,linux/arm64" {
			t.Fatalf("%+v %v", res, err)
		}

		if line := res[0].String(); !strings.Contains(line, "images.web") || !strings.Contains(line, both) {
			t.Fatalf("report line %q", line)
		}
	})

	t.Run("a digest that does not resolve names the key and the reference", func(t *testing.T) {
		missing := "sha256:" + strings.Repeat("a", 64)
		_, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("ghost", missing)}, nil, options())

		if err == nil || !strings.Contains(err.Error(), "images.ghost") || !strings.Contains(err.Error(), reg.Host()+"/org/web@"+missing) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a single-arch manifest fails when two platforms are required", func(t *testing.T) {
		_, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("single", single)}, both2, options())
		if err == nil || !strings.Contains(err.Error(), "images.single") || !strings.Contains(err.Error(), "not an index") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("an index missing a platform fails and says which", func(t *testing.T) {
		_, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("amd", onlyAmd)}, both2, options())
		if err == nil || !strings.Contains(err.Error(), "lacks linux/arm64") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("an index whose platform manifest is missing fails", func(t *testing.T) {
		_, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("dangling", dangling)}, both2, options())
		if err == nil || !strings.Contains(err.Error(), "linux/arm64 manifest") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("no platforms named: a single manifest only has to resolve", func(t *testing.T) {
		if _, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("single", single)}, nil, options()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an entry without a digest is refused", func(t *testing.T) {
		_, err := imagecheck.Verify(context.Background(), []imagecheck.Image{image("nodigest", "")}, nil, options())
		if err == nil || !strings.Contains(err.Error(), "no digest") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("every failure is reported, the good ones still listed", func(t *testing.T) {
		res, err := imagecheck.Verify(context.Background(),
			[]imagecheck.Image{image("a-good", both), image("b-bad", single), image("c-bad", onlyAmd)}, both2, options())
		if len(res) != 1 || err == nil || !strings.Contains(err.Error(), "images.b-bad") || !strings.Contains(err.Error(), "images.c-bad") {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

func TestParsePlatforms(t *testing.T) {
	got, err := imagecheck.ParsePlatforms([]string{"linux/amd64", " linux/arm64/v8 ", ""})
	if err != nil || len(got) != 2 || got[1].String() != "linux/arm64/v8" {
		t.Fatalf("%+v %v", got, err)
	}

	for _, bad := range []string{"amd64", "linux/", "/arm64", "a/b/c/d"} {
		if _, err := imagecheck.ParsePlatforms([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFromValues(t *testing.T) {
	imgs, err := imagecheck.FromValues(map[string]any{"images": map[string]any{
		"b": map[string]any{"registry": "r.example", "repository": "o/b", "digest": "sha256:bb"},
		"a": map[string]any{"repository": "r.example/o/a", "digest": "sha256:aa"},
	}})
	if err != nil || len(imgs) != 2 || imgs[0].Key != "a" || imgs[0].Ref() != "r.example/o/a@sha256:aa" || imgs[1].Ref() != "r.example/o/b@sha256:bb" {
		t.Fatalf("%+v %v", imgs, err)
	}

	if none, err := imagecheck.FromValues(map[string]any{"replicas": 2}); err != nil || none != nil {
		t.Fatalf("%v %v", none, err)
	}

	if _, err := imagecheck.FromValues(map[string]any{"images": map[string]any{"x": map[string]any{"digest": "sha256:aa"}}}); err == nil {
		t.Error("an entry without a repository accepted")
	}
}

// TestVerifyLiveRegistry runs against a real registry: set OCICTL_TEST_IMAGE
// to a multi-arch registry/repository@digest (and GITHUB_TOKEN when it is
// private). It is skipped without it.
func TestVerifyLiveRegistry(t *testing.T) {
	ref := os.Getenv("OCICTL_TEST_IMAGE")
	if ref == "" {
		t.Skip("OCICTL_TEST_IMAGE not set")
	}

	repo, d, ok := strings.Cut(ref, "@")
	if !ok {
		t.Fatalf("OCICTL_TEST_IMAGE %q: want registry/repository@digest", ref)
	}

	platforms, err := imagecheck.ParsePlatforms(strings.Split(os.Getenv("OCICTL_TEST_PLATFORMS"), ","))
	if err != nil {
		t.Fatal(err)
	}

	images := []imagecheck.Image{{Key: "live", Repository: repo, Digest: d}}

	if _, err := imagecheck.Verify(context.Background(), images, platforms, ocipush.Options{}); err != nil {
		t.Fatal(err)
	}
}
