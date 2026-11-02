package layer_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/truvity/ocictl/pkg/layer"
)

// goldenGzip pins the compressed bytes of fixedPayload. The compressor is
// github.com/klauspost/compress/gzip at the version in go.mod, so this digest
// must not move with a Go toolchain: CI runs this test under two toolchains
// (just golden). It moves only when that dependency is moved on purpose, and
// then every published layer digest moves with it (see CHANGELOG.md).
const goldenGzip = "sha256:4ae4890107feac6618ec6bc187f2eeedc6975553388ec504bd5be97f007c5bf5"

// fixedPayload is text-like data with enough repetition and variety (about
// 300 KiB) that the compressor's match finding and block splitting matter.
func fixedPayload() []byte {
	var b strings.Builder

	state := uint64(0x9E3779B97F4A7C15)
	words := []string{"image", "layer", "digest", "manifest", "registry", "chart", "values", "tag", "annotation", "sha256"}

	for b.Len() < 300<<10 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17

		b.WriteString(words[state%uint64(len(words))])

		if state%7 == 0 {
			b.WriteString(hex.EncodeToString([]byte{byte(state >> 8), byte(state >> 16), byte(state >> 24)}))
		}

		b.WriteByte(' ')
	}

	return []byte(b.String())
}

func TestGzipIsPinned(t *testing.T) {
	var buf bytes.Buffer

	zw := layer.NewGzipWriter(&buf)
	if _, err := zw.Write(fixedPayload()); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(buf.Bytes())
	got := "sha256:" + hex.EncodeToString(sum[:])
	t.Logf("compressed %d bytes, digest %s", buf.Len(), got)

	if got != goldenGzip {
		t.Errorf("compressed digest %s, want %s: the compressor's output moved, so every published layer digest would", got, goldenGzip)
	}

	// The header is neutral and the content round-trips with the standard reader.
	hdr := buf.Bytes()[:10]
	if hdr[3] != 0 || !bytes.Equal(hdr[4:8], []byte{0, 0, 0, 0}) || hdr[9] != 255 {
		t.Errorf("gzip header not neutral: % x", hdr)
	}

	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}

	back, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(back, fixedPayload()) {
		t.Fatalf("round trip: %v", err)
	}
}
