# Changelog

Release notes for earlier versions are generated from commit history on the
[GitHub releases page](https://github.com/truvity/ocictl/releases).

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
