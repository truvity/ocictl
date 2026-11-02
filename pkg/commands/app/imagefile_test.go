package app_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/commands/app"
	"github.com/truvity/ocictl/pkg/commands/imagecmd"
	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
)

func tarGz(t *testing.T, files [][2]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestImageFileDigestAndExtract(t *testing.T) {
	old := imagecmd.RegistryOptions
	imagecmd.RegistryOptions = func(string) ocipush.Options {
		return ocipush.Options{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		}}
	}

	defer func() { imagecmd.RegistryOptions = old }()

	reg := fakeregistry.New("", "")
	defer reg.Close()

	// Two layers: the second replaces the entry point and whiteouts another file.
	l1 := tarGz(t, [][2]string{{"usr/libexec/app/entry", "old"}, {"etc/gone", "g"}})
	l2 := tarGz(t, [][2]string{{"usr/libexec/app/entry", "new"}, {"etc/.wh.gone", ""}})
	cfg := []byte(`{"os":"linux","architecture":"amd64"}`)

	m := ocispec.Manifest{Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(reg.PutBlob(cfg)), Size: int64(len(cfg))}}
	m.SchemaVersion = 2

	for _, l := range [][]byte{l1, l2} {
		m.Layers = append(m.Layers, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.Digest(reg.PutBlob(l)), Size: int64(len(l))})
	}

	body, _ := json.Marshal(m)
	image := reg.PutManifest("org/app", "1.0.0", ocispec.MediaTypeImageManifest, body)
	ref := reg.Host() + "/org/app@" + image

	h := sha256.Sum256([]byte("new"))
	want := "sha256:" + hex.EncodeToString(h[:])

	got, err := run(t, "ocictl", "image", "file-digest", ref, "--path", "/usr/libexec/app/entry")
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}

	if _, err := run(t, "ocictl", "image", "file-digest", ref, "--path", "/usr/libexec/app/entry", "--expect", want); err != nil {
		t.Fatalf("--expect with the right digest: %v", err)
	}

	if _, err := run(t, "ocictl", "image", "file-digest", ref, "--path", "/usr/libexec/app/entry", "--expect", "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("--expect with the wrong digest passed")
	}

	dest := filepath.Join(t.TempDir(), "entry")

	out, err := run(t, "ocictl", "image", "extract", ref, "--path", "usr/libexec/app/entry", "-o", dest)
	if err != nil || out != want+" "+dest {
		t.Fatalf("%q %v", out, err)
	}

	if data, err := os.ReadFile(dest); err != nil || string(data) != "new" { //nolint:gosec // test path
		t.Fatalf("%q %v", data, err)
	}

	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v %v", st, err)
	}

	// The whiteout hides the file: exit status 2 through the real entry point.
	if code := app.Run([]string{"ocictl", "image", "file-digest", ref, "--path", "/etc/gone"}, "test"); code != 2 {
		t.Errorf("a whited-out file: exit %d, want 2", code)
	}

	if code := app.Run([]string{"ocictl", "image", "file-digest", ref, "--path", "/usr/libexec/app/entry", "--expect", "sha256:00"}, "test"); code != 1 {
		t.Errorf("a digest mismatch: exit %d, want 1", code)
	}

	// A failed extract leaves nothing behind, not even a temp file.
	if _, err := run(t, "ocictl", "image", "extract", ref, "--path", "/etc/gone", "-o", filepath.Join(filepath.Dir(dest), "gone")); err == nil {
		t.Error("extracting a whited-out file succeeded")
	}

	if entries, err := os.ReadDir(filepath.Dir(dest)); err != nil || len(entries) != 1 {
		t.Errorf("the destination directory holds %d entries, want only the extracted file: %v", len(entries), err)
	}
}
