package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/indexcmd"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// indexTestImage stores a one-layer image for linux/<arch> and returns its digest.
func indexTestImage(t *testing.T, reg *fakeregistry.Registry, repo, arch string) string {
	t.Helper()

	layer := reg.PutBlob([]byte("layer-" + arch))
	cfgBody, _ := json.Marshal(map[string]any{"os": "linux", "architecture": arch})
	cfg := reg.PutBlob(cfgBody)

	m := ocispec.Manifest{
		Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(cfg), Size: int64(len(cfgBody))},
		Layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.Digest(layer), Size: int64(len("layer-" + arch))}},
	}
	m.SchemaVersion = 2

	body, _ := json.Marshal(m)

	return reg.PutManifest(repo, "build-"+arch, ocispec.MediaTypeImageManifest, body)
}

func TestIndexCreateEndToEnd(t *testing.T) {
	old := indexcmd.RegistryOptions
	indexcmd.RegistryOptions = func(string) ocipush.Options {
		return ocipush.Options{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		}}
	}

	defer func() { indexcmd.RegistryOptions = old }()

	reg := fakeregistry.New("", "")
	defer reg.Close()

	repo := reg.Host() + "/org/app"
	amd, arm := indexTestImage(t, reg, "org/app", "amd64"), indexTestImage(t, reg, "org/app", "arm64")
	args := []string{"ocictl", "index", "create", "--repository", repo, "--tag", "2.0.0",
		"--manifest", repo + "@" + amd, "--manifest", repo + "@" + arm + ":linux/arm64"}

	first, err := run(t, args...)
	if err != nil || !strings.HasPrefix(first, "pushed: "+repo+":2.0.0@sha256:") {
		t.Fatalf("%q %v", first, err)
	}

	again, err := run(t, args...)
	if err != nil || again != "already present: "+strings.TrimPrefix(first, "pushed: ") {
		t.Fatalf("%q %v", again, err)
	}

	js, err := run(t, append(args, "--output", "json")...)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Digest, Status, MediaType, Tag string
		Platforms                      []string
	}

	if err := json.Unmarshal([]byte(js), &doc); err != nil || doc.Status != "already-present" || doc.MediaType != ocispec.MediaTypeImageIndex ||
		doc.Tag != "2.0.0" || !strings.HasSuffix(first, doc.Digest) || strings.Join(doc.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("%q %+v %v", js, doc, err)
	}

	if _, err := run(t, "ocictl", "index", "create", "--repository", repo, "--tag", "x", "--manifest", repo); err == nil {
		t.Error("a manifest without a digest was accepted")
	}
}
