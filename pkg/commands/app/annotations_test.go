package app_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	keySource   = "org.opencontainers.image.source"
	exampleRepo = "https://github.com/example-org/example"
)

func manifestOf(t *testing.T, out string) string {
	t.Helper()

	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "manifest ") {
			return strings.TrimPrefix(l, "manifest ")
		}
	}

	t.Fatalf("no manifest line in %q", out)

	return ""
}

func digestOf(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := run(t, append([]string{"ocictl", "digest", dir}, args...)...)
	if err != nil {
		t.Fatalf("digest %v: %v", args, err)
	}

	return manifestOf(t, out)
}

func TestAnnotationFlags(t *testing.T) {
	dir := tree(t)

	plain := digestOf(t, dir)
	viaNamed := digestOf(t, dir, "--source", exampleRepo)
	viaKey := digestOf(t, dir, "--annotation", keySource+"="+exampleRepo)

	if viaNamed == plain {
		t.Error("--source changed nothing: annotations are not part of the digest")
	}

	if viaNamed != viaKey {
		t.Error("--source and --annotation of the same key differ")
	}

	// --annotation wins over the named flag.
	if got := digestOf(t, dir, "--source", "https://example.com/a", "--annotation", keySource+"="+exampleRepo); got != viaKey {
		t.Error("--annotation did not override --source")
	}

	// Every named flag lands in the manifest.
	all := digestOf(t, dir, "--source", exampleRepo, "--revision", "abc123", "--licenses", "MIT",
		"--url", "https://example.com/", "--documentation", "https://example.com/docs", "--vendor", "Example")
	if all == viaNamed {
		t.Error("the other named flags changed nothing")
	}
}

func TestAnnotationRefusesCredentials(t *testing.T) {
	dir := tree(t)

	for _, args := range [][]string{
		{"--source", "https://user:hunter2@github.com/example-org/example"},
		{"--annotation", "example.com/link=https://user:hunter2@host.example/x"},
		{"--url", "https://hunter2@host.example/"},
	} {
		_, err := run(t, append([]string{"ocictl", "digest", dir}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Errorf("%v accepted: %v", args, err)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error repeats the credential: %v", err)
		}
	}

	if _, err := run(t, "ocictl", "digest", dir, "--annotation", "novalue"); err == nil {
		t.Error("malformed annotation accepted")
	}
}

func TestAnnotationsFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := tree(t)
	repo := t.TempDir()

	git := func(args ...string) string {
		t.Helper()

		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}

		return strings.TrimSpace(string(out))
	}

	git("init", "-q")
	git("remote", "add", "origin", "https://user:hunter2@github.com/example-org/example.git")
	git("commit", "-q", "--allow-empty", "-m", "init")

	head := git("rev-parse", "HEAD")

	t.Chdir(repo)

	fromGit := digestOf(t, dir, "--annotations-from-git")
	explicit := digestOf(t, dir, "--source", exampleRepo, "--revision", head)

	if fromGit != explicit {
		t.Error("--annotations-from-git is not source=<origin, credentials stripped> and revision=HEAD")
	}

	// The named flags win over what git says.
	if got := digestOf(t, dir, "--annotations-from-git", "--revision", "abc"); got != digestOf(t, dir, "--source", exampleRepo, "--revision", "abc") {
		t.Error("--revision did not override the git revision")
	}

	t.Chdir(t.TempDir())

	if _, err := run(t, "ocictl", "digest", dir, "--annotations-from-git"); err == nil || !strings.Contains(err.Error(), "--annotations-from-git") {
		t.Errorf("outside a repository: %v", err)
	}
}

func TestSourcemapsPushTakesAnnotations(t *testing.T) {
	maps := t.TempDir()
	if err := os.WriteFile(maps+"/a.js.map", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{"ocictl", "sourcemaps", "push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps, "--dry-run"}

	plain, err := run(t, args...)
	if err != nil {
		t.Fatal(err)
	}

	annotated, err := run(t, append(args, "--source", exampleRepo)...)
	if err != nil || annotated == plain {
		t.Fatalf("--source changed nothing: %q vs %q (%v)", annotated, plain, err)
	}
}
