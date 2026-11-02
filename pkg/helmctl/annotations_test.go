package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/truvity/ocictl/pkg/ocipush"
)

func TestChartArtifactAnnotations(t *testing.T) {
	cfg := PushConfig{
		TgzPath: writeTestChart(t),
		Meta:    ChartMeta{Name: "demo", Version: "1.0.0", Description: "A demo", APIVersion: "v2", Type: "application"},
	}

	plain, err := ChartArtifact(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(plain.Annotations) != 3 {
		t.Fatalf("with no flag there is no extra annotation: %v", plain.Annotations)
	}

	cfg.Annotations = map[string]string{
		"org.opencontainers.image.source":   "https://github.com/example-org/example",
		"org.opencontainers.image.revision": "0123456789abcdef0123456789abcdef01234567",
		"org.opencontainers.image.title":    "renamed",
	}

	annotated, err := ChartArtifact(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The manifest carries them (the digest follows from the manifest bytes).
	data, err := ocipush.BuildManifest(annotated)
	if err != nil {
		t.Fatal(err)
	}

	var m struct{ Annotations map[string]string }
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}

	for k, v := range cfg.Annotations {
		if m.Annotations[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, m.Annotations[k], v)
		}
	}

	if m.Annotations["org.opencontainers.image.version"] != "1.0.0" || m.Annotations["org.opencontainers.image.description"] != "A demo" {
		t.Errorf("the chart's own annotations are gone: %v", m.Annotations)
	}

	plainData, err := ocipush.BuildManifest(plain)
	if err != nil {
		t.Fatal(err)
	}

	if string(plainData) == string(data) {
		t.Error("annotations are not part of the manifest digest")
	}
}

// writeTestChart writes a minimal chart archive; Push only repacks it.
func writeTestChart(t *testing.T) string {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "apiVersion: v2\nname: demo\nversion: 1.0.0\n"

	if err := tw.WriteHeader(&tar.Header{Name: "demo/Chart.yaml", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
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
