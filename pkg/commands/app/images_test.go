package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/chartcmd"
	"github.com/truvity/ocictl/pkg/commands/imagescmd"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func fakeReads(t *testing.T) {
	t.Helper()

	opts := func(string) ocipush.Options {
		return ocipush.Options{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		}}
	}

	oldImages, oldChart := imagescmd.RegistryOptions, chartcmd.RegistryOptions
	imagescmd.RegistryOptions, chartcmd.RegistryOptions = opts, opts

	t.Cleanup(func() { imagescmd.RegistryOptions, chartcmd.RegistryOptions = oldImages, oldChart })
}

// pushImage stores a single-platform manifest and returns its digest.
func pushImage(t *testing.T, reg *fakeregistry.Registry, seed string) string {
	t.Helper()

	cfg := reg.PutBlob([]byte(seed))
	m := ocispec.Manifest{Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(cfg), Size: 1}}
	m.SchemaVersion = 2

	body, _ := json.Marshal(m)

	return reg.PutManifest("org/web", "t-"+seed, ocispec.MediaTypeImageManifest, body)
}

func pushIndex(t *testing.T, reg *fakeregistry.Registry, tag string, children map[string]string) string {
	t.Helper()

	idx := ocispec.Index{}
	idx.SchemaVersion = 2

	for arch, d := range children {
		idx.Manifests = append(idx.Manifests, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(d), Size: 10,
			Platform: &ocispec.Platform{OS: "linux", Architecture: arch},
		})
	}

	body, _ := json.Marshal(idx)

	return reg.PutManifest("org/web", tag, ocispec.MediaTypeImageIndex, body)
}

func releaseManifest(t *testing.T, host, d string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "release.yaml")
	body := "version: 1.0.0\nvalues:\n  images:\n    web:\n      registry: " + host +
		"\n      repository: org/web\n      tag: \"1.0.0\"\n      digest: " + d + "\n"

	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestImagesVerify(t *testing.T) {
	fakeReads(t)

	reg := fakeregistry.New("", "")
	defer reg.Close()

	amd, arm := pushImage(t, reg, "amd"), pushImage(t, reg, "arm")
	both := pushIndex(t, reg, "multi", map[string]string{"amd64": amd, "arm64": arm})

	const platforms = "linux/amd64,linux/arm64"

	out, err := run(t, "ocictl", "images", "verify", "--manifest", releaseManifest(t, reg.Host(), both), "--platforms", platforms)
	if err != nil || out != "verified: images.web "+reg.Host()+"/org/web@"+both+" (index "+platforms+")" {
		t.Fatalf("%q %v", out, err)
	}

	// The same release with a single-arch image fails, naming the key and reference.
	_, err = run(t, "ocictl", "images", "verify", "--manifest", releaseManifest(t, reg.Host(), amd), "--platforms", platforms)
	if err == nil || !strings.Contains(err.Error(), "images.web") || !strings.Contains(err.Error(), reg.Host()+"/org/web@"+amd) {
		t.Fatalf("%v", err)
	}

	// A manifest with no images is not a pass.
	empty := filepath.Join(t.TempDir(), "release.yaml")
	if err := os.WriteFile(empty, []byte("version: 1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, "ocictl", "images", "verify", "--manifest", empty); err == nil || !strings.Contains(err.Error(), "nothing to verify") {
		t.Fatalf("empty manifest: %v", err)
	}

	if _, err := run(t, "ocictl", "images", "verify", "--manifest", empty, "--platforms", "amd64"); err == nil {
		t.Fatal("a malformed platform was accepted")
	}
}

func TestChartPackageVerifyImages(t *testing.T) {
	fakeReads(t)

	reg := fakeregistry.New("", "")
	defer reg.Close()

	good := pushIndex(t, reg, "multi", map[string]string{"amd64": pushImage(t, reg, "amd"), "arm64": pushImage(t, reg, "arm")})
	chart := filepath.Join(t.TempDir(), "demo")

	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: demo\nversion: 0.0.0\n",
		"values.yaml": "images:\n  web:\n    registry: " + reg.Host() + "\n    repository: org/web\n    digest: sha256:" + strings.Repeat("a", 64) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(chart, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	args := []string{"ocictl", "chart", "package", "--chart", chart, "--version", "1.0.0", "--verify-images", "--platforms", "linux/amd64,linux/arm64",
		"--output", t.TempDir()}
	unresolvable := "org/web@sha256:" + strings.Repeat("a", 64)

	if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "images.web") || !strings.Contains(err.Error(), unresolvable) {
		t.Fatalf("an unresolvable digest packaged: %v", err)
	}

	// The release manifest overlays the digest that does resolve; the check passes
	// and packaging goes on to helm (absent here, so it stops there).
	t.Setenv("PATH", t.TempDir())

	overlay := releaseManifest(t, reg.Host(), good)

	_, err := run(t, append(args, "--manifest", overlay)...)
	if err == nil || !strings.Contains(err.Error(), "helm package") {
		t.Fatalf("the verified chart did not reach helm: %v", err)
	}

	if _, err := run(t, "ocictl", "chart", "package", "--chart", chart, "--version", "1.0.0", "--platforms", "linux/amd64"); err == nil {
		t.Fatal("--platforms without --verify-images accepted")
	}
}
