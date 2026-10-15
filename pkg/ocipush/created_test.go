package ocipush

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestResolveCreated(t *testing.T) {
	const want = "2023-11-14T22:13:20Z"

	cases := []struct {
		name, flag, epoch, want, wantErr string
	}{
		{name: "neither", want: ""},
		{name: "epoch", epoch: "1700000000", want: want},
		{name: "flag", flag: "2023-11-15T00:13:20+02:00", want: want},
		{name: "flag drops fractions", flag: "2023-11-14T22:13:20.987Z", want: want},
		{name: "epoch beats flag", flag: "2001-01-01T00:00:00Z", epoch: "1700000000", want: want},
		{name: "epoch zero is a date", epoch: "0", want: "1970-01-01T00:00:00Z"},
		{name: "bad epoch", epoch: "yesterday", wantErr: "SOURCE_DATE_EPOCH"},
		{name: "negative epoch", epoch: "-5", wantErr: "SOURCE_DATE_EPOCH"},
		{name: "bad flag", flag: "2023-11-14", wantErr: "--created"},
		{name: "bad flag is not hidden by epoch", flag: "nope", epoch: "1700000000", wantErr: "--created"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveCreated(c.flag, c.epoch)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error %v, want one naming %q", err, c.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if c.want == "" {
				if !got.IsZero() {
					t.Fatalf("got %v, want the zero time", got)
				}

				return
			}

			if s := got.Format(time.RFC3339); s != c.want || got.Location() != time.UTC {
				t.Fatalf("got %s (%v), want %s UTC", s, got.Location(), c.want)
			}
		})
	}
}

func TestResolveCreatedReadsTheEnvironment(t *testing.T) {
	t.Setenv(SourceDateEpochEnv, "1700000000")

	got, err := ResolveCreated("")
	if err != nil || got.Unix() != 1700000000 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestBuildManifestCreated(t *testing.T) {
	art := Artifact{
		Layer: []byte("l"), LayerMediaType: "application/x-l",
		Config: []byte("{}"), ConfigMediaType: "application/x-c",
		Annotations: map[string]string{ocispec.AnnotationCreated: "wall clock", "k": "v"},
	}

	manifestCreated := func(a Artifact) (string, bool, []byte) {
		data, err := BuildManifest(a)
		if err != nil {
			t.Fatal(err)
		}

		var m ocispec.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}

		v, ok := m.Annotations[ocispec.AnnotationCreated]

		return v, ok, data
	}

	if _, ok, _ := manifestCreated(art); ok {
		t.Error("created annotation passed through a caller map")
	}

	art.Created = time.Unix(1700000000, 999).In(time.FixedZone("x", 7200))

	v, ok, first := manifestCreated(art)
	if !ok || v != "2023-11-14T22:13:20Z" {
		t.Errorf("created = %q (%v)", v, ok)
	}

	_, _, second := manifestCreated(art)
	if string(first) != string(second) {
		t.Error("same source date, different manifest")
	}

	only := Artifact{Layer: []byte("l"), Config: []byte("{}"), Created: art.Created}
	if v, ok, _ := manifestCreated(only); !ok || v != "2023-11-14T22:13:20Z" {
		t.Errorf("no annotations map: created = %q (%v)", v, ok)
	}
}
