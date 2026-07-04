# ocictl

Deterministic OCI chart packaging and CRD repack tooling.

## Tools

| Binary | Purpose |
|--------|---------|
| **helmctl** | Deterministic Helm chart packaging + OCI push (GHCR + ECR) |
| **crdctl** | Fetch upstream CRDs → generate chart → package → push |

## Install

```bash
# Via go run (no install needed):
go run github.com/truvity/ocictl/cmd/helmctl@latest --help
go run github.com/truvity/ocictl/cmd/crdctl@latest --help
```

## Usage

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

## CRD Charts

| Chart | Upstream |
|-------|----------|
| cilium-crds | cilium/cilium |
| barman-cloud-crds | cloudnative-pg/plugin-barman-cloud |
| volume-snapshot-crds | kubernetes-csi/external-snapshotter |

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

Apache-2.0
