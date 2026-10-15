// Package layer packs a directory into the deterministic tar+gzip layer that
// ocictl's directory artifacts (source maps, static sites) share.
//
// The bytes depend only on the file paths and contents: entries are sorted,
// headers carry no owner, mtime or name, and the gzip header is fixed. The
// compressed bytes also depend on the Go toolchain's compress/flate; a
// toolchain bump may change them (see docs/static.md).
package layer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

// Packed is a ready-to-push layer.
type Packed struct {
	// Layer is the tar.gz bytes.
	Layer []byte
	// DiffID is the digest of the uncompressed tar (an image config's
	// rootfs.diff_ids entry).
	DiffID digest.Digest
	// Files are the archive paths in archive order.
	Files []string
}

// Accept decides whether a regular file (slash path relative to the packed
// directory) enters the archive. Returning an error aborts packing.
type Accept func(rel string) (bool, error)

// Pack packs the regular files below dir. Symlinks and special files are
// always refused. A file for which accept returns false is skipped; a nil
// accept takes every file. Pack does not require any file to be present.
func Pack(dir string, accept Accept) (*Packed, error) {
	type entry struct{ rel, abs string }

	var entries []entry

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file (symlinks and special files are refused)", rel)
		}

		if accept != nil {
			ok, err := accept(rel)
			if err != nil {
				return err
			}

			if !ok {
				return nil
			}
		}

		if err := ValidatePath(rel); err != nil {
			return err
		}

		entries = append(entries, entry{rel: rel, abs: p})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	var buf bytes.Buffer

	// Fixed header: no name, no mtime, OS "unknown" (Go's default).
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}

	gz.ModTime = time.Time{}

	sum := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(gz, sum))
	files := make([]string, 0, len(entries))

	for _, e := range entries {
		if err := addFile(tw, e.rel, e.abs); err != nil {
			return nil, err
		}

		files = append(files, e.rel)
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}

	return &Packed{
		Layer:  buf.Bytes(),
		DiffID: digest.NewDigestFromBytes(digest.SHA256, sum.Sum(nil)),
		Files:  files,
	}, nil
}

func addFile(tw *tar.Writer, rel, abs string) error {
	f, err := os.Open(abs) //nolint:gosec // caller-provided dir
	if err != nil {
		return err
	}

	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return err
	}

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     rel,
		Size:     info.Size(),
		Mode:     0o644,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}

	if n != info.Size() {
		return fmt.Errorf("%s changed while packing", rel)
	}

	return nil
}

// ValidatePath refuses names a server would refuse to unpack.
func ValidatePath(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name {
		return fmt.Errorf("%q is not a clean relative slash path", name)
	}

	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("%q has an unsafe path segment", name)
		}
	}

	return nil
}
