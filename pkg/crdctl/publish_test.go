package crdctl_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/crdctl"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

// stubHelm puts a `helm` on PATH that only knows `package <dir> -d <out>`:
// it archives the chart directory as <out>/<name>-<version>.tgz.
func stubHelm(t *testing.T) {
	t.Helper()

	script := `#!/bin/sh
[ "$1" = package ] || { echo "stub helm: only package" >&2; exit 1; }
name=$(basename "$2")
out="$4/$name-1.0.0.tgz"
tar -czf "$out" -C "$(dirname "$2")" "$name" || exit 1
echo "Successfully packaged chart and saved it to: $out"
`

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// repoWithCRDs lays out charts/demo/{crdctl.yaml,Chart.yaml} and a populated
// CRD cache, so Publish never touches the network. crds is the cached CRD text.
func repoWithCRDs(t *testing.T, crds string) string {
	t.Helper()

	root := t.TempDir()
	chart := filepath.Join(root, "charts", "demo")
	cache := filepath.Join(root, ".cache", "crdctl", "demo", "1.0.0", "raw")

	for p, body := range map[string]string{
		filepath.Join(chart, "crdctl.yaml"): "repo: example-org/upstream\npinned_version: v1.0.0\npaths:\n  - crds\n",
		filepath.Join(chart, "Chart.yaml"):  "apiVersion: v2\nname: demo\nversion: 0.0.0\n",
		filepath.Join(cache, "crds.yaml"):   crds,
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return filepath.Join(chart, "crdctl.yaml")
}

func TestPublishIsIdempotent(t *testing.T) {
	stubHelm(t)

	reg := fakeregistry.New("", "")
	defer reg.Close()

	cfg := crdctl.PublishConfig{
		ConfigPath:      repoWithCRDs(t, "kind: CustomResourceDefinition\n"),
		Registry:        reg.Host(),
		Repository:      "org/charts/demo",
		RegistryOptions: ocipush.Options{PlainHTTP: true, Credential: anonymous},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if _, err := crdctl.Publish(ctx, logger, cfg); err != nil {
		t.Fatal(err)
	}

	// The same CRDs again: exit 0 and nothing written.
	puts := reg.Count("PUT")

	if _, err := crdctl.Publish(ctx, logger, cfg); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("an already-present publish wrote to the registry")
	}

	// Different CRDs under the same version: refused, naming both digests.
	changed := cfg
	changed.ConfigPath = repoWithCRDs(t, "kind: CustomResourceDefinition\nmetadata: {name: changed}\n")

	_, err := crdctl.Publish(ctx, logger, changed)
	if err == nil || strings.Count(err.Error(), "sha256:") < 2 {
		t.Fatalf("different content under an existing tag: %v", err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("a refused publish wrote to the registry")
	}

	// AllowOverwrite keeps the old behaviour.
	changed.AllowOverwrite = true

	if _, err := crdctl.Publish(ctx, logger, changed); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
}
