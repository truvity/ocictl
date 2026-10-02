package sourcemaps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Packed is a ready-to-push layer.
type Packed struct {
	// Layer is the tar.gz bytes.
	Layer []byte
	// Files are the archive paths in archive order.
	Files []string
}

// PackDir packs the files below dir into the deterministic layer.
//
// Only regular files ending in ".map" are accepted. Any other regular file
// makes PackDir fail unless it matches one of include (a path.Match pattern,
// tried against the slash-separated path relative to dir and, when the
// pattern has no "/", against the base name). Symlinks and other special
// files are always refused, and so is a directory holding no ".map" file.
func PackDir(dir string, include []string) (*Packed, error) {
	for _, pattern := range include {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("--include %q: %w", pattern, err)
		}
	}

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

		if !strings.HasSuffix(rel, MapSuffix) && !matchesAny(include, rel) {
			return fmt.Errorf("%s: not a %s file; allow it explicitly with --include", rel, MapSuffix)
		}

		if err := validateArchivePath(rel); err != nil {
			return err
		}

		entries = append(entries, entry{rel: rel, abs: p})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	maps := 0

	for _, e := range entries {
		if strings.HasSuffix(e.rel, MapSuffix) {
			maps++
		}
	}

	if maps == 0 {
		return nil, fmt.Errorf("%s holds no %s file", dir, MapSuffix)
	}

	var buf bytes.Buffer

	// Fixed header: no name, no mtime, OS "unknown" (Go's default).
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}

	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
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

	return &Packed{Layer: buf.Bytes(), Files: files}, nil
}

func addFile(tw *tar.Writer, rel, abs string) error {
	f, err := os.Open(abs) //nolint:gosec // caller-provided maps dir
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

func matchesAny(patterns []string, rel string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}

		if !strings.Contains(p, "/") {
			if ok, _ := path.Match(p, path.Base(rel)); ok {
				return true
			}
		}
	}

	return false
}

// validateArchivePath refuses names a server would refuse to unpack.
func validateArchivePath(name string) error {
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
