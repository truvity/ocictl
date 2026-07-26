package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixtureChart lays down a minimal chart directory.
func writeFixtureChart(t *testing.T, dir string, chartYAML string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"Chart.yaml":               chartYAML,
		"values.yaml":              "replicas: 1\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// packageOnce runs Package with vendoring against the fixture parent.
func packageOnce(t *testing.T, parentDir string) []byte {
	t.Helper()

	outDir := t.TempDir()

	result, err := Package(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), PackageConfig{
		ChartDir:           parentDir,
		Version:            "9.9.9",
		VendorDependencies: true,
		OutputDir:          outDir,
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	data, err := os.ReadFile(result.TgzPath)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// TestPackageVendorDependencies exercises the monorepo file:// dependency
// flow: the child chart is vendored into the parent, and the packaged
// parent is byte-deterministic across runs after normalization — the
// property the OCI digest depends on.
func TestPackageVendorDependencies(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not in PATH")
	}

	root := t.TempDir()
	childDir := filepath.Join(root, "shared", "child")
	parentDir := filepath.Join(root, "project", "parent")

	writeFixtureChart(t, childDir, "apiVersion: v2\nname: child\nversion: 1.2.3\n")
	writeFixtureChart(t, parentDir, `apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: child
    version: "1.2.3"
    repository: "file://../../shared/child"
    alias: sub
`)

	first := packageOnce(t, parentDir)

	// The vendored child must be inside the packaged parent — helm
	// re-serializes the dependency as an expanded charts/<name>/ tree.
	names := tarEntryNames(t, first)
	if !containsEntry(names, "parent/charts/child/Chart.yaml") {
		t.Fatalf("vendored child missing from package; entries: %v", names)
	}

	if !containsEntry(names, "parent/Chart.lock") {
		t.Fatalf("Chart.lock missing from package; entries: %v", names)
	}

	// Determinism: a second run (later wall clock, fresh helm dep update)
	// must normalize to identical bytes.
	second := packageOnce(t, parentDir)

	n1, err := NormalizeTgz(first)
	if err != nil {
		t.Fatal(err)
	}

	n2, err := NormalizeTgz(second)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(n1, n2) {
		t.Fatal("normalized package bytes differ between runs — vendored artifacts leak wall-clock state")
	}

	// The lock's generated timestamp must be pinned in the package.
	lock := tarEntryContent(t, first, "parent/Chart.lock")
	if !strings.Contains(lock, `generated: "1970-01-01T00:00:00Z"`) {
		t.Fatalf("Chart.lock generated timestamp not pinned:\n%s", lock)
	}

	// Source-tree artifacts exist (documented side effect, gitignorable).
	if _, err := os.Stat(filepath.Join(parentDir, "Chart.lock")); err != nil {
		t.Fatalf("source Chart.lock expected as documented side effect: %v", err)
	}
}

func tarEntryNames(t *testing.T, tgz []byte) []string {
	t.Helper()

	var names []string

	forEachTarEntry(t, tgz, func(hdr *tar.Header, _ []byte) {
		names = append(names, hdr.Name)
	})

	return names
}

func tarEntryContent(t *testing.T, tgz []byte, name string) string {
	t.Helper()

	var content string

	forEachTarEntry(t, tgz, func(hdr *tar.Header, data []byte) {
		if hdr.Name == name {
			content = string(data)
		}
	})

	if content == "" {
		t.Fatalf("entry %s not found", name)
	}

	return content
}

func forEachTarEntry(t *testing.T, tgz []byte, fn func(*tar.Header, []byte)) {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		fn(hdr, data)
	}
}

func containsEntry(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}

	return false
}
