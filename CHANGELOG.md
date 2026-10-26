# Changelog

Release notes for earlier versions are generated from commit history on the
[GitHub releases page](https://github.com/truvity/ocictl/releases).

## v0.10.3

- Dependency updates.

## v0.10.2

- Dependency updates.

## v0.10.1

- Dependency updates.

## v0.10.0

### Added

- **`org.opencontainers.image.created` from the source date.** `static push`,
  `digest`, `sourcemaps push`, `chart push` and `crd publish` set the manifest
  annotation (and, for `static --shape image`, the config's `created`) from
  `SOURCE_DATE_EPOCH` when set, else from the new `--created <RFC3339>` flag,
  else omit it as before. RFC 3339 in UTC at seconds precision, never the wall
  clock; the same date gives byte-identical digests. Kargo reads this
  annotation for Freight creation time. Digests without a date are unchanged.
  See `docs/static.md`.

## v0.9.0

### Added

- **`ocictl`, one binary.** `ocictl chart` (was `helmctl`), `ocictl crd` (was
  `crdctl`), `ocictl sourcemaps push|serve` (was `smctl`), and two new
  commands below. The binary also answers to the old names: invoked as
  `helmctl`, `crdctl` or `smctl` (argv[0]), it behaves exactly as that tool did.
- **`ocictl static push`.** Packs a directory, with no file filter, into the
  same deterministic tar+gzip layer and pushes it under a version tag, as an
  OCI artifact (`--shape artifact`, the default) or an image (`--shape image`).
  The push is idempotent against immutable registries: a tag that already
  holds the same manifest is "already present", a different one is an error
  naming both digests. See `docs/static.md`.
- **`ocictl digest <dir>`.** Prints the layer, config and manifest digests
  without pushing; the same lines as `static push --dry-run`.
- `ocictl` is a release asset next to the existing ones.

### Deprecated

- `helmctl`, `crdctl` and `smctl` (as separate release assets and as
  `cmd/` packages). They are unchanged and still published, but will stop
  being published after two releases. Use `ocictl chart`, `ocictl crd` and
  `ocictl sourcemaps`; the table is in the README.

## v0.8.3

### Security

- Go toolchain 1.27.2, which fixes the standard-library vulnerabilities
  `govulncheck` reported against 1.27.1. golangci-lint 2.14.0 (needed to read
  Go 1.27.2 export data).

## v0.8.2

- Dependency updates.

## v0.8.1

- **The CRD charts leave this repository.** `barman-cloud-crds` moved to truvity/cnpg, `volume-snapshot-crds` and `cilium-crds` to truvity/k8s, each publishing at the upstream version it mirrors, as before. Nothing is published from here any more (`publish-charts.yaml` and the `crd-*` recipes are gone); the versions already at `ghcr.io/truvity/charts/<name>` are untouched. `crdctl` itself is unchanged and stays: the new homes run it from a pinned release.

## v0.8.0

### Added

- **`helmctl package` resolves chart dependencies that are missing.** A chart
  whose `Chart.yaml` `dependencies:` are not all present in `charts/`
  (`file://` library in the same repository, `oci://` or `https://`) has them
  resolved with `helm dependency build` before packaging, so it no longer needs
  a vendored copy of its library chart. A committed `Chart.lock` is honoured
  and a stale one refused with a clear message. When every dependency is
  already present (committed archives plus `Chart.lock`), nothing changes: no
  helm dependency step, no network, the same bytes. Resolved archives are
  normalised and `Chart.lock`'s timestamp pinned, so output stays reproducible.
  `--require-image-digests` now also checks the dependencies' `images:`.
  See `docs/goreleaser.md`.

## v0.7.1

- **The `smctl` image is published at `ghcr.io/truvity/ocictl/smctl`.** v0.7.0 pushed it to `ghcr.io/truvity/ocictl/smctl/smctl`: `base_import_paths: true` made ko append the binary name to the repository even with `bare: true`. The option is removed; nothing else changes.

## v0.7.0

### Added

- **`smctl`: source maps as OCI artifacts.** `smctl push` packs the `.map` files
  of a frontend build into a deterministic artifact (`artifactType
  application/vnd.ocictl.sourcemaps.v1`, one tar.gz layer) tagged with the
  release version and pushes it to ghcr or ECR after `goreleaser release`;
  `smctl serve` answers Grafana Alloy `faro.receiver` source-map lookups from
  those artifacts, with a size-capped LRU cache, safe unpacking and ECR,
  docker-config or anonymous registry auth. See `docs/sourcemaps.md`.
- The `smctl serve` image is published as `ghcr.io/truvity/ocictl/smctl:<version>`
  (linux/amd64 and linux/arm64, distroless static, nonroot) with each release.
- `pkg/ocipush`: `Artifact.ArtifactType` and `PushWithOptions` (plain HTTP and
  an explicit credential function). `Push` is unchanged.

## v0.6.2

- Leak hygiene: the real ECR host is gone from README, docs and code comments; `hack/leak-canary.sh` runs in `just check`.
- README pins the install example to a tag, follows the component contract's heading order and gains `Consumers` and `Neighbours`; `docs/rfc-fleet.md` tombstone removed.
- `renovate.json` extends the shared preset; ci-workflows pins moved to v3.13.1.

## v0.6.1 — 2026-09-24

### Fixed

- **`helmctl package` no longer injects every declared image into every
  chart** — a chart is now given only the images it actually declares.

## v0.6.0 — 2026-09-20

### Added

- **Strict `values.schema.json` for every CRD chart** — `cilium-crds`,
  `barman-cloud-crds` and `volume-snapshot-crds` now reject an unknown
  value instead of silently accepting it (`just chart-lint` checks this).

### Fixed

- Repaired mangled `runs-on`/`timeout` lines in the workflow files.

### Other

- `cilium-crds` tracks Cilium 1.20.1.
- Renovate and devbox-update now run from the shared `ci-caller` fleet job
  instead of a per-repo workflow.
- Shared CI bumped to `ci-workflows` v3.0.1.

## v0.5.0 — 2026-08-06

### Breaking

- **Retire the fleet toolchain** — superseded by
  [truvity/gemaal](https://github.com/truvity/gemaal), where it was rewritten
  from scratch (design: `docs/design.md` there). Nothing imported ocictl's
  copies. Removed:
  - `cmd/fleetctl` (binary, goreleaser builds/archives, `bin/fleetctl` wrapper)
  - `pkg/fleettest` — tenant resolution + `TestMain` integration-test harness
  - `pkg/fleetcfg` — committed `fleet.yaml` parsing
  - `pkg/kubewho` — `kubectl auth whoami` wrapper
  - `docs/rfc-fleet.md` replaced with a retired stub pointing at gemaal

ocictl is again pure build-time OCI tooling: `crdctl` + `helmctl`.
