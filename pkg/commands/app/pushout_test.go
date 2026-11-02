package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/pushout"
	"github.com/truvity/ocictl/pkg/commands/staticcmd"
	"github.com/truvity/ocictl/pkg/helmctl"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// The JSON shape of a push result is a contract (AGENTS.md): a golden pins the
// field names, their order and the values for a fixed input.
func TestPushResultJSONGolden(t *testing.T) {
	maps := t.TempDir()
	if err := os.WriteFile(filepath.Join(maps, "a.js.map"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "ocictl", "sourcemaps", "push", "--goreleaser-dist", dist, "--image", "web", "--maps", maps, "--dry-run", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}

	const golden = `{"ref":"registry.example.com/url-shortener/sourcemaps/web:1.6.1@sha256:eee71d42bd53ab6b098b8213b47c38164d37d1b080dec098423e42cd3f09006d",` +
		`"repository":"registry.example.com/url-shortener/sourcemaps/web","tag":"1.6.1",` +
		`"digest":"sha256:eee71d42bd53ab6b098b8213b47c38164d37d1b080dec098423e42cd3f09006d","status":"dry-run",` +
		`"artifactType":"application/vnd.ocictl.sourcemaps.v1","mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	if out != golden {
		t.Fatalf("JSON shape changed:\n got: %s\nwant: %s", out, golden)
	}
}

func TestChartResultJSON(t *testing.T) {
	res := pushout.Chart(&helmctl.PushResult{
		Ref: "registry.example.com/org/charts/demo:1.0.0", Digest: "sha256:aa", Repository: "registry.example.com/org/charts/demo",
		Tag: "1.0.0", ArtifactType: "application/vnd.cncf.helm.config.v1+json",
	}, pushout.StatusAlreadyPresent)

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	const want = `{"ref":"registry.example.com/org/charts/demo:1.0.0@sha256:aa","repository":"registry.example.com/org/charts/demo","tag":"1.0.0",` +
		`"digest":"sha256:aa","status":"already-present","artifactType":"application/vnd.cncf.helm.config.v1+json",` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	if string(data) != want {
		t.Fatalf("got %s", data)
	}
}

func TestStaticPushOutputFlags(t *testing.T) {
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
	work := t.TempDir()
	digestFile := filepath.Join(work, "digest")
	ghOut := filepath.Join(work, "github-output")
	t.Setenv("GITHUB_OUTPUT", ghOut)

	repository := reg.Host() + "/org/site"
	args := []string{"ocictl", "static", "push", "--dir", dir, "--repository", repository, "--tag", "1.0.0"}

	// Without the flags the text is what it always was.
	text, err := run(t, args...)
	if err != nil || !strings.HasPrefix(text, "pushed: "+repository+":1.0.0@sha256:") {
		t.Fatalf("%q %v", text, err)
	}

	digest := strings.TrimPrefix(text, "pushed: "+repository+":1.0.0@")

	// JSON, digest file and GitHub output on a re-run (already present).
	out, err := run(t, append(args, "--output", "json", "--digest-file", digestFile, "--github-output", "chart-")...)
	if err != nil {
		t.Fatal(err)
	}

	var res pushout.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%q: %v", out, err)
	}

	want := pushout.Result{
		Ref: repository + ":1.0.0@" + digest, Repository: repository, Tag: "1.0.0", Digest: digest,
		Status: "already-present", ArtifactType: "application/vnd.ocictl.static.v1", MediaType: "application/vnd.oci.image.manifest.v1+json",
	}
	if res != want {
		t.Fatalf("got %+v\nwant %+v", res, want)
	}

	if got, _ := os.ReadFile(digestFile); string(got) != digest+"\n" { //nolint:gosec // test path
		t.Fatalf("digest file %q", got)
	}

	if got, _ := os.ReadFile(ghOut); string(got) != "chart-digest="+digest+"\nchart-ref="+want.Ref+"\n" { //nolint:gosec // test path
		t.Fatalf("GITHUB_OUTPUT %q", got)
	}

	if _, err := run(t, append(args, "--output", "yaml")...); err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("unknown --output: %v", err)
	}
}

func TestDigestOutputFlags(t *testing.T) {
	dir := tree(t)
	digestFile := filepath.Join(t.TempDir(), "digest")

	text, err := run(t, "ocictl", "digest", dir)
	if err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "ocictl", "digest", dir, "--output", "json", "--digest-file", digestFile)
	if err != nil {
		t.Fatal(err)
	}

	var res pushout.DigestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%q: %v", out, err)
	}

	if !strings.Contains(text, "manifest "+res.Digest) || !strings.Contains(text, "layer "+res.LayerDigest) ||
		!strings.Contains(text, "config "+res.ConfigDigest) || res.ArtifactType != "application/vnd.ocictl.static.v1" {
		t.Fatalf("text %q vs json %+v", text, res)
	}

	if got, _ := os.ReadFile(digestFile); string(got) != res.Digest+"\n" { //nolint:gosec // test path
		t.Fatalf("digest file %q", got)
	}
}
