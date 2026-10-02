package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dist = "../../pkg/goreleaserdist/testdata/dist"

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	old := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w
	runErr := newApp().Run(context.Background(), append([]string{"smctl"}, args...))
	os.Stdout = old

	_ = w.Close()
	out, _ := io.ReadAll(r)

	return strings.TrimSpace(string(out)), runErr
}

func maps(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.js.map"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestPushDryRunDerivesRepositoryAndVersion(t *testing.T) {
	out, err := run(t, "push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps(t), "--dry-run")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(out, "registry.example.com/url-shortener/sourcemaps/web:1.6.1@sha256:") {
		t.Fatalf("got %q", out)
	}

	again, _ := run(t, "push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps(t), "--dry-run")
	if again != out {
		t.Fatalf("dry run is not deterministic: %q vs %q", out, again)
	}

	out, err = run(t, "push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps(t), "--dry-run",
		"--repository-template", "{registry}/maps/{app}", "--app", "frontend")
	if err != nil || !strings.HasPrefix(out, "registry.example.com/maps/frontend:1.6.1@") {
		t.Fatalf("template flag: %q %v", out, err)
	}
}

func TestPushExplicitTarget(t *testing.T) {
	out, err := run(t, "push", "--repository", "ghcr.io/example-org/sourcemaps/web", "--version", "2.0.0+7", "--maps", maps(t), "--dry-run")
	if err != nil || !strings.HasPrefix(out, "ghcr.io/example-org/sourcemaps/web:2.0.0_7@sha256:") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestPushRefusals(t *testing.T) {
	m := maps(t)
	cases := map[string][]string{
		"no target":          {"push", "--maps", m, "--version", "1.0.0", "--dry-run"},
		"both targets":       {"push", "--maps", m, "--goreleaser-dist", dist, "--image", "web", "--repository", "x.example/y", "--dry-run"},
		"image without dist": {"push", "--maps", m, "--image", "web", "--version", "1", "--dry-run"},
		"no version":         {"push", "--maps", m, "--repository", "x.example/y", "--dry-run"},
		"bad version":        {"push", "--maps", m, "--repository", "x.example/y", "--version", "a/b", "--dry-run"},
		"unknown image":      {"push", "--maps", m, "--goreleaser-dist", dist, "--image", "nope", "--dry-run"},
		"empty maps":         {"push", "--maps", t.TempDir(), "--repository", "x.example/y", "--version", "1", "--dry-run"},
	}

	for name, args := range cases {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
