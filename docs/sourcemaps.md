# Source maps as OCI artifacts

A browser SPA ships minified JavaScript; when it reports an error through
Grafana Faro, the stack frames point into `assets/index-3fa9.js:1:48213` and
mean nothing until they are mapped back to source. `smctl` makes the source
maps a first-class build output:

- `smctl push` packs the `.map` files of one build into a deterministic OCI
  artifact and pushes it to the same registry the application image goes to
  (ghcr for a public repository, ECR for a private one — the mechanism is the
  same), tagged with the release version.
- `smctl serve` is a small HTTP service that Grafana Alloy's `faro.receiver`
  asks for a map by URL. It pulls the artifact for that release on first use,
  unpacks it into a bounded cache and answers from disk.

Maps never ride in the application image, so they are never served to a
browser by the application, and they are stored and expired by the registry
the way images are.

## The artifact

| Part | Value |
| --- | --- |
| manifest | OCI image manifest, `artifactType: application/vnd.ocictl.sourcemaps.v1` |
| config | `application/vnd.oci.empty.v1+json`, content `{}` |
| layers | exactly one: `application/vnd.oci.image.layer.v1.tar+gzip` |
| layer content | the `*.map` files, each at the path it is requested under (`assets/index-3fa9.js.map`) |
| tag | the release version, `+` replaced by `_`; anything not matching `^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$` is refused |
| annotation | `org.opencontainers.image.version` = the unsanitised version; **no** `created` |

The artifact is addressed by **version tag only**. There is no OCI subject or
referrer link to the application image: the server needs nothing but the
repository and the release string Faro sends.

Packing is deterministic. Entries are sorted, with mtime 0, uid/gid 0, mode
0644, no owner names, and a fixed gzip header; the same files always give the
same layer and the same manifest digest, so re-running a release re-pushes
identical bytes (`smctl push --dry-run` prints the digest without pushing).

## Publishing

`smctl push` runs in the same job as `goreleaser release`, right after it —
the pattern `helmctl` uses for charts (see [docs/goreleaser.md](goreleaser.md)).
The version and the image's registry come from GoReleaser's `dist/`, so a
release that did not publish cannot push maps for it.

```bash
# derive version and repository from the release
smctl push --goreleaser-dist dist/myproject --image web --maps dist-sourcemaps

# the same, for a build GoReleaser did not run
smctl push --repository ghcr.io/example-org/sourcemaps/web --version 1.4.0 --maps dist-sourcemaps
```

| Flag | |
| --- | --- |
| `--maps <dir>` | required; searched recursively. Refused if it holds no `.map` file, a symlink or special file, or any non-`.map` file not allowed by `--include` |
| `--include <pattern>` | repeatable; also pack non-`.map` files matching a `path.Match` pattern. (The server keeps only `.map` files regardless.) |
| `--goreleaser-dist <dir>` | takes the version (and, with `--image`, the registry and repository) from `metadata.json` / `artifacts.json` |
| `--image <name>` | an image the release published: its repository, `registry/repository`, or last path segment |
| `--repository-template <tpl>` | default `{registry}/{owner}/sourcemaps/{app}`; variables `{registry}`, `{repository}`, `{owner}` (first path segment), `{app}` |
| `--app <name>` | `{app}` (default: last segment of the image repository) |
| `--repository <repo>` / `--version <ver>` | explicit, instead of the above |
| `--profile <aws>` | AWS profile for ECR (as for `helmctl push`) |
| `--dry-run` | pack and print `repo:tag@digest`; push nothing |

Authentication is `pkg/ocipush`'s: `GITHUB_TOKEN` for `ghcr.io`, the Docker
credential store (for example the ECR credential helper) for anything else.
Nothing about a real registry or account is built in.

