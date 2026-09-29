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

# Scan tracked files for leaked particulars (account ids, ARNs, internal
# hostnames, tokens) — this repo is public and its history cannot be
# unpublished.
leak-canary:
    hack/leak-canary.sh

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf dist/ coverage.out .cache/ charts/*/templates/

# Build every CRD chart and validate the schema contract: the charts
# take no values, and values.schema.json makes configuring them a loud
# error instead of a silent no-op. Also proves the golden render under
# tests/golden/<chart>/ still matches (component contract C3) and that
# every fixture under tests/invalid/<chart>/ is refused.
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
        golden="tests/golden/$chart/default.yaml"
        if [ -f "$golden" ]; then
            if ! diff -u "$golden" <(helm template "$chart" "$dir") >/dev/null; then
                echo "ERROR: $chart's render no longer matches $golden — 'just golden' to update, then review the diff" >&2
                exit 1
            fi
        fi
        for fixture in tests/invalid/"$chart"/*; do
            [ -f "$fixture" ] || continue
            if helm template "$chart" "$dir" --values "$fixture" >/dev/null 2>&1; then
                echo "ERROR: $chart accepted $fixture — values.schema.json not enforced" >&2
                exit 1
            fi
        done
    done
    echo "chart-lint: all CRD charts render, match their golden and reject unknown values"

# Regenerate the golden renders under tests/golden/ — review the diff.
golden: build crd-build-all
    #!/usr/bin/env bash
    set -euo pipefail
    for dir in charts/*/; do
        chart="$(basename "$dir")"
        [ -d "tests/golden/$chart" ] || continue
        helm template "$chart" "$dir" >"tests/golden/$chart/default.yaml"
    done
    echo "golden: regenerated"

# Run all checks (build + test + lint + chart-lint + vuln + leak-canary)
check: build test lint chart-lint vuln leak-canary

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
