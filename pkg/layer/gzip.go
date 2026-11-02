package layer

import (
	"io"
	"time"

	"github.com/klauspost/compress/gzip"
)

// GzipLevel is the one compression level every published layer uses.
const GzipLevel = gzip.BestCompression

// NewGzipWriter returns the compressor of every layer ocictl publishes (the
// static, source-map and chart layers).
//
// It is github.com/klauspost/compress/gzip, not the standard library's
// compress/gzip, because the standard library's compress/flate output is not a
// promise: a Go release may change the bytes for unchanged content, and with
// them the layer and manifest digests, so that an immutable tag then rejects a
// rebuild after a toolchain bump. This module's version is pinned in go.mod and
// the golden digests in the tests pin its output, so the digests move only when
// that dependency is moved on purpose.
//
// The header carries no name and an mtime of zero (the Unix epoch, which is what
// the standard library writes for a zero time; this package writes the time as
// given, so the writer is created with it). OS is 255, "unknown".
func NewGzipWriter(w io.Writer) *gzip.Writer {
	zw, err := gzip.NewWriterLevel(w, GzipLevel)
	if err != nil {
		// Only an invalid level fails, and GzipLevel is a constant.
		panic("layer: " + err.Error())
	}

	zw.ModTime = time.Unix(0, 0)

	return zw
}
