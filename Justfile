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

# The golden digests must not depend on the Go toolchain: run them under the
# oldest toolchain go.mod allows (its `go` line) and under the one it pins (its
# `toolchain` line). The layer compressor is pinned in go.mod, not taken from
# the standard library, so both must give the same digests.
golden:
    #!/usr/bin/env bash
    set -euo pipefail
    low="go$(awk '$1=="go"{print $2}' go.mod)"
    high="$(awk '$1=="toolchain"{print $2}' go.mod)"
    for tc in "$low" "$high"; do
        echo "golden digests under $tc"
        out="$(GOTOOLCHAIN="$tc" go test -count=1 -v -run 'Golden|GzipIsPinned' ./pkg/static ./pkg/layer ./pkg/helmctl)" || { echo "$out"; exit 1; }
        n="$(grep -c '^--- PASS' <<<"$out" || true)"
        echo "$n golden tests passed under $(GOTOOLCHAIN="$tc" go version)"
        # A guard must refuse an empty sweep.
        [ "$n" -ge 4 ] || { echo "expected at least 4 golden tests, ran $n"; exit 1; }
    done

# Run linters. `config verify` first: `run` accepts unknown top-level keys
# silently, so a settings block in the wrong place is otherwise invisible.
lint:
    golangci-lint config verify
    GOTOOLCHAIN=local golangci-lint run ./...

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
    rm -rf dist/ coverage.out .cache/

# Run all checks (build + test + lint + leak-canary)
check: build test lint leak-canary

# Build a snapshot release locally (no push, no tag)
snapshot:
    KO_DOCKER_REPO=ghcr.io/truvity/ocictl/smctl goreleaser release --snapshot --clean
