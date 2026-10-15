# AGENTS.md

Instructions for AI coding agents working with `ocictl`, in this repo or in a
repo that consumes it. Human-readable too — nothing here is agent-only.

## What this repo ships

One binary, `ocictl`, with subcommands. `ocictl` is also the repository name.

| Command | Does |
| --- | --- |
| `ocictl chart` | deterministic Helm chart packaging + OCI push (was `helmctl`) |
| `ocictl crd` | fetch upstream CRDs → generate chart → package → push (was `crdctl`) |
| `ocictl sourcemaps` | source maps as OCI artifacts: `push` after a release, `serve` to Grafana Alloy (was `smctl`) |
| `ocictl static push` | a directory as a deterministic OCI artifact or image, idempotent ([docs/static.md](docs/static.md)) |
| `ocictl digest <dir>` | the layer and manifest digests of a directory, no push |

`helmctl`, `crdctl` and `smctl` are **deprecated** aliases: separate
release assets and `cmd/` packages with identical flags and output, kept for two
releases. The `ocictl` binary also behaves as them when invoked under those
names (argv[0]). Prefer the `ocictl …` spelling in anything you write.

Writing `ocictl helmctl …` fails: the old names are not subcommands. The chart
tool is `ocictl chart …`.

## The command surface, exhaustively

This is the whole of `ocictl`. There are no other subcommands (`release run` and
`release plan` are planned, not present):

```
ocictl chart goreleaser-manifest --goreleaser-dist <dir> [--images a,b] [-o <file>]
ocictl chart package  --chart <dir> (--version <ver> | --manifest <file>)
                      [--app-version <ver>] [--require-image-digests] [--output <dir>]
ocictl chart push     --tgz <file> --registry <url> --repository <path>
                      --name <name> --version <ver> [--profile <aws>]
ocictl crd build      --config <crdctl.yaml>
ocictl crd publish    --config <crdctl.yaml> --registry <url> --repository <path> [--profile <aws>]
ocictl sourcemaps push  --maps <dir> [--include <pattern>...] [--dry-run] [--profile <aws>]
                        ( --goreleaser-dist <dir> --image <name> [--app <name>] [--repository-template <tpl>]
                        | --repository <host/path> )  [--version <ver>]
ocictl sourcemaps serve --config <file>
ocictl static push    --dir <dir> --repository <host/path> --tag <ver>
                      [--shape artifact|image] [--architecture <a>] [--os <os>]
                      [--annotation k=v]... [--profile <aws>] [--dry-run]
ocictl digest <dir>   [--shape artifact|image] [--architecture <a>] [--os <os>] [--annotation k=v]...
```

Note `chart push` takes **no positional project argument**.
`ocictl chart push myproject` is not a shorter form of anything; it fails on
missing required flags. `digest` is the one command with a positional argument
(the directory).

The deprecated names expose the same subcommands at the top level
(`helmctl package …`, `smctl push …`; `docs/sourcemaps.md` and
`docs/goreleaser.md` still use them).

Source of truth: the command definitions in
[`pkg/commands/`](pkg/commands) (`chartcmd`, `crdcmd`, `smcmd`, `staticcmd`),
assembled in [`pkg/commands/app/app.go`](pkg/commands/app/app.go). If this file
and those disagree, they are right and this one is a bug — fix it.

## If you are about to write a command into documentation

Read the command definitions in `pkg/commands/` first. Not the README, not a grep of how often a
string appears in other docs, and not another repo's prose.

This warning exists because of a real incident (2026-08-16). An agent cleaning
up references to a retired tool was told "charts are ocictl's now", and turned
that repo-level fact into `ocictl helmctl push url-shortener` — wrong binary,
wrong argument shape — and wrote it into several files as fact. It then
"verified" the fix by grepping documentation, which only measured how often the
mistake had been repeated. The command surface above was three minutes of
reading `main.go` away.

A plausible command that fails is worse than an admission that you do not know
the command. If you cannot verify an invocation, write that it is unverified.

## Using these tools from another repo

Do not `go install` and do not add them to `devbox.json` packages. Pin in
`go.mod` with a `tool` directive, add a `bin/<tool>` wrapper that `exec`s
`go run`, and put `bin/` on PATH via devbox. Full rationale:
[docs/consuming.md](docs/consuming.md).

## Working in this repo

```bash
devbox shell     # dev environment
just check       # build + test + lint + govulncheck — must pass before a PR
```

This repository holds no charts. The CRD charts `crdctl` used to publish live
in truvity/cnpg and truvity/k8s, which run `crdctl` from a pinned release.
