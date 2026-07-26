package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"time"
)

// NormalizeTgz repacks a gzipped tarball deterministically: entries sorted
// alphabetically, timestamps normalized to epoch 0, uid/gid/uname/gname
// cleared, gzip header ModTime zeroed. Same content in → same bytes out,
// regardless of when or where the original was produced.
//
// Used on chart layers before OCI push (Push) and on vendored dependency
// tarballs inside a parent chart (Package with VendorDependencies) — an
// embedded charts/*.tgz is opaque bytes to the outer normalization pass,
// so it must be normalized on its own before the parent is packaged.
func NormalizeTgz(origData []byte) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(origData))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	type tarEntry struct {
		Header *tar.Header
		Data   []byte
	}

	var entries []tarEntry
	tarReader := tar.NewReader(gzReader)

	for {
		hdr, readErr := tarReader.Next()
		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			return nil, fmt.Errorf("read tar entry: %w", readErr)
		}

		var data []byte
		if hdr.Size > 0 {
			data, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("read tar data for %s: %w", hdr.Name, err)
			}
		}

		entries = append(entries, tarEntry{Header: hdr, Data: data})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Header.Name < entries[j].Header.Name
	})

	epoch := time.Unix(0, 0)

	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	gzWriter.ModTime = epoch
	tarWriter := tar.NewWriter(gzWriter)

	for i := range entries {
		e := &entries[i]
		e.Header.ModTime = epoch
		e.Header.AccessTime = time.Time{}
		e.Header.ChangeTime = time.Time{}
		e.Header.Uid = 0
		e.Header.Gid = 0
		e.Header.Uname = ""
		e.Header.Gname = ""

		if err := tarWriter.WriteHeader(e.Header); err != nil {
			return nil, fmt.Errorf("write header for %s: %w", e.Header.Name, err)
		}

		if len(e.Data) > 0 {
			if _, err := tarWriter.Write(e.Data); err != nil {
				return nil, fmt.Errorf("write data for %s: %w", e.Header.Name, err)
			}
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}

	return buf.Bytes(), nil
}
