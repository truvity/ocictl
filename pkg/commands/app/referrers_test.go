package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/referrerscmd"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func TestAttachAndReferrers(t *testing.T) {
	old := referrerscmd.RegistryOptions
	referrerscmd.RegistryOptions = func(string) ocipush.Options {
		return ocipush.Options{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		}}
	}

	defer func() { referrerscmd.RegistryOptions = old }()

	defer func(d time.Duration) { ocipush.ReferrersSettle = d }(ocipush.ReferrersSettle)
	ocipush.ReferrersSettle = 20 * time.Millisecond

	for name, api := range map[string]bool{"referrers-api": true, "tag-schema": false} {
		t.Run(name, func(t *testing.T) {
			reg := fakeregistry.New("", "")
			defer reg.Close()
			reg.SetReferrersAPI(api)

			body := `{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.empty.v1+json",` +
				`"digest":"` + "'+EMPTY+'" + `","size":2},"layers":[]}`
			subject := reg.PutManifest("org/app", "1.0.0", ocispec.MediaTypeImageManifest, []byte(body))
			ref := reg.Host() + "/org/app@" + subject

			file := filepath.Join(t.TempDir(), "app.spdx.json")
			if err := os.WriteFile(file, []byte(`{"spdxVersion":"SPDX-2.3"}`), 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := run(t, "ocictl", "attach", "--subject", ref, "--artifact-type", "application/spdx+json",
				"--media-type", "application/spdx+json", file)
			if err != nil || !strings.HasPrefix(out, "attached: "+reg.Host()+"/org/app@sha256:") || !strings.Contains(out, "(subject "+subject+", "+name+")") {
				t.Fatalf("%q %v", out, err)
			}

			// A tag before the @ is ignored; the digest names the subject.
			listed, err := run(t, "ocictl", "referrers", reg.Host()+"/org/app:1.0.0@"+subject)
			if err != nil || !strings.Contains(listed, "application/spdx+json") {
				t.Fatalf("%q %v", listed, err)
			}

			js, err := run(t, "ocictl", "referrers", ref, "--output", "json", "--artifact-type", "application/spdx+json")
			if err != nil {
				t.Fatal(err)
			}

			var doc struct {
				Subject   string
				Mode      string
				Referrers []ocispec.Descriptor
			}

			if err := json.Unmarshal([]byte(js), &doc); err != nil || doc.Mode != name || doc.Subject != subject || len(doc.Referrers) != 1 {
				t.Fatalf("%q %+v %v", js, doc, err)
			}

			if none, err := run(t, "ocictl", "referrers", ref, "--artifact-type", "application/vnd.example"); err != nil || none != "" {
				t.Fatalf("filter: %q %v", none, err)
			}
		})
	}

	if _, err := run(t, "ocictl", "attach", "--subject", "registry.example.com/org/app:1.0.0", "--artifact-type", "x/y", "f"); err == nil {
		t.Error("a tag as the subject was accepted")
	}

	if _, err := run(t, "ocictl", "referrers", "registry.example.com/org/app"); err == nil {
		t.Error("a reference without a digest was accepted")
	}
}
