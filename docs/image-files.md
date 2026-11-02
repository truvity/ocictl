# Reading a file out of a remote image

A release test sometimes has to prove that the executable inside an image is the
same, byte for byte, as a file the build produced (a package, a binary in a
release archive). That needs a file out of a *remote* image without a Docker
daemon:

```bash
ocictl image file-digest registry.example.com/example-org/app@sha256:<digest> \
  --path /usr/libexec/app/entry [--platform linux/arm64] [--expect sha256:<digest>]

ocictl image extract registry.example.com/example-org/app@sha256:<digest> \
  --path /usr/libexec/app/entry [--platform linux/arm64] -o ./entry   # "-o -" is stdout
```

- `file-digest` prints `sha256:<digest>` of the file's content. With `--expect` it
  exits 1 unless the digest matches. `extract` writes the file (mode from the
  image, never wider than 0755) and prints `sha256:<digest> <path>`; it writes a
  temp file beside the destination and renames, so a failure leaves nothing.
- The file is the one the **flattened** image has: the topmost layer that holds
  the path wins; a whiteout (`.wh.<name>`), an opaque directory marker
  (`.wh..wh..opq`) or a whiteout of an ancestor directory in a later layer hides
  it. A file re-created in the same layer as its whiteout is there.
- **Exit status 2** when the path is not in the image (never there, or hidden by
  a whiteout; the message says which). Any other failure is 1.
- `<digest>` may name an image manifest or an index. For an index, `--platform
  os/arch[/variant]` selects the image; it may be left out only when the index
  holds one image (attestation entries do not count).
- The path must be a regular file. A symlink is an error (links are not followed);
  a hard link reads as its target in the same layer; a directory is an error.
- Layers are read as gzip or uncompressed tar. Other layer media types (zstd) are
  refused with their name.
- Registry read access only: `GITHUB_TOKEN` for `ghcr.io` when set (public images
  also work without), the Docker credential store otherwise.

Unlike `docker create` + `docker cp`, nothing is run or unpacked to disk, and
layers are read from the top down until the path is found.
