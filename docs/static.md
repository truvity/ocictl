# Static sites as OCI artifacts

`ocictl static push` publishes a directory (a built static site, an asset
bundle) as one deterministic OCI artifact under a version tag.
`ocictl digest <dir>` prints the digests that push would produce, without
touching a registry.

## The artifact contract

The directory is packed into ONE layer: `application/vnd.oci.image.layer.v1.tar+gzip`.

- Every regular file below `--dir` is included, at its path relative to the
  directory root (`index.html`, `assets/app.js`). There is no `.map` or other
  filter, unlike `ocictl sourcemaps push`.
- Symlinks and special files (devices, sockets, FIFOs) are refused, as is a
  directory with no files and any name that is not a clean relative slash path.
- Entries are sorted; each has mtime 0, uid/gid 0, mode 0644, no owner names;
  the gzip header is fixed. The same tree gives the same layer whatever its
  mtimes or creation order.
- No `org.opencontainers.image.created`. No title annotations are added; the
  only annotations are the ones you pass with `--annotation`.

A consumer that accepts any image manifest with a tar(+gzip) layer extracted
at the root can serve either shape.

### Shapes

| | `--shape artifact` (default) | `--shape image` |
| --- | --- | --- |
| manifest `artifactType` | `application/vnd.ocictl.static.v1` | none |
| config media type | `application/vnd.oci.empty.v1+json` | `application/vnd.oci.image.config.v1+json` |
| config content | `{}` | `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:<uncompressed tar>"]}}` |
| layers | one tar+gzip | one tar+gzip |

The image config has no `created`, no `history`. `architecture` and `os`
default to `amd64` and `linux`; override with `--architecture` and `--os`.
`diff_ids` is the digest of the uncompressed tar.

## Pushing

```bash
ocictl static push \
  --dir dist/site \
  --repository registry.example.com/example-org/site \
  --tag 1.4.0 \
  [--shape artifact|image] [--architecture amd64] [--os linux] \
  [--annotation org.opencontainers.image.source=https://example.com/repo]... \
  [--profile <aws>] [--dry-run]
```

| Flag | |
| --- | --- |
| `--dir <path>` | required |
| `--repository <host/path>` | required; no tag |
| `--tag <version>` | required; `+` becomes `_`, anything not matching `^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$` is refused |
| `--shape` | `artifact` (default) or `image` |
| `--architecture`, `--os` | image config fields (shape `image`) |
| `--annotation k=v` | repeatable manifest annotation; part of the manifest digest |
| `--profile <aws>` | AWS profile for ECR, as for the other push commands |
| `--dry-run` | print the digests, push nothing, contact no registry |

Authentication is `pkg/ocipush`'s: `GITHUB_TOKEN` for `ghcr.io`, the Docker
credential store (for example the ECR credential helper) otherwise.

`--dry-run` and `ocictl digest <dir>` print the same lines:

```
layer sha256:...
config sha256:...
manifest sha256:...
```

(`digest` takes the same `--shape`, `--architecture`, `--os` and
`--annotation` flags, since they change the config and manifest digests.)

### Idempotency

Immutable-tag registries refuse to overwrite a tag, so a release job that is
re-run would fail on the second push. `static push` resolves the tag first:

| Tag | Result |
| --- | --- |
| absent | push; prints `pushed: <repo>:<tag>@<manifest digest>` |
| present, same manifest digest | nothing written; exit 0; prints `already present: <repo>:<tag>@<manifest digest>` |
| present, different manifest digest | exit 1, naming both digests; the content changed under a released version, so cut a new version |

## Reproducibility caveats

The layer is gzip output, and the compressed bytes come from Go's
`compress/flate`. Identical content gives identical digests with the same Go
toolchain, but **a Go toolchain bump may change the layer digest** (and so
the manifest digest) for unchanged content. Consequences:

- `TestGoldenDigests` in `pkg/static` pins the digests of a fixed tree for
  both shapes, so a toolchain bump that changes compression fails CI here
  instead of silently changing published digests. Update the constants
  deliberately, in the same change as the toolchain bump.
- With a tag already published by an older ocictl, re-running a release with a
  newer one can report the "different manifest digest" error for unchanged
  content. That is the guard working; publish under a new version.
- The `diff_id` (digest of the uncompressed tar) does not depend on the
  compressor.

Pin the ocictl version in the repository that publishes (see
[consuming.md](consuming.md)) so the compressor changes only when you move it.

## With GoReleaser

As with charts and source maps, run it as a step after `goreleaser release`
([goreleaser.md](goreleaser.md)); nothing hooks into GoReleaser:

```bash
goreleaser release --clean

VERSION=$(jq -r .version dist/myproject/metadata.json)
ocictl static push \
  --dir dist/site \
  --repository ghcr.io/example-org/site \
  --tag "$VERSION"
```

Because the push is idempotent, re-running the whole job is safe.

## Go API

`pkg/layer` (deterministic packing), `pkg/static` (artifact shapes, `Describe`,
idempotent `Push`) and `pkg/ocipush` (`Resolve`, push).
