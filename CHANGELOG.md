# Changelog

Release notes for earlier versions are generated from commit history on the
[GitHub releases page](https://github.com/truvity/ocictl/releases).

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
