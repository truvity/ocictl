# ocictl

[![CI](https://github.com/truvity/ocictl/actions/workflows/ci.yaml/badge.svg)](https://github.com/truvity/ocictl/actions/workflows/ci.yaml)
[![Release](https://github.com/truvity/ocictl/actions/workflows/release.yaml/badge.svg)](https://github.com/truvity/ocictl/actions/workflows/release.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/truvity/ocictl.svg)](https://pkg.go.dev/github.com/truvity/ocictl)
[![Go Report Card](https://goreportcard.com/badge/github.com/truvity/ocictl)](https://goreportcard.com/report/github.com/truvity/ocictl)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Deterministic OCI chart packaging and CRD repack tooling.

## Who it is for

Anyone who needs to turn a build into a **digest-pinned, immutable** Helm
chart and push it to an OCI registry (GHCR or a private ECR), or who needs
to repack a third party's CRDs into a chart. Two binaries; `ocictl` is the
repository name, never a command:

| Binary      | Purpose                                                    |
| ----------- | ---------------------------------------------------------- |
| **helmctl** | Deterministic Helm chart packaging + OCI push (GHCR + ECR) |
| **crdctl**  | Fetch upstream CRDs → generate chart → package → push      |

AI agents: start with **[AGENTS.md](AGENTS.md)** — the exhaustive command
surface and the rule for not inventing one.

The fleet toolchain (`fleetctl` + libraries) that briefly lived here was
retired in v0.5.0 — superseded by [truvity/gemaal](https://github.com/truvity/gemaal).
ocictl is build-time OCI tooling only.

## The model

Two independent pipelines that share one deterministic push primitive:

| Package                                                                                 | Role                                                                                     |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| [`pkg/ocipush`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/ocipush)               | Deterministic OCI artifact push via ORAS (not `helm push`) — GHCR + ECR auth              |
| [`pkg/helmctl`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/helmctl)               | Helm chart packaging: version + values injection, release manifests, deterministic push   |
| [`pkg/goreleaserdist`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/goreleaserdist) | Parse a GoReleaser `dist/` (images + index digests + version) into a release manifest     |
| [`pkg/crdctl`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/crdctl)                 | CRD fetch from GitHub + chart generation + publish pipeline                               |

Determinism is the point of the push primitive, not an add-on:

- Tar entries sorted alphabetically, timestamps normalized to epoch 0, UID/GID zeroed
- No `org.opencontainers.image.created` annotation
- Same content → same manifest digest, safe for an immutable tag

No particular is ever baked in: `--registry`, `--repository` and `--profile`
are required, caller-supplied flags on every push (`helmctl push`,
`crdctl publish`) — the binary itself carries no default account, region or
profile. See [below](#the-rule-that-makes-this-repository-public).

## Install and a worked example

```bash
# Via go run (no install needed), pinned to a release:
go run github.com/truvity/ocictl/cmd/helmctl@v0.6.1 --help
go run github.com/truvity/ocictl/cmd/crdctl@v0.6.1 --help
```

Consuming this from another repository — pinning the version in `go.mod`,
a `bin/helmctl` wrapper, and devbox PATH so employees and CI run the same
binary — is **[docs/consuming.md](docs/consuming.md)**.

### helmctl + GoReleaser: deterministic immutable charts

Turn any GoReleaser build (ko and/or `dockers_v2` images) into a chart whose
`values.yaml` carries digest-pinned image references and whose version comes
from the release — see **[docs/goreleaser.md](docs/goreleaser.md)** for the
full guide (manifest schema, chart-side conventions, multi-arch digests):

```bash
goreleaser release --clean
helmctl goreleaser-manifest --goreleaser-dist dist/myproject -o dist/myproject/chart-manifest.yaml
helmctl package --chart charts/myproject --manifest dist/myproject/chart-manifest.yaml \
  --require-image-digests --output dist/myproject/charts/
```

### helmctl

```bash
# Package a chart (source directory is never modified)
helmctl package --chart charts/cilium-crds --version 1.19.5 --output dist/

# Push to GHCR
helmctl push --tgz dist/cilium-crds-1.19.5.tgz \
  --registry ghcr.io --repository truvity/charts/cilium-crds \
  --version 1.19.5 --name cilium-crds

# Push to ECR (private)
helmctl push --tgz dist/my-chart-1.0.0.tgz \
  --registry <account-id>.dkr.ecr.eu-example-1.amazonaws.com \
  --repository myteam/charts/my-chart \
  --profile <profile> \
  --version 1.0.0 --name my-chart
```

### crdctl

```bash
# Fetch CRDs from GitHub and generate chart templates/ (no push)
crdctl build --config charts/cilium-crds/crdctl.yaml

# Full pipeline: fetch + package + push to GHCR
crdctl publish --config charts/cilium-crds/crdctl.yaml \
  --registry ghcr.io --repository truvity/charts/cilium-crds
```

## Consumers

Who uses ocictl, and through which surface:

| Consumer            | Surface                     |
| -------------------- | ---------------------------- |
| truvity/gemaal       | `go tool helmctl`            |
| opwerm/nexus          | `cilium-crds` chart          |
| developer machines   | `go run …@vX` (this README)  |

## Neighbours

- **ocictl ↔ gemaal** — gemaal's pipeline shells out to `helmctl`.

## Documentation

- [AGENTS.md](AGENTS.md) — the exhaustive command surface, for agents and humans alike
- [docs/consuming.md](docs/consuming.md) — pinning and wiring ocictl's
  tools from another repository
- [docs/goreleaser.md](docs/goreleaser.md) — the helmctl + GoReleaser pipeline in full
- [CHANGELOG.md](CHANGELOG.md) — release notes
- The component contract every chart here is held to lives in
  [truvity/policy](https://github.com/truvity/policy/blob/master/docs/contracts/component.md).

## The rule that makes this repository public

Nothing here defaults to a real account, region, profile or internal
hostname — every particular is either a caller input (a CLI flag, checked
`Required: true` on every push command) or lives outside this repo entirely
(an org variable, a workflow secret). That is what makes it safe to publish:
the mechanism is public, the estate's specifics never enter it.
[`hack/leak-canary.sh`](hack/leak-canary.sh) enforces this mechanically on
every `just check` — it scans tracked files for account IDs, ARNs, internal
hostnames and tokens, because a public repository's history cannot be
unpublished once a particular has landed in it.

## Status

- Latest release: **v0.6.1** (2026-09-24) — see the
  [releases page](https://github.com/truvity/ocictl/releases) for every tag.
- Published to GHCR at `ghcr.io/truvity/charts/{name}:{version}`:

| Chart                | Upstream                                                                                       |
| --------------------- | -------------------------------------------------------------------------------------------- |
| cilium-crds           | [cilium/cilium](https://github.com/cilium/cilium)                                             |
| barman-cloud-crds     | [cloudnative-pg/plugin-barman-cloud](https://github.com/cloudnative-pg/plugin-barman-cloud)   |
| volume-snapshot-crds  | [kubernetes-csi/external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter) |

Versions are pinned in each chart's own `crdctl.yaml`.

## Development

```bash
# Enter dev environment
devbox shell

# Run all checks (build + test + lint + chart-lint + vuln + leak-canary)
just check

# Build all CRD charts locally
just crd-build-all

# Publish all CRD charts to GHCR
just crd-publish-all
```

## Releasing

- A push of a `v*` tag triggers
  [`.github/workflows/release.yaml`](.github/workflows/release.yaml), which
  calls the shared `release-public.yaml` (GoReleaser build + publish).
- [`.github/workflows/auto-release.yaml`](.github/workflows/auto-release.yaml)
  cuts the next patch tag automatically once automerged dependency bumps have
  moved `master` past the latest release — weekly, or immediately for a push
  whose merged PR carries the `security` label. Off by default, gated on
  `vars.AUTO_RELEASE` and `vars.ACCESS_ROSTER_ISSUER`.
- [`.github/workflows/publish-charts.yaml`](.github/workflows/publish-charts.yaml)
  republishes a CRD chart to GHCR whenever its `crdctl.yaml` changes on
  `master`.

## Licence

MIT
