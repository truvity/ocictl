package ocipush_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func anonymous(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }

func artifact(tag, content string) ocipush.Artifact {
	return ocipush.Artifact{
		Layer:           []byte(content),
		LayerMediaType:  "application/vnd.example.layer.v1",
		Config:          []byte("{}"),
		ConfigMediaType: ocispec.MediaTypeEmptyJSON,
		Tag:             tag,
	}
}

func TestPushIdempotent(t *testing.T) {
	reg := fakeregistry.New("", "")
	defer reg.Close()

	repo := reg.Host() + "/org/thing"
	opts := ocipush.Options{PlainHTTP: true, Credential: anonymous}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	first := artifact("1.0.0", "one")
	want, err := ocipush.ManifestDigest(first)
	if err != nil {
		t.Fatal(err)
	}

	// Absent: written.
	out, dgst, err := ocipush.PushIdempotent(ctx, logger, repo, first, opts)
	if err != nil || out != ocipush.Pushed || dgst != want {
		t.Fatalf("first push: %v %s %v", out, dgst, err)
	}

	// Same content: nothing is written.
	puts := reg.Count("PUT")

	out, dgst, err = ocipush.PushIdempotent(ctx, logger, repo, first, opts)
	if err != nil || out != ocipush.AlreadyPresent || dgst != want {
		t.Fatalf("second push: %v %s %v", out, dgst, err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("an already-present push wrote to the registry")
	}

	// Different content under the same tag: refused, naming both digests.
	other := artifact("1.0.0", "two")
	otherDigest, _ := ocipush.ManifestDigest(other)

	_, _, err = ocipush.PushIdempotent(ctx, logger, repo, other, opts)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), otherDigest) {
		t.Fatalf("different content under an existing tag: %v", err)
	}

	if reg.Count("PUT") != puts {
		t.Fatal("a refused push wrote to the registry")
	}

	// AllowOverwrite restores the old behaviour: the tag moves.
	opts.AllowOverwrite = true

	out, dgst, err = ocipush.PushIdempotent(ctx, logger, repo, other, opts)
	if err != nil || out != ocipush.Pushed || dgst != otherDigest {
		t.Fatalf("overwrite: %v %s %v", out, dgst, err)
	}

	got, found, err := ocipush.Resolve(ctx, repo, "1.0.0", opts)
	if err != nil || !found || got != otherDigest {
		t.Fatalf("tag after overwrite: %s %v %v", got, found, err)
	}
}

func TestPushIdempotentNeedsATag(t *testing.T) {
	_, _, err := ocipush.PushIdempotent(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		"registry.example.com/org/thing", artifact("", "x"), ocipush.Options{})
	if err == nil {
		t.Fatal("push without a tag accepted")
	}
}
