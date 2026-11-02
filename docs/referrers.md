# Referrers: attach and list

`ocictl attach` pushes files as an OCI artifact whose `subject` is an existing
manifest, so the registry can list it as a **referrer** of that manifest (an
SBOM, a signature, an attestation). `ocictl referrers` lists them.

```bash
ocictl attach --subject registry.example.com/example-org/app@sha256:<digest> \
  --artifact-type application/spdx+json --media-type application/spdx+json \
  [--annotation k=v]... [--created <RFC3339>] [--profile <aws>] [--output json] app.spdx.json

ocictl referrers registry.example.com/example-org/app@sha256:<digest> \
  [--artifact-type application/spdx+json] [--output json]
```

- The subject is always a **digest**: a tag may move, a referrer is bound to the
  content it describes. A tag before the `@` is ignored.
- Each file becomes one layer (`--media-type`, default `application/octet-stream`)
  titled with its base name (`org.opencontainers.image.title`); the config is the
  OCI empty descriptor. The same files and flags give the same manifest digest,
  so attaching again is a no-op.
- Credentials are those of the other push commands: `GITHUB_TOKEN` for
  `ghcr.io`, the Docker credential store otherwise.

## Two ways a registry lists referrers

| | How | Seen on |
| --- | --- | --- |
| referrers API | `GET /v2/<name>/referrers/<digest>`; the registry indexes a pushed manifest that has a subject | registries implementing OCI distribution 1.1 |
| tag schema | an image index tagged `sha256-<hex of the subject digest>` lists the referrers, each entry with its `artifactType` and annotations | GHCR (measured 2026-11-01: the API answers 404) |

`attach` and `referrers` detect which one the registry has (`mode` in the output:
`referrers-api` or `tag-schema`). Signatures and attestations pushed by cosign and
`actions/attest` on GHCR are found the same way.

### The tag-schema write

A registry tag cannot be written conditionally, so two writers that read the index
at the same moment both write it and the second write drops the first writer's
entry. `attach` therefore does a read-modify-write, waits `ReferrersSettle`
(300 ms plus jitter), **reads the index back**, and repeats (up to 10 times) if
its entry, or one it merged, is gone. Entries are merged by digest and sorted, so
the index does not depend on writer order. This closes the race for writers that
overlap; a writer whose stale read-modify-write lands after another writer has
already finished its check can still, in principle, drop that writer's entry,
which is why `referrers` is the thing to run to see what a subject has.

Old index manifests are not deleted (they are harmless and deleting needs a
permission some tokens lack).

## Tests

Against `pkg/internal/fakeregistry`, in both modes, including six concurrent
attaches to one subject. `TestAttachLiveRegistry` runs against a real registry
when `OCICTL_TEST_SUBJECT` (`<host/path>@<digest>` you may write to) is set, and
is skipped otherwise; it has not been run live from this change. Reading with
`oras discover` or `cosign tree` was not run either; the tests read the result
back with oras-go's own `Referrers`, which `oras discover` is built on.
