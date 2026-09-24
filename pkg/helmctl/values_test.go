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

// writeStrictChart creates a chart whose values.schema.json forbids any key
// it does not name — the shape most charts that validate their input have,
// and the shape that turns a stray value into an uninstallable artifact.
func writeStrictChart(t *testing.T, values string, properties string) string {
	t.Helper()

	dir := writeChart(t, values)

	schema := `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {` + properties + `}
}`
	if err := os.WriteFile(filepath.Join(dir, "values.schema.json"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

// packageAndRender packages a chart exactly as a release does and then
// renders the PACKAGED artifact with no values of its own.
//
// Rendering the source tree would not catch this class at all: the defect
// is introduced by packaging, so the only artifact worth asserting on is
// the one a consumer receives.
func packageAndRender(t *testing.T, chartDir string, overlay map[string]any) string {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	result, err := Package(context.Background(), logger, PackageConfig{
		ChartDir:      chartDir,
		Version:       "9.9.9",
		ValuesOverlay: overlay,
		OutputDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("package: %v", err)
	}

	out, err := exec.Command("helm", "template", "t", result.TgzPath).CombinedOutput()
	if err != nil {
		t.Fatalf("the PUBLISHED chart does not render, which is what a consumer gets:\n%v\n%s", err, out)
	}

	values, err := exec.Command("helm", "show", "values", result.TgzPath).CombinedOutput()
	if err != nil {
		t.Fatalf("show values: %v\n%s", err, values)
	}

	return string(values)
}

// A chart that declares no images is published WITHOUT an images key.
//
// The release manifest lists everything the build produced, and a
// repository usually publishes more than one chart from one build. The
// chart that runs the workloads declares them; the chart that provisions
// the data resources beside it declares none, and giving that one an
// `images` map it never asked for is not a harmless extra.
//
// It is the difference between a chart that installs and one that cannot.
// A schema with additionalProperties:false is checked BEFORE any template
// runs, so the extra key fails every install, including one that passes no
// values at all. Nothing about the release looks wrong: it packages, it
// pushes, it is green, and the chart is broken for everyone who pulls it.
//
// Verified the other way round: with the narrowing removed, `helm template`
// here fails with `additional properties 'images' not allowed`.
func TestAChartThatDeclaresNoImagesGetsNone(t *testing.T) {
	chartDir := writeStrictChart(t,
		"postgres:\n  instances: 1\n",
		`"postgres": {"type": "object"}`)

	out := packageAndRender(t, chartDir, map[string]any{
		"images": map[string]any{
			"web": map[string]any{"repository": "example/web", "digest": "sha256:abc"},
		},
	})

	if strings.Contains(out, "images") {
		t.Errorf("the packaged chart carries an images key it never declared:\n%s", out)
	}
}

// A chart is given the images it declares, and not the others.
//
// The same manifest reaches every chart in the release. One that declares
// `web` must get `web`'s digest and must not acquire the rest, which
// belong to a different chart in the same repository.
func TestAChartGetsOnlyTheImagesItDeclares(t *testing.T) {
	chartDir := writeChart(t,
		"images:\n  web: {repository: example/web, digest: \"\"}\n")

	overlay := map[string]any{
		"images": map[string]any{
			"web":     map[string]any{"repository": "example/web", "digest": "sha256:web"},
			"counter": map[string]any{"repository": "example/counter", "digest": "sha256:counter"},
		},
	}

	narrowed, err := RestrictImagesToDeclared(chartDir, overlay)
	if err != nil {
		t.Fatal(err)
	}

	images, ok := narrowed["images"].(map[string]any)
	if !ok {
		t.Fatalf("images went missing from a chart that declares one: %#v", narrowed)
	}

	if _, present := images["web"]; !present {
		t.Error("the image the chart declares was dropped")
	}

	if _, present := images["counter"]; present {
		t.Error("an image belonging to another chart was kept")
	}

	// The caller's map is the release's, shared across every chart in the
	// run. Narrowing it in place would empty it for whoever is packaged
	// next.
	if len(overlay["images"].(map[string]any)) != 2 {
		t.Error("the caller's overlay was modified")
	}
}
