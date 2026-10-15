package app_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/app"
	"github.com/truvity/ocictl/pkg/commands/staticcmd"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

const dist = "../../goreleaserdist/testdata/dist"

// run executes args[0] args[1:] through the same dispatch the binaries use
// and returns trimmed stdout and the error.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	old := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w

	root := app.Legacy(app.Name(args[0]), "test")
	if root == nil {
		root = app.New("test")
	}

	runErr := root.Run(context.Background(), args)
	os.Stdout = old

	_ = w.Close()
	out, _ := io.ReadAll(r)

	return strings.TrimSpace(string(out)), runErr
}

func same(t *testing.T, oldArgs, newArgs []string) string {
	t.Helper()

	oldOut, oldErr := run(t, oldArgs...)
	newOut, newErr := run(t, newArgs...)

	if (oldErr == nil) != (newErr == nil) {
		t.Fatalf("error differs: %v vs %v", oldErr, newErr)
	}

	if oldErr != nil && oldErr.Error() != newErr.Error() {
		t.Fatalf("error text differs: %v vs %v", oldErr, newErr)
	}

	// Help text names the command; spell the new path as the old name.
	for _, g := range [][2]string{{"ocictl chart", "helmctl"}, {"ocictl crd", "crdctl"}, {"ocictl sourcemaps", "smctl"}} {
		newOut = strings.ReplaceAll(newOut, g[0], g[1])
	}

	if oldOut != newOut {
		t.Fatalf("output differs:\nold: %q\nnew: %q", oldOut, newOut)
	}

	return oldOut
}

func TestNameSelectsLegacyByArgv0(t *testing.T) {
	for argv0, want := range map[string]string{
		"helmctl": "helmctl", "/usr/bin/crdctl": "crdctl", "./smctl": "smctl", "smctl.exe": "smctl",
		"ocictl": "ocictl", "something-else": "ocictl",
	} {
		if got := app.Name(argv0); got != want {
			t.Errorf("Name(%q) = %q, want %q", argv0, got, want)
		}
	}
}

func TestChartMatchesHelmctl(t *testing.T) {
	out := same(t,
		[]string{"helmctl", "goreleaser-manifest", "--goreleaser-dist", dist},
		[]string{"ocictl", "chart", "goreleaser-manifest", "--goreleaser-dist", dist})
	if out == "" {
		t.Fatal("no manifest printed")
	}

	// Failures are identical too.
	same(t, []string{"helmctl", "package"}, []string{"ocictl", "chart", "package"})
	empty := t.TempDir()
	same(t, []string{"helmctl", "goreleaser-manifest", "--goreleaser-dist", empty},
		[]string{"ocictl", "chart", "goreleaser-manifest", "--goreleaser-dist", empty})
}

func TestCrdMatchesCrdctl(t *testing.T) {
	same(t, []string{"crdctl", "build"}, []string{"ocictl", "crd", "build"})
	same(t, []string{"crdctl", "build", "--config", "/nonexistent/crdctl.yaml"},
		[]string{"ocictl", "crd", "build", "--config", "/nonexistent/crdctl.yaml"})
}

func TestSourcemapsMatchesSmctl(t *testing.T) {
	maps := t.TempDir()
	if err := os.WriteFile(filepath.Join(maps, "a.js.map"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{"push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps, "--dry-run"}

	out := same(t, append([]string{"smctl"}, args...), append([]string{"ocictl", "sourcemaps"}, args...))
	if !strings.HasPrefix(out, "registry.example.com/url-shortener/sourcemaps/web:1.6.1@sha256:") {
		t.Fatalf("got %q", out)
	}

	same(t, []string{"smctl", "push", "--maps", maps}, []string{"ocictl", "sourcemaps", "push", "--maps", maps})
}

func tree(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "<p>x</p>\n", "a/b.js": "1\n"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestDigestMatchesStaticPushDryRun(t *testing.T) {
	dir := tree(t)

	for _, shape := range []string{"artifact", "image"} {
		d, err := run(t, "ocictl", "digest", dir, "--shape", shape)
		if err != nil {
			t.Fatal(err)
		}

		p, err := run(t, "ocictl", "static", "push", "--dir", dir, "--repository", "registry.example.com/site", "--tag", "1.0.0",
			"--shape", shape, "--dry-run")
		if err != nil {
			t.Fatal(err)
		}

		if d != p || !strings.Contains(d, "layer sha256:") || !strings.Contains(d, "manifest sha256:") {
			t.Fatalf("%s: digest %q vs dry-run %q", shape, d, p)
		}
	}

	if _, err := run(t, "ocictl", "digest"); err == nil {
		t.Error("digest without a dir accepted")
	}

	if _, err := run(t, "ocictl", "digest", dir, "--shape", "tarball"); err == nil {
		t.Error("unknown shape accepted")
	}

	bad := []string{"ocictl", "static", "push", "--dir", dir, "--repository", "x.example/y", "--tag", "1", "--annotation", "novalue", "--dry-run"}
	if _, err := run(t, bad...); err == nil {
		t.Error("malformed annotation accepted")
	}
}

func TestStaticPushEndToEnd(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	old := staticcmd.RegistryOptions
	staticcmd.RegistryOptions = func(string) ocipush.Options {
		return ocipush.Options{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		}}
	}

	defer func() { staticcmd.RegistryOptions = old }()

	dir := tree(t)
	args := []string{"ocictl", "static", "push", "--dir", dir, "--repository", reg.Host() + "/org/site", "--tag", "1.0.0",
		"--shape", "image", "--annotation", "org.opencontainers.image.source=https://example.com/repo"}

	first, err := run(t, args...)
	if err != nil || !strings.HasPrefix(first, "pushed: ") {
		t.Fatalf("%q %v", first, err)
	}

	second, err := run(t, args...)
	if err != nil || !strings.HasPrefix(second, "already present: ") || strings.TrimPrefix(second, "already present: ") != strings.TrimPrefix(first, "pushed: ") {
		t.Fatalf("%q %v", second, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, args...); err == nil || strings.Count(err.Error(), "sha256:") < 2 {
		t.Fatalf("changed content under an existing tag: %v", err)
	}
}

func TestCreatedFromSourceDate(t *testing.T) {
	dir := tree(t)
	undated, err := run(t, "ocictl", "digest", dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")

	dated, err := run(t, "ocictl", "digest", dir)
	if err != nil || dated == undated {
		t.Fatalf("SOURCE_DATE_EPOCH changed nothing: %q %v", dated, err)
	}

	again, err := run(t, "ocictl", "digest", dir)
	if err != nil || again != dated {
		t.Fatalf("not reproducible: %q vs %q (%v)", again, dated, err)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "")

	viaFlag, err := run(t, "ocictl", "digest", dir, "--created", "2023-11-14T22:13:20Z")
	if err != nil || viaFlag != dated {
		t.Fatalf("--created differs from the same SOURCE_DATE_EPOCH: %q vs %q (%v)", viaFlag, dated, err)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "not-a-number")

	if _, err := run(t, "ocictl", "digest", dir); err == nil || !strings.Contains(err.Error(), "SOURCE_DATE_EPOCH") {
		t.Errorf("invalid SOURCE_DATE_EPOCH: %v", err)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "")

	if _, err := run(t, "ocictl", "digest", dir, "--created", "yesterday"); err == nil || !strings.Contains(err.Error(), "--created") {
		t.Errorf("invalid --created: %v", err)
	}
}
