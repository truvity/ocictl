package smserver

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// refusedError marks an artifact the server will not serve: malformed,
// oversized or hostile. It is a 404 to the client and is remembered.
type refusedError struct{ reason string }

func (e *refusedError) Error() string { return "artifact refused: " + e.reason }

func refuse(format string, args ...any) error {
	return &refusedError{reason: fmt.Sprintf(format, args...)}
}

// unpack extracts the regular ".map" files of a tar.gz stream into root.
//
// The whole artifact is refused (not just the entry) on a path that is
// absolute, unclean, has a ".." segment, a backslash or NUL, on a symlink,
// hardlink or device, and on exceeding the caps. Regular files that are not
// ".map" are skipped. It returns the number of files and bytes kept.
func unpack(r io.Reader, root *os.Root, lim Limits) (files int, total int64, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, refuse("layer is not gzip: %v", err)
	}

	// Entries that are skipped still cost decompression; bound the whole
	// decompressed stream (kept bytes, skipped bytes and tar framing) at
	// twice the kept cap plus a fixed allowance for framing.
	capped := &capReader{r: gz, left: 2*int64(lim.MaxTotalSize) + 4<<20}
	tr := tar.NewReader(capped)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return 0, 0, refuse("bad tar: %v", err)
		}

		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		default:
			return 0, 0, refuse("entry %q has type %q (only regular files)", hdr.Name, string(hdr.Typeflag))
		}

		name := hdr.Name
		if err := checkName(name); err != nil {
			return 0, 0, err
		}

		if !strings.HasSuffix(name, ".map") {
			continue
		}

		if files++; files > lim.MaxFiles {
			return 0, 0, refuse("more than %d files", lim.MaxFiles)
		}

		if hdr.Size > int64(lim.MaxFileSize) {
			return 0, 0, refuse("%q is %d bytes, over the %d file cap", name, hdr.Size, lim.MaxFileSize)
		}

		if total+hdr.Size > int64(lim.MaxTotalSize) {
			return 0, 0, refuse("unpacked size over the %d cap", lim.MaxTotalSize)
		}

		n, err := writeFile(root, name, tr, hdr.Size)
		if err != nil {
			return 0, 0, err
		}

		total += n
	}

	// Drain so the caller's digest verifier sees the whole blob; a gzip
	// trailer is checked here too.
	if _, err := io.Copy(io.Discard, capped); err != nil {
		return 0, 0, refuse("bad gzip: %v", err)
	}

	if files == 0 {
		return 0, 0, refuse("no .map file in the artifact")
	}

	return files, total, nil
}

func checkName(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name {
		return refuse("entry name %q is not a clean relative path", name)
	}

	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return refuse("entry name %q escapes the root", name)
		}
	}

	return nil
}

func writeFile(root *os.Root, name string, src io.Reader, size int64) (int64, error) {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return 0, refuse("create directory for %q: %v", name, err)
		}
	}

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, refuse("create %q: %v", name, err)
	}

	// Read one byte past the declared size: a header that lies about the size
	// cannot make us write more than we budgeted.
	n, err := io.Copy(f, io.LimitReader(src, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return 0, refuse("write %q: %v", name, err)
	}

	if n != size {
		return 0, refuse("%q is larger than its header says", name)
	}

	return n, nil
}

var errStreamCap = errors.New("decompressed stream over the cap")

// capReader fails (rather than reporting EOF) once more than left bytes have
// been read.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errStreamCap
	}

	if int64(len(p)) > c.left {
		p = p[:c.left]
	}

	n, err := c.r.Read(p)
	c.left -= int64(n)

	return n, err
}
