package ocipush_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

const (
	subjectName = "org/app"
	emptyDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)

func noAuth(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func subjectManifest(t *testing.T, reg *fakeregistry.Registry) string {
	t.Helper()

	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + emptyDigest + `","size":2},"layers":[]}`

	return reg.PutManifest(subjectName, "1.0.0", ocispec.MediaTypeImageManifest, []byte(body))
}

func sbom(content string) ocipush.Artifact {
	return ocipush.Artifact{
		ArtifactType: "application/spdx+json",
		Blobs: []ocipush.Blob{{
			MediaType: "application/spdx+json", Data: []byte(content),
			Annotations: map[string]string{ocispec.AnnotationTitle: "sbom.spdx.json"},
		}},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fastSettle(t *testing.T) {
	t.Helper()

	old := ocipush.ReferrersSettle
	ocipush.ReferrersSettle = 40 * time.Millisecond

	t.Cleanup(func() { ocipush.ReferrersSettle = old })
}

// listWithORAS reads the referrers the way `oras discover` does: with oras-go's
// own client, which uses the API and falls back to the tag schema.
func listWithORAS(t *testing.T, reg *fakeregistry.Registry, subject string) []ocispec.Descriptor {
	t.Helper()

	repo, err := remote.NewRepository(reg.Host() + "/" + subjectName)
	if err != nil {
		t.Fatal(err)
	}

	repo.PlainHTTP = true

	var out []ocispec.Descriptor

	err = repo.Referrers(context.Background(), ocispec.Descriptor{Digest: digest.Digest(subject)}, "", func(refs []ocispec.Descriptor) error {
		out = append(out, refs...)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestAttachWithReferrersAPI(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()
	reg.SetReferrersAPI(true)

	subject := subjectManifest(t, reg)
	repo := reg.Host() + "/" + subjectName
	opts := ocipush.Options{PlainHTTP: true, Credential: noAuth}

	res, err := ocipush.Attach(context.Background(), quietLogger(), repo, subject, sbom("{}"), opts)
	if err != nil || res.Mode != ocipush.ModeAPI || res.Subject.Digest.String() != subject {
		t.Fatalf("%+v %v", res, err)
	}

	listed, mode, err := ocipush.Referrers(context.Background(), repo, subject, "", opts)
	if err != nil || mode != ocipush.ModeAPI || len(listed) != 1 || listed[0].Digest.String() != res.Digest || listed[0].ArtifactType != "application/spdx+json" {
		t.Fatalf("%+v %v %v", listed, mode, err)
	}

	if got := listWithORAS(t, reg, subject); len(got) != 1 || got[0].Digest.String() != res.Digest {
		t.Fatalf("oras-go sees %+v", got)
	}

	// The same artifact again is the same manifest, listed once.
	again, err := ocipush.Attach(context.Background(), quietLogger(), repo, subject, sbom("{}"), opts)
	if err != nil || again.Digest != res.Digest {
		t.Fatalf("%+v %v", again, err)
	}

	if listed, _, _ := ocipush.Referrers(context.Background(), repo, subject, "", opts); len(listed) != 1 {
		t.Fatalf("attached twice, listed %d", len(listed))
	}

	// The artifact type filter.
	if none, _, err := ocipush.Referrers(context.Background(), repo, subject, "application/vnd.example", opts); err != nil || len(none) != 0 {
		t.Fatalf("filter: %+v %v", none, err)
	}
}

func TestAttachWithTagSchemaFallback(t *testing.T) {
	fastSettle(t)

	reg := fakeregistry.New("", "") // no referrers API, like GHCR
	defer reg.Close()

	subject := subjectManifest(t, reg)
	repo := reg.Host() + "/" + subjectName
	opts := ocipush.Options{PlainHTTP: true, Credential: noAuth}
	ctx := context.Background()

	first, err := ocipush.Attach(ctx, quietLogger(), repo, subject, sbom("{}"), opts)
	if err != nil || first.Mode != ocipush.ModeTagSchema {
		t.Fatalf("%+v %v", first, err)
	}

	sig := ocipush.Artifact{ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		Blobs: []ocipush.Blob{{MediaType: "application/octet-stream", Data: []byte("sig")}}}

	second, err := ocipush.Attach(ctx, quietLogger(), repo, subject, sig, opts)
	if err != nil {
		t.Fatal(err)
	}

	// The index lives under the sha256-<hex> tag and lists both, with types.
	tag := ocipush.ReferrersTag(digest.Digest(subject))
	if !strings.HasPrefix(tag, "sha256-") || strings.Contains(tag, ":") {
		t.Fatalf("tag %q", tag)
	}

	for name, got := range map[string][]ocispec.Descriptor{
		"ocictl":  mustList(t, repo, subject, "", opts),
		"oras-go": listWithORAS(t, reg, subject),
	} {
		if len(got) != 2 {
			t.Fatalf("%s lists %d referrers: %+v", name, len(got), got)
		}

		types := map[string]string{}
		for _, d := range got {
			types[d.Digest.String()] = d.ArtifactType
		}

		if types[first.Digest] != "application/spdx+json" || types[second.Digest] != "application/vnd.dev.sigstore.bundle.v0.3+json" {
			t.Fatalf("%s: %v", name, types)
		}
	}

	if only := mustList(t, repo, subject, "application/spdx+json", opts); len(only) != 1 || only[0].Digest.String() != first.Digest {
		t.Fatalf("filter: %+v", only)
	}

	// Attaching the first again leaves two entries.
	if _, err := ocipush.Attach(ctx, quietLogger(), repo, subject, sbom("{}"), opts); err != nil {
		t.Fatal(err)
	}

	if got := mustList(t, repo, subject, "", opts); len(got) != 2 {
		t.Fatalf("re-attach changed the list: %+v", got)
	}
}

func mustList(t *testing.T, repo, subject, artifactType string, opts ocipush.Options) []ocispec.Descriptor {
	t.Helper()

	got, _, err := ocipush.Referrers(context.Background(), repo, subject, artifactType, opts)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// Two writers that read the index before either writes would, without the
// read-back, leave only the last one: both must survive.
func TestConcurrentAttachesBothSurvive(t *testing.T) {
	fastSettle(t)

	reg := fakeregistry.New("", "")
	defer reg.Close()
	reg.SetManifestLatency(30 * time.Millisecond) // widens the read-to-write window

	subject := subjectManifest(t, reg)
	repo := reg.Host() + "/" + subjectName
	opts := ocipush.Options{PlainHTTP: true, Credential: noAuth}

	const writers = 6

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		digests = map[string]bool{}
		errs    []error
	)

	for i := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			res, err := ocipush.Attach(context.Background(), quietLogger(), repo, subject, sbom(fmt.Sprintf(`{"n":%d}`, i)), opts)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)

				return
			}

			digests[res.Digest] = true
		}()
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("attach failed: %v", errs)
	}

	listed := mustList(t, repo, subject, "", opts)
	have := map[string]bool{}

	for _, d := range listed {
		have[d.Digest.String()] = true
	}

	if len(digests) != writers {
		t.Fatalf("expected %d distinct referrers, got %d", writers, len(digests))
	}

	for d := range digests {
		if !have[d] {
			t.Errorf("referrer %s was lost from the index", d)
		}
	}
}

func TestAttachRefusals(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	repo := reg.Host() + "/" + subjectName
	opts := ocipush.Options{PlainHTTP: true, Credential: noAuth}
	ctx := context.Background()

	missing := "sha256:" + strings.Repeat("b", 64)
	if _, err := ocipush.Attach(ctx, quietLogger(), repo, missing, sbom("{}"), opts); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a subject that does not exist: %v", err)
	}

	if _, err := ocipush.Attach(ctx, quietLogger(), repo, "1.0.0", sbom("{}"), opts); err == nil {
		t.Error("a tag as the subject was accepted")
	}

	noType := sbom("{}")
	noType.ArtifactType = ""

	if _, err := ocipush.Attach(ctx, quietLogger(), repo, subjectManifest(t, reg), noType, opts); err == nil {
		t.Error("an artifact without an artifact type was accepted")
	}
}

// TestAttachLiveRegistry attaches a small artifact to a manifest in a real
// registry (GHCR has no referrers API, so this exercises the tag schema) and
// lists it back. It WRITES to that registry, so it runs only when
// OCICTL_TEST_SUBJECT (a <host/path>@<digest> you may push to) and, for ghcr.io,
// GITHUB_TOKEN are set.
func TestAttachLiveRegistry(t *testing.T) {
	ref := os.Getenv("OCICTL_TEST_SUBJECT")
	if ref == "" {
		t.Skip("OCICTL_TEST_SUBJECT not set")
	}

	repo, subject, ok := strings.Cut(ref, "@")
	if !ok {
		t.Fatalf("OCICTL_TEST_SUBJECT %q: want <host/path>@<digest>", ref)
	}

	art := ocipush.Artifact{
		ArtifactType: "application/vnd.ocictl.test.v1",
		Blobs:        []ocipush.Blob{{MediaType: "text/plain", Data: []byte("ocictl live referrers test\n")}},
	}

	res, err := ocipush.Attach(context.Background(), quietLogger(), repo, subject, art, ocipush.Options{})
	if err != nil {
		t.Fatal(err)
	}

	listed, _, err := ocipush.Referrers(context.Background(), repo, subject, "application/vnd.ocictl.test.v1", ocipush.Options{})
	if err != nil || !slices.ContainsFunc(listed, func(d ocispec.Descriptor) bool { return d.Digest.String() == res.Digest }) {
		t.Fatalf("attached %s, listed %+v (%v)", res.Digest, listed, err)
	}
}
