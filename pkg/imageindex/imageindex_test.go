package imageindex_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/imageindex"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func noAuth(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func options() ocipush.Options { return ocipush.Options{PlainHTTP: true, Credential: noAuth} }

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// pushImage stores a one-layer image for os/arch in repo and returns its manifest digest.
func pushImage(t *testing.T, reg *fakeregistry.Registry, repo, osName, arch string) string {
	t.Helper()

	layer := reg.PutBlob([]byte("layer-" + arch))
	cfgBody, _ := json.Marshal(map[string]any{"os": osName, "architecture": arch, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{layer}}})
	cfg := reg.PutBlob(cfgBody)

	m := ocispec.Manifest{
		Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(cfg), Size: int64(len(cfgBody))},
		Layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.Digest(layer), Size: int64(len("layer-" + arch))}},
	}
	m.SchemaVersion = 2

	body, _ := json.Marshal(m)

	return reg.PutManifest(repo, "build-"+arch, ocispec.MediaTypeImageManifest, body)
}

func TestCreateIndexFromTwoJobs(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd := pushImage(t, reg, "org/app", "linux", "amd64")
	arm := pushImage(t, reg, "org/app", "linux", "arm64")
	repo := reg.Host() + "/org/app"

	// The platform comes from each image's config when not named.
	spec := imageindex.Spec{
		Repository: repo, Tag: "2.0.0",
		Inputs: []imageindex.Input{{Repository: repo, Digest: arm}, {Repository: repo, Digest: amd}},
	}
	ctx := context.Background()

	first, err := imageindex.Create(ctx, logger(), spec, options())
	if err != nil || first.AlreadyPresent || strings.Join(first.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("%+v %v", first, err)
	}

	// The index in the registry lists both, platform-labelled, sorted.
	got, found, err := ocipush.Resolve(ctx, repo, "2.0.0", options())
	if err != nil || !found || got != first.Digest {
		t.Fatalf("%s %v %v", got, found, err)
	}

	var idx ocispec.Index

	resp, err := reg.Client().Get(reg.URL + "/v2/org/app/manifests/2.0.0")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil || len(idx.Manifests) != 2 || idx.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("%+v %v", idx, err)
	}

	if idx.Manifests[0].Platform.Architecture != "amd64" || idx.Manifests[0].Digest.String() != amd || idx.Manifests[1].Digest.String() != arm {
		t.Fatalf("%+v", idx.Manifests)
	}

	// Re-running with the same inputs (in any order) says so, with the same digest, and writes nothing.
	puts := reg.Count("PUT")
	spec.Inputs = []imageindex.Input{{Repository: repo, Digest: amd}, {Repository: repo, Digest: arm}}

	again, err := imageindex.Create(ctx, logger(), spec, options())
	if err != nil || !again.AlreadyPresent || again.Digest != first.Digest {
		t.Fatalf("%+v %v", again, err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("an already-present index wrote to the registry")
	}

	// A different set under the same tag is refused, naming both digests.
	spec.Inputs = []imageindex.Input{{Repository: repo, Digest: amd}}

	if _, err := imageindex.Create(ctx, logger(), spec, options()); err == nil || strings.Count(err.Error(), "sha256:") < 2 {
		t.Fatalf("a changed index under an existing tag: %v", err)
	}
}

func TestCreateIndexRefusals(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd := pushImage(t, reg, "org/app", "linux", "amd64")
	other := pushImage(t, reg, "org/app", "linux", "arm64")
	repo := reg.Host() + "/org/app"
	ctx := context.Background()
	linuxAMD := &ocispec.Platform{OS: "linux", Architecture: "amd64"}

	create := func(inputs ...imageindex.Input) error {
		_, err := imageindex.Create(ctx, logger(), imageindex.Spec{Repository: repo, Tag: "t", Inputs: inputs}, options())

		return err
	}

	if err := create(); err == nil {
		t.Error("an index of nothing was created")
	}

	twice := imageindex.Input{Repository: repo, Digest: amd}
	if err := create(twice, twice); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("the same digest twice: %v", err)
	}

	// Two different images claiming one platform.
	first, second := imageindex.Input{Repository: repo, Digest: amd, Platform: linuxAMD}, imageindex.Input{Repository: repo, Digest: other, Platform: linuxAMD}
	if err := create(first, second); err == nil || !strings.Contains(err.Error(), "linux/amd64 appears twice") {
		t.Errorf("a duplicate platform: %v", err)
	}

	missing := "sha256:" + strings.Repeat("c", 64)
	if err := create(imageindex.Input{Repository: repo, Digest: missing, Platform: linuxAMD}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a missing image: %v", err)
	}

	// An index is not a single-platform image.
	idx := reg.PutManifest("org/app", "multi", ocispec.MediaTypeImageIndex, []byte(`{"schemaVersion":2,"manifests":[]}`))
	if err := create(imageindex.Input{Repository: repo, Digest: idx, Platform: linuxAMD}); err == nil || !strings.Contains(err.Error(), "single-platform") {
		t.Errorf("an index as an input: %v", err)
	}
}

func TestCreateIndexCopiesFromAnotherRepository(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd := pushImage(t, reg, "org/build-amd64", "linux", "amd64")
	arm := pushImage(t, reg, "org/build-arm64", "linux", "arm64")
	target := reg.Host() + "/org/app"

	res, err := imageindex.Create(context.Background(), logger(), imageindex.Spec{
		Repository: target, Tag: "1.0.0",
		Inputs: []imageindex.Input{{Repository: reg.Host() + "/org/build-amd64", Digest: amd}, {Repository: reg.Host() + "/org/build-arm64", Digest: arm}},
	}, options())
	if err != nil || len(res.Platforms) != 2 {
		t.Fatalf("%+v %v", res, err)
	}

	// Both images are now readable in the index's own repository.
	for _, d := range []string{amd, arm} {
		resp, err := reg.Client().Get(reg.URL + "/v2/org/app/manifests/" + d)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("%s was not copied into the target repository: %s", d, resp.Status)
		}
	}
}

func TestParseInput(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)

	in, err := imageindex.ParseInput("registry.example.com:5000/org/app@" + d + ":linux/arm64/v8")
	if err != nil || in.Repository != "registry.example.com:5000/org/app" || in.Digest != d {
		t.Fatalf("%+v %v", in, err)
	}

	if p := in.Platform; p.OS != "linux" || p.Architecture != "arm64" || p.Variant != "v8" {
		t.Fatalf("%+v %v", in, err)
	}

	if in, err = imageindex.ParseInput("registry.example.com/org/app@" + d); err != nil || in.Platform != nil {
		t.Fatalf("%+v %v", in, err)
	}

	if in, err = imageindex.ParseInput("registry.example.com/org/app:1.0@" + d); err != nil || in.Repository != "registry.example.com/org/app" {
		t.Fatalf("a tag before the @ is dropped: %+v %v", in, err)
	}

	for _, bad := range []string{"registry.example.com/org/app", "@" + d,
		"r/o@sha256:short", "r/o@" + d + ":linux", "r/o@" + d + ":a/b/c/d"} {
		if _, err := imageindex.ParseInput(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
