# ocictl

[![CI](https://github.com/truvity/ocictl/actions/workflows/ci.yaml/badge.svg)](https://github.com/truvity/ocictl/actions/workflows/ci.yaml)
[![Release](https://github.com/truvity/ocictl/actions/workflows/release.yaml/badge.svg)](https://github.com/truvity/ocictl/actions/workflows/release.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/truvity/ocictl.svg)](https://pkg.go.dev/github.com/truvity/ocictl)
[![Go Report Card](https://goreportcard.com/badge/github.com/truvity/ocictl)](https://goreportcard.com/report/github.com/truvity/ocictl)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Deterministic OCI chart packaging and CRD repack tooling.

## Tools

| Binary       | Purpose                                                     |
| ------------ | ----------------------------------------------------------- |
| **helmctl**  | Deterministic Helm chart packaging + OCI push (GHCR + ECR)  |
| **crdctl**   | Fetch upstream CRDs → generate chart → package → push       |
| **fleetctl** | Identity- and namespace-aware Kubernetes primitives         |

## Packages

| Package                                                                                   | Description                                                       |
| ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| [`pkg/ocipush`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/ocipush)                 | Deterministic OCI artifact push via ORAS (GHCR + ECR auth)        |
| [`pkg/helmctl`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/helmctl)                 | Helm chart packaging: version + values injection, release manifests, deterministic push |
| [`pkg/goreleaserdist`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/goreleaserdist)   | Parse a GoReleaser `dist/` (images + index digests + version) into a release manifest |
| [`pkg/crdctl`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/crdctl)                   | CRD fetch from GitHub + chart generation + publish pipeline       |
| [`pkg/fleettest`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/fleettest)             | Tenant resolution + the `TestMain` integration-test harness       |
| [`pkg/kubewho`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/kubewho)                 | `kubectl auth whoami` wrapper: caller identity + prefixed-group extraction |
| [`pkg/fleetcfg`](https://pkg.go.dev/github.com/truvity/ocictl/pkg/fleetcfg)               | Parse the committed `fleet.yaml` (cluster values, build hooks)    |

## Install

```bash
# Via go run (no install needed):
go run github.com/truvity/ocictl/cmd/helmctl@latest --help
go run github.com/truvity/ocictl/cmd/crdctl@latest --help
go run github.com/truvity/ocictl/cmd/fleetctl@latest --help
```

## Usage

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
  --registry 721506300184.dkr.ecr.eu-central-1.amazonaws.com \
  --repository nexus/charts/my-chart \
  --profile stable@admin \
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

### fleetctl

Identity- and namespace-aware Kubernetes primitives, designed to be called
from build-system tasks rather than to orchestrate them. Every cluster
interaction shells out to `kubectl` — no kubeconfig parsing, no client-go.

```bash
# Deploy into the resolved tenant, with the cluster's fleet.yaml values
# merged into the release as .Values.fleet
fleetctl deploy -c devel@oidc --app url-shortener --chart ./charts/url-shortener

# Run an integration test inside the resolved tenant (FLEET_* exported)
fleetctl test -c devel@oidc --app url-shortener -- go test ./svc/... -tags integration

# Debug: the cluster's view of the caller + the tenant that resolves from it
fleetctl whoami -c devel@oidc
```

A **tenant** is the `(namespace, release)` pair one install is named by,
and all three commands resolve it through the same path, so they cannot
disagree about which install they mean. Tenancy is **standing by
default**: the namespace already exists — an employee's `emp-{slug}`, or
CI's static namespace — and the tool creates and deletes nothing.
`fleetctl test --ephemeral` opts into the other model, creating
`{prefix}{git-sha}` and tearing it down afterwards; teardown waits for the
namespace to actually be gone and only ever deletes one this run created
itself.

The full design — resolution ladders, the committed `fleet.yaml`, build
hooks, the teardown contract — lives in
**[docs/rfc-fleet.md](docs/rfc-fleet.md)**.

## CRD Charts

| Chart                | Upstream                                                                                      |
| -------------------- | --------------------------------------------------------------------------------------------- |
| cilium-crds          | [cilium/cilium](https://github.com/cilium/cilium)                                             |
| barman-cloud-crds    | [cloudnative-pg/plugin-barman-cloud](https://github.com/cloudnative-pg/plugin-barman-cloud)   |
| volume-snapshot-crds | [kubernetes-csi/external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter) |

Versions are pinned in each chart's `crdctl.yaml`. Published to `ghcr.io/truvity/charts/{name}:{version}`.

## Development

```bash
# Enter dev environment
devbox shell

# Run all checks
just check

# Build all CRD charts locally
just crd-build-all

# Publish all CRD charts to GHCR
just crd-publish-all
```

## Determinism

Both tools guarantee reproducible OCI manifests:

- Tar entries sorted alphabetically
- Timestamps normalized to epoch 0
- UID/GID zeroed
- `org.opencontainers.image.created` annotation stripped
- Same content → same manifest digest (safe for immutable tags)

## License

MIT
