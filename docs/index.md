# Merging per-architecture images into one index

Some images are built natively, one job per architecture (a cgo driver that
links a system library, say), so the registry holds two single-platform images
and no tag that serves both. GoReleaser's split/merge is a Pro feature, and
`docker buildx imagetools create` needs a daemon. `ocictl index create` merges
them without either:

```bash
ocictl index create \
  --repository registry.example.com/example-org/app --tag 2.0.0 \
  --manifest registry.example.com/example-org/app@sha256:<amd64 image> \
  --manifest registry.example.com/example-org/app@sha256:<arm64 image> \
  [--annotation k=v]... [--created <RFC3339>] [--profile <aws>] [--output json]
```

- Each `--manifest` is `<host/path>@<digest>`, optionally `:<os>/<arch>[/<variant>]`.
  Without the platform it is read from the image's config; name it when the
  config is wrong or absent. A tag before the `@` is ignored.
- Every input must be a single-platform image manifest (not an index) that exists
  in its registry. The same digest twice, or two images for one platform, are
  refused.
- The index lists the images sorted by platform, so the digest does not depend on
  the order of the flags. `org.opencontainers.image.created` only from
  `SOURCE_DATE_EPOCH` or `--created`, as for the other commands.
- **Idempotent**, like `static push`: the tag absent, push; present with the same
  index digest, write nothing and print `already present`; present with another
  digest, exit 1 naming both digests.
- Inputs from another repository of the same registry are copied into
  `--repository` by digest first (blobs by cross-repository mount when the
  registry allows it, a copy otherwise), because an index can only list manifests
  its own repository holds. Pushing therefore needs write access to
  `--repository`, read access to the inputs'.

`--output json` prints `ref`, `repository`, `tag`, `digest`, `status`
(`pushed` or `already-present`), `artifactType`, `mediaType` and `platforms`.

## Using the digest

A release manifest (`chart package --manifest`) pins images by digest. Put the
index digest, not either single-platform digest, in it:

```yaml
version: 2.0.0
values:
  images:
    driver:
      registry: registry.example.com
      repository: example-org/app
      tag: "2.0.0"
      digest: sha256:<the index digest printed by index create>
```

`ocictl images verify` (when present) and `chart package --require-image-digests`
then see one digest that serves both architectures. There is no flag that edits
a generated manifest; the digest is written by hand or by the caller's script
from `--output json`.
