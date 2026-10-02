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
    rm -rf dist/ coverage.out .cache/

# Run all checks (build + test + lint + leak-canary)
check: build test lint leak-canary

# Build a snapshot release locally (no push, no tag)
snapshot:
    KO_DOCKER_REPO=ghcr.io/truvity/ocictl/smctl goreleaser release --snapshot --clean
