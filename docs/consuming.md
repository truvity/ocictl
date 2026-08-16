# Consuming ocictl from another repository

`helmctl` and `crdctl` are meant to be run *by* other repos, not installed by
hand on every laptop. This is the wiring that gives an employee a plain
`helmctl` on their PATH and gives CI the identical binary, with the version
pinned in one place and no second copy to drift.

Three pieces, none of them optional.

## 1. Pin the version in `go.mod`

Go's `tool` directive pins the tool the same way it pins a library, so
Renovate updates it and a lockfile records the exact build:

```go
tool (
	github.com/truvity/ocictl/cmd/helmctl
)
```

This is the **only** place the version lives. Nothing below repeats it.

## 2. A `bin/<tool>` wrapper

```bash
#!/usr/bin/env bash
# helmctl wrapper — deterministic Helm chart packaging/push (truvity/ocictl).
# Version-pinned by the go.mod `tool github.com/truvity/ocictl/cmd/helmctl`.

set -euo pipefail

exec go run github.com/truvity/ocictl/cmd/helmctl "$@"
```

`go run` resolves through `go.mod`, so the wrapper carries no version and
cannot disagree with the pin. Make it executable and commit it.

## 3. Put `bin/` on PATH via devbox

```json
{
  "env": {
    "PATH": "$DEVBOX_PROJECT_ROOT/bin:$PATH"
  }
}
```

Now `helmctl package …` works in a shell, in a Justfile, in a moon task, and
in CI — one spelling everywhere.

## Why not add it to `devbox.json` packages

Because nixpkgs would then own the version, and `go.mod` would own it too.
Two owners is how you get a tool that is 1.4.0 on a laptop and 1.6.0 in CI
with nothing reporting the difference — the failure this pattern exists to
prevent. Keep Go tools pinned by Go, and let devbox provide only what Go
cannot: system packages, language runtimes, browsers.

The same reasoning applies in reverse. If a tool ships browser binaries or
other non-Go payload (playwright is the canonical case), nix must own it and
the npm/Go side must follow — bumping one alone breaks the pair.

## Verifying

```bash
helmctl --version          # resolves via bin/ -> go run -> go.mod pin
go list -m github.com/truvity/ocictl
```

If those disagree, something has been installed outside this scheme.
