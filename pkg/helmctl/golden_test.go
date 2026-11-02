package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// goldenNormalizedChart pins the bytes NormalizeTgz writes for a fixed chart
// archive. The compressor is the one pinned in go.mod (layer.NewGzipWriter), so
// a Go toolchain bump must not move this digest; CI runs it under two toolchains
// (just golden). If it moves, every chart layer digest moves: see CHANGELOG.md.
const goldenNormalizedChart = "sha256:f99ef8a0378bd67ce0b2530abf0822796aca78a1261381556d2f59f852124357"

func TestGoldenNormalizeTgz(t *testing.T) {
	var in bytes.Buffer

	gz := gzip.NewWriter(&in)
	tw := tar.NewWriter(gz)

	files := map[string]string{
		"demo/Chart.yaml":              "apiVersion: v2\nname: demo\nversion: 1.0.0\n",
		"demo/values.yaml":             "replicas: 2\nimage:\n  repository: example.com/demo\n  tag: \"1.0.0\"\n",
		"demo/templates/deploy.yaml":   strings.Repeat("kind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n---\n", 400),
		"demo/templates/service.yaml":  strings.Repeat("kind: Service\nspec:\n  ports:\n    - port: 80\n---\n", 300),
		"demo/templates/_helpers.tpl":  strings.Repeat("{{- define \"demo.name\" -}}demo{{- end -}}\n", 200),
		"demo/charts/.helmignore":      ".git\n*.swp\n",
		"demo/templates/configmap.yml": strings.Repeat("data:\n  key: value\n", 500),
	}

	// Insert in a fixed order with noisy mtimes: normalization sorts and zeroes them.
	for i, name := range []string{
		"demo/values.yaml", "demo/Chart.yaml", "demo/templates/service.yaml", "demo/templates/deploy.yaml",
		"demo/templates/_helpers.tpl", "demo/charts/.helmignore", "demo/templates/configmap.yml",
	} {
		body := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000+int64(i)*977, 0), Uid: 1000 + i, Gid: 100}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := NormalizeTgz(in.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(out)
	got := "sha256:" + hex.EncodeToString(sum[:])
	t.Logf("normalized %d bytes, digest %s", len(out), got)

	if got != goldenNormalizedChart {
		t.Errorf("normalized chart digest %s, want %s", got, goldenNormalizedChart)
	}
}
