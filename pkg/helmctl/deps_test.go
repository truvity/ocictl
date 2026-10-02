package helmctl

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const depParentYAML = `apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: lib
    version: "1.0.0"
    repository: "file://../lib"
`

// depFixture lays down a library chart and a parent that depends on it via
// file://, and returns the parent directory.
func depFixture(t *testing.T, libValues string) string {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not in PATH")
	}

	root := t.TempDir()
	libDir := filepath.Join(root, "lib")
	parentDir := filepath.Join(root, "parent")

	writeFixtureChart(t, libDir, "apiVersion: v2\nname: lib\nversion: 1.0.0\n")
	writeFixtureChart(t, parentDir, depParentYAML)

	if libValues != "" {
		if err := os.WriteFile(filepath.Join(libDir, "values.yaml"), []byte(libValues), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return parentDir
}

func packageWith(t *testing.T, cfg PackageConfig) ([]byte, error) {
	t.Helper()

	cfg.Version = "9.9.9"
	cfg.OutputDir = t.TempDir()

	result, err := Package(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		return nil, err
	}

	data, readErr := os.ReadFile(result.TgzPath)
	if readErr != nil {
		t.Fatal(readErr)
	}

	return data, nil
}

func TestPackageFileDependencyRenders(t *testing.T) {
	parentDir := depFixture(t, "")

	data, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	if !containsEntry(tarEntryNames(t, data), "parent/charts/lib/Chart.yaml") {
		t.Fatalf("library missing from package; entries: %v", tarEntryNames(t, data))
	}

	tgz := filepath.Join(t.TempDir(), "parent-9.9.9.tgz")
	if err := os.WriteFile(tgz, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("helm", "template", "r", tgz).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	// Both the parent's and the library's ConfigMap render.
	if n := strings.Count(string(out), "kind: ConfigMap"); n != 2 {
		t.Fatalf("want 2 rendered ConfigMaps (parent + library), got %d:\n%s", n, out)
	}
}

func TestPackageWithoutDependenciesUnchanged(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not in PATH")
	}

	dir := filepath.Join(t.TempDir(), "plain")
	writeFixtureChart(t, dir, "apiVersion: v2\nname: plain\nversion: 0.1.0\n")

	off, err := packageWith(t, PackageConfig{ChartDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	on, err := packageWith(t, PackageConfig{ChartDir: dir, VendorDependencies: true})
	if err != nil {
		t.Fatal(err)
	}

	nOff, _ := NormalizeTgz(off)
	nOn, _ := NormalizeTgz(on)

	if !bytes.Equal(nOff, nOn) {
		t.Fatal("resolving dependencies changed a chart that declares none")
	}

	if _, err := os.Stat(filepath.Join(dir, "Chart.lock")); err == nil {
		t.Fatal("Chart.lock was created for a chart without dependencies")
	}
}

func TestPackageMissingDependencyFails(t *testing.T) {
	parentDir := depFixture(t, "")

	broken := strings.Replace(depParentYAML, "file://../lib", "file://../does-not-exist", 1)
	if err := os.WriteFile(filepath.Join(parentDir, "Chart.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true})
	if err == nil {
		t.Fatal("want an error for an unresolvable dependency")
	}

	if !strings.Contains(err.Error(), "resolve dependencies of chart") || !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error does not name the failure: %v", err)
	}
}

func TestPackageStaleLockFails(t *testing.T) {
	parentDir := depFixture(t, "")

	// First run writes Chart.lock for lib 1.0.0.
	if _, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true}); err != nil {
		t.Fatal(err)
	}

	// Bump the declared dependency version (and the library) so the lock no
	// longer matches Chart.yaml.
	libChart := filepath.Join(filepath.Dir(parentDir), "lib", "Chart.yaml")
	if err := os.WriteFile(libChart, []byte("apiVersion: v2\nname: lib\nversion: 1.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bumped := strings.Replace(depParentYAML, `"1.0.0"`, `"1.1.0"`, 1)
	if err := os.WriteFile(filepath.Join(parentDir, "Chart.yaml"), []byte(bumped), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true})
	if err == nil {
		t.Fatal("want an error for a stale Chart.lock")
	}

	if !strings.Contains(err.Error(), "Chart.lock is stale") {
		t.Fatalf("error does not explain the stale lock: %v", err)
	}
}

func TestPackageHonoursCommittedLock(t *testing.T) {
	parentDir := depFixture(t, "")

	if _, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true}); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(parentDir, "Chart.lock")

	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true}); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(lockPath)
	if !bytes.Equal(before, after) {
		t.Fatal("an existing, valid Chart.lock was rewritten")
	}
}

func TestPackageDependencyDeterministic(t *testing.T) {
	parentDir := depFixture(t, "")

	first, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true})
	if err != nil {
		t.Fatal(err)
	}

	second, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true})
	if err != nil {
		t.Fatal(err)
	}

	n1, _ := NormalizeTgz(first)
	n2, _ := NormalizeTgz(second)

	if !bytes.Equal(n1, n2) {
		t.Fatal("packaging twice produced different normalized bytes")
	}
}

func TestRequireImageDigestsCoversDependencies(t *testing.T) {
	const libImages = "images:\n  worker:\n    repository: example/worker\n    tag: \"1\"\n"

	t.Run("dependency image without digest is refused", func(t *testing.T) {
		parentDir := depFixture(t, libImages)

		_, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true, RequireImageDigests: true})
		if err == nil || !strings.Contains(err.Error(), "dependency lib: images.worker has no digest") {
			t.Fatalf("want dependency digest refusal, got %v", err)
		}
	})

	t.Run("parent override supplies the digest", func(t *testing.T) {
		parentDir := depFixture(t, libImages)

		values := "lib:\n  images:\n    worker:\n      digest: sha256:abc\n"
		if err := os.WriteFile(filepath.Join(parentDir, "values.yaml"), []byte(values), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := packageWith(t, PackageConfig{ChartDir: parentDir, VendorDependencies: true, RequireImageDigests: true}); err != nil {
			t.Fatalf("want success, got %v", err)
		}
	})
}
