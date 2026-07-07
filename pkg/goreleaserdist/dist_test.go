package goreleaserdist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadParsesRealWorldDist(t *testing.T) {
	dist, err := Load("testdata/dist")
	if err != nil {
		t.Fatal(err)
	}

	if dist.Version != "1.6.1" || dist.Tag != "url-shortener/v1.6.1" || dist.Commit == "" {
		t.Fatalf("metadata mismatch: %+v", dist)
	}

	if len(dist.Images) != 3 {
		t.Fatalf("want 3 images (web deduped, redirect, migrate), got %d: %+v", len(dist.Images), dist.Images)
	}

	// Sorted by repository: migrate, redirect, web.
	migrate, redirect, web := dist.Images[0], dist.Images[1], dist.Images[2]

	if web.Repository != "url-shortener/web" || web.Tag != "1.6.1" || web.Digest != "sha256:web111" {
		t.Fatalf("dockers_v2 image wrong (latest must lose to version tag): %+v", web)
	}

	if redirect.Repository != "url-shortener/redirect" || redirect.Tag != "1.6.1" || redirect.Digest != "sha256:red222" {
		t.Fatalf("ko image wrong (tag must come from metadata version): %+v", redirect)
	}

	if migrate.Registry != "registry.example.com" {
		t.Fatalf("registry parse wrong: %+v", migrate)
	}
}

func TestLoadRejectsMissingDigest(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, `[
	  {"name": "reg.example.com/p/x:1.0.0", "type": "Docker Image", "extra": {}}
	]`)

	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Fatalf("want no-digest error, got %v", err)
	}
}

func TestLoadRejectsConflictingDigests(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, `[
	  {"name": "reg.example.com/p/x:1.0.0", "type": "Docker Image", "extra": {"Digest": "sha256:aaa"}},
	  {"name": "reg.example.com/p/x:latest", "type": "Docker Image", "extra": {"Digest": "sha256:bbb"}}
	]`)

	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "conflicting digests") {
		t.Fatalf("want conflicting-digests error, got %v", err)
	}
}

func TestManifestAllImages(t *testing.T) {
	dist, err := Load("testdata/dist")
	if err != nil {
		t.Fatal(err)
	}

	m, err := dist.Manifest(ManifestOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if m.Version != "1.6.1" || m.AppVersion != "1.6.1" || m.Commit == "" {
		t.Fatalf("manifest metadata wrong: %+v", m)
	}

	images, ok := m.Values["images"].(map[string]any)
	if !ok || len(images) != 3 {
		t.Fatalf("want 3 image entries, got %v", m.Values)
	}

	web, ok := images["web"].(map[string]any)
	if !ok || web["digest"] != "sha256:web111" || web["repository"] != "url-shortener/web" ||
		web["registry"] != "registry.example.com" || web["tag"] != "1.6.1" {
		t.Fatalf("web entry wrong: %v", web)
	}
}

func TestManifestFilterAndUnknownKey(t *testing.T) {
	dist, err := Load("testdata/dist")
	if err != nil {
		t.Fatal(err)
	}

	m, err := dist.Manifest(ManifestOptions{Images: []string{"web", "redirect"}})
	if err != nil {
		t.Fatal(err)
	}

	images := m.Values["images"].(map[string]any)
	if len(images) != 2 {
		t.Fatalf("filter not applied: %v", images)
	}

	if _, err := dist.Manifest(ManifestOptions{Images: []string{"nope"}}); err == nil ||
		!strings.Contains(err.Error(), "not found in dist") {
		t.Fatalf("want unknown-key error, got %v", err)
	}
}

func TestManifestKeyCollision(t *testing.T) {
	d := &Dist{
		Version: "1.0.0",
		Images: []Image{
			{Registry: "a.example.com", Repository: "p1/api", Digest: "sha256:x", Tag: "1.0.0"},
			{Registry: "b.example.com", Repository: "p2/api", Digest: "sha256:y", Tag: "1.0.0"},
		},
	}

	if _, err := d.Manifest(ManifestOptions{}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want collision error, got %v", err)
	}
}

func writeFixture(t *testing.T, dir, artifacts string) {
	t.Helper()

	meta := `{"version": "1.0.0", "tag": "v1.0.0", "commit": "abc"}`

	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "artifacts.json"), []byte(artifacts), 0o644); err != nil {
		t.Fatal(err)
	}
}