The GoReleaser job snippet: build the SPA once, let the image copy `dist/`,
move the maps out so they are not in the image, then push them. See
[docs/goreleaser.md](goreleaser.md#source-maps-in-the-same-job).

## Serving to Alloy

```bash
smctl serve --config /etc/smctl/config.yaml
```

```yaml
listen: ":8080"
repositoryTemplate: ghcr.io/example-org/sourcemaps/{app}   # per-app `repository:` overrides
apps:
  - name: web
  - name: admin
    repository: ghcr.io/example-org/internal/admin-maps
auth:
  mode: anonymous            # anonymous | ecr | dockerConfig
  # dockerConfig: /etc/smctl/docker/config.json   (mode dockerConfig)
cache:
  dir: /tmp/smctl-cache      # emptied at start
  maxSize: 1GiB              # unpacked bytes kept; whole releases are evicted LRU
limits:
  maxLayerSize: 256MiB       # compressed layer
  maxTotalSize: 512MiB       # unpacked bytes of one release
  maxFileSize: 64MiB
  maxFiles: 10000
negativeCache:
  ttl: 30s
  maxEntries: 4096
fetchTimeout: 60s
```

Every key is optional except `apps` and a repository for each app; the values
above are the defaults. Unknown keys are an error. Sizes are bytes or take a
`KiB`/`MiB`/`GiB` suffix.

### Request shape

```
GET|HEAD /<app>/<release>/<script path>.map
```

This is exactly what Alloy's `faro.receiver` sends. For a frame whose script is
`https://app.example/assets/index-3fa9.js?x=1`, a location with
`minified_path_prefix = "https://app.example/"` and
`path = "http://smctl:8080/web/{{ .Release }}"` produces
`GET http://smctl:8080/web/<release>/assets/index-3fa9.js.map`. Only a `200` is
a hit for Alloy; any other status is cached by Alloy as a miss for its
`error_cleanup_interval`.

```alloy
faro.receiver "app" {
  server { listen_address = "0.0.0.0" }

  sourcemaps {
    location {
      path               = "http://smctl.observability.svc:8080/web/{{ .Release }}"
      minified_path_prefix = "https://app.example/"
    }
  }
}
```

Alloy removes `/` and `\` from the release and trims a leading `.`; it does
not escape `+`, so the server applies the same `+` → `_` as the publisher.
The Faro `app.release` must therefore equal the version the maps were pushed
under (for GoReleaser, `{{ .Version }}`, without the `v`).

### Responses

| Status | When |
| --- | --- |
| `200` | the map; `Content-Type: application/json`, short `Cache-Control` |
| `404` | unknown app; malformed path or release; no artifact for the release; the registry denies access (ghcr answers an anonymous pull of a missing or private package with 403, so this is usually "no such repository yet"; logged as a warning and counted in `smctl_denied_total`); an artifact the server refuses; no such file in the artifact |
| `405` | any method but GET and HEAD |
| `502` | the registry failed (5xx) or is unreachable (not remembered, so the next request retries) |

`GET /healthz` is the liveness/readiness check. `GET /metrics` is Prometheus
text: requests by status, cache hits, loads, load failures, refusals,
denied, not-found, negative hits, evictions, cache bytes.

### Authentication

| `auth.mode` | Registry login |
| --- | --- |
| `anonymous` | none beyond the registry's own anonymous token flow (a public ghcr package) |
| `ecr` | `ecr:GetAuthorizationToken` through the default AWS credential chain, in the region parsed from the repository host; the token is cached until about five minutes before it expires. Every repository host must be an ECR host or the server will not start. Grant the pod's role `ecr:GetAuthorizationToken` plus `ecr:BatchGetImage` and `ecr:GetDownloadUrlForLayer` on the map repositories. |
| `dockerConfig` | credentials from the docker `config.json` at `auth.dockerConfig` |

A credential is only ever sent to the host of a configured repository — not to
a token service on another host, not to anything a registry redirects to.

### Retention

The maps live in an ordinary registry repository, so retention is the
repository's lifecycle policy — expire map tags the way you expire images, and
keep them at least as long as the release can still report errors. The server's
cache is only a cache: it is emptied at start and anything it evicts is pulled
again on demand.

## Security model

- **No SSRF.** The URL's first segment must be a configured application name;
  it selects a configured repository. No part of a request becomes a registry
  host, repository or credential.
- **Strict artifact checks.** Manifest must be an image manifest with the
  expected artifact type (or, for `oras push` without `--artifact-type`, the
  expected config type), exactly one layer of the expected media type, within
  the layer cap. The blob digest is verified while it is read; nothing from a
  blob that fails verification is kept.
- **Safe unpack.** Extraction is confined by `os.Root`. The whole artifact is
  refused on an absolute or unclean path, `..`, backslash, NUL, a duplicate, a
  symlink, hardlink, device or FIFO. Only regular `.map` files are kept. File
  count, size of a file, unpacked total, and the decompressed stream are all
  capped, and an artifact bigger than the whole cache is refused.
- **Bounded memory and disk.** LRU by whole release against a size cap; one
  pull per release at a time; a bounded negative cache with a TTL.
- **Read-only credentials.** The server only pulls; it needs no push rights.
- Maps can contain your original source. Serve `smctl` only on the in-cluster
  address Alloy uses; it has no authentication of its own.

## Go API

`pkg/sourcemaps` (pack, tag, push) and `pkg/smserver` (the handler) are
importable; `smctl` is a thin wrapper.
