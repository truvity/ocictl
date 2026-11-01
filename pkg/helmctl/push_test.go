package helmctl_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/helmctl"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

// writeChartTgz writes a minimal chart archive with the given values.yaml.
func writeChartTgz(t *testing.T, values string) string {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, body := range map[string]string{
		"demo/Chart.yaml":  "apiVersion: v2\nname: demo\nversion: 1.0.0\n",
		"demo/values.yaml": values,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "demo-1.0.0.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestPushIsIdempotent(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	cfg := helmctl.PushConfig{
		TgzPath:         writeChartTgz(t, "a: 1\n"),
		Registry:        reg.Host(),
		Repository:      "org/charts/demo",
		RegistryOptions: ocipush.Options{PlainHTTP: true, Credential: anonymous},
		Meta:            helmctl.ChartMeta{Name: "demo", Version: "1.0.0", APIVersion: "v2", Type: "application"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	first, err := helmctl.Push(ctx, logger, cfg)
	if err != nil || first.Outcome != ocipush.Pushed {
		t.Fatalf("first push: %+v %v", first, err)
	}

	// The same chart again: exit 0 and nothing written.
	puts := reg.Count("PUT")

	second, err := helmctl.Push(ctx, logger, cfg)
	if err != nil || second.Outcome != ocipush.AlreadyPresent || second.Digest != first.Digest {
		t.Fatalf("second push: %+v %v", second, err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("an already-present push wrote to the registry")
	}

	// Different content under the same tag: refused, naming both digests.
	changed := cfg
	changed.TgzPath = writeChartTgz(t, "a: 2\n")

	_, err = helmctl.Push(ctx, logger, changed)
	if err == nil || !strings.Contains(err.Error(), first.Digest) || strings.Count(err.Error(), "sha256:") < 2 {
		t.Fatalf("different content under an existing tag: %v", err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("a refused push wrote to the registry")
	}

	// --allow-overwrite keeps the old behaviour.
	changed.AllowOverwrite = true

	moved, err := helmctl.Push(ctx, logger, changed)
	if err != nil || moved.Outcome != ocipush.Pushed || moved.Digest == first.Digest {
		t.Fatalf("overwrite: %+v %v", moved, err)
	}
}
