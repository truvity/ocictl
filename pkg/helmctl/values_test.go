package helmctl

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// writeChart creates a minimal valid chart with the given values.yaml body.
func writeChart(t *testing.T, values string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	chartYAML := "apiVersion: v2\nname: demo\nversion: 0.1.0\nappVersion: 0.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}

	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte(cm), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func readValues(t *testing.T, chartDir string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

func TestInjectValuesDeepMergePreservesUnrelatedKeys(t *testing.T) {
	dir := writeChart(t, `# top comment
replicas: 2
images:
  web:
    registry: ""
    repository: old/web
    tag: ""
    digest: ""
nested:
  keep: yes
`)

	overlay := map[string]any{
		"images": map[string]any{
			"web": map[string]any{
				"registry":   "reg.example.com",
				"repository": "p/web",
				"tag":        "1.2.3",
				"digest":     "sha256:abc",
			},
			"api": map[string]any{"digest": "sha256:def", "repository": "p/api"},
		},
	}

	if err := InjectValues(dir, overlay); err != nil {
		t.Fatal(err)
	}

	values := readValues(t, dir)

	if values["replicas"] != 2 {
		t.Fatalf("unrelated key lost: %v", values)
	}

	images := values["images"].(map[string]any)

	web := images["web"].(map[string]any)
	if web["digest"] != "sha256:abc" || web["repository"] != "p/web" {
		t.Fatalf("web not merged: %v", web)
	}

	if _, ok := images["api"]; !ok {
		t.Fatalf("new key not appended: %v", images)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if !strings.Contains(string(raw), "# top comment") {
		t.Fatalf("comments lost:\n%s", raw)
	}
}

func TestRequireImageDigests(t *testing.T) {
	ok := writeChart(t, "images:\n  web: {repository: p/web, digest: sha256:abc}\n")
	if err := RequireImageDigests(ok); err != nil {
		t.Fatalf("digest present must pass: %v", err)
	}

	bad := writeChart(t, "images:\n  web: {repository: p/web, digest: \"\"}\n")
	if err := RequireImageDigests(bad); err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Fatalf("want digest error, got %v", err)
	}
}

func TestLoadManifestValidatesAndRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	goodBody := "version: 1.2.3\nvalues:\n  images:\n    web: {digest: sha256:abc}\n"
	os.WriteFile(good, []byte(goodBody), 0o644) //nolint:errcheck

	m, err := LoadManifest(good)
	if err != nil {
		t.Fatal(err)
	}

	if m.AppVersion != "1.2.3" {
		t.Fatalf("appVersion default missing: %+v", m)
	}

	typo := filepath.Join(dir, "typo.yaml")
	os.WriteFile(typo, []byte("version: 1.2.3\nvaluez: {}\n"), 0o644) //nolint:errcheck

	if _, err := LoadManifest(typo); err == nil {
		t.Fatal("unknown field must fail")
	}

	missing := filepath.Join(dir, "missing.yaml")
	os.WriteFile(missing, []byte("values: {}\n"), 0o644) //nolint:errcheck

	if _, err := LoadManifest(missing); err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("want version-required error, got %v", err)
	}
}

func TestPackageWithManifestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}

	dir := writeChart(t, "images:\n  web:\n    registry: \"\"\n    repository: \"\"\n    tag: \"\"\n    digest: \"\"\n")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	result, err := Package(context.Background(), logger, PackageConfig{
		ChartDir:   dir,
		Version:    "9.9.9",
		AppVersion: "9.9.9-app",
		ValuesOverlay: map[string]any{
			"images": map[string]any{
				"web": map[string]any{
					"registry":   "reg.example.com",
					"repository": "p/web",
					"tag":        "9.9.9",
					"digest":     "sha256:abc",
				},
			},
		},
		RequireImageDigests: true,
		OutputDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(result.TgzPath, "demo-9.9.9.tgz") {
		t.Fatalf("unexpected tgz: %s", result.TgzPath)
	}

	// Source chart must be untouched.
	src := readValues(t, dir)
	if img := src["images"].(map[string]any)["web"].(map[string]any); img["digest"] != "" {
		t.Fatalf("source chart was modified: %v", img)
	}
}

func TestPackageRequireImageDigestsFails(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}

	dir := writeChart(t, "images:\n  web: {repository: p/web, digest: \"\"}\n")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	_, err := Package(context.Background(), logger, PackageConfig{
		ChartDir:            dir,
		Version:             "1.0.0",
		RequireImageDigests: true,
		OutputDir:           t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Fatalf("want digest gate failure, got %v", err)
	}
}
