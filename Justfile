# Development commands for ocictl

# Disable go.work (parent workspace interferes with standalone module builds)
export GOWORK := "off"

# Format all Go files
fmt:
    golangci-lint fmt ./...

# Build (compile check only — bin/ has shell wrappers, not compiled output)
build: fmt
    go build ./...

# Run unit tests
test:
    go test ./... -coverprofile=coverage.out

# Run linters. `config verify` first: `run` accepts unknown top-level keys
# silently, so a settings block in the wrong place is otherwise invisible.
lint:
    golangci-lint config verify
    golangci-lint run ./...

# Run Go vulnerability check
vuln:
    govulncheck ./...

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf dist/ coverage.out .cache/ charts/*/templates/

# Build every CRD chart and validate the schema contract: the charts
# take no values, and values.schema.json makes configuring them a loud
# error instead of a silent no-op.
chart-lint: build crd-build-all
    #!/usr/bin/env bash
    set -euo pipefail
    for dir in charts/*/; do
        chart="$(basename "$dir")"
        helm lint "$dir" >/dev/null
        helm template "$chart" "$dir" >/dev/null
        if helm template "$chart" "$dir" --set anything=1 >/dev/null 2>&1; then
            echo "ERROR: $chart accepted an unknown value — values.schema.json not enforced"
            exit 1
        fi
    done
    echo "chart-lint: all CRD charts render and reject unknown values"

# Run all checks (build + test + lint + chart-lint + vuln)
check: build test lint chart-lint vuln

# Build a snapshot release locally (no push, no tag)
snapshot:
    goreleaser release --snapshot --clean

# --- CRD chart operations ---

# Build a single CRD chart (fetch upstream CRDs, generate templates/)
crd-build chart:
    bin/crdctl build --config charts/{{chart}}/crdctl.yaml

# Build all CRD charts
crd-build-all:
    #!/usr/bin/env bash
    set -euo pipefail
    for dir in charts/*/; do
        bin/crdctl build --config "$dir/crdctl.yaml"
    done

# Publish a single CRD chart to GHCR (fetch + package + push)
crd-publish chart:
    bin/crdctl publish --config charts/{{chart}}/crdctl.yaml --registry ghcr.io --repository truvity/charts/{{chart}}

# Publish all CRD charts to GHCR
crd-publish-all:
    #!/usr/bin/env bash
    set -euo pipefail
    for dir in charts/*/; do
        chart="$(basename "$dir")"
        bin/crdctl publish --config "$dir/crdctl.yaml" --registry ghcr.io --repository "truvity/charts/$chart"
    done
