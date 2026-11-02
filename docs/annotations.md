# Manifest annotations

`chart push`, `crd publish`, `sourcemaps push`, `static push` and `digest` take
the same annotation flags. **Nothing is defaulted**: with no flag, the manifest
carries no annotation beyond the ones the command always wrote (a chart's
`title`, `version` and `description`; a source-map artifact's `version`).
`org.opencontainers.image.created` is separate: only from `SOURCE_DATE_EPOCH`
or `--created` (see [static.md](static.md#creation-time)).

| Flag | Annotation (`org.opencontainers.image.`…) |
| --- | --- |
| `--source <url>` | `source` |
| `--revision <rev>` | `revision` |
| `--licenses <spdx>` | `licenses` |
| `--url <url>` | `url` |
| `--documentation <url>` | `documentation` |
| `--vendor <name>` | `vendor` |
| `--annotation k=v` (repeatable) | any key |
| `--annotations-from-git` | `source` from the `origin` remote, `revision` from `HEAD` |

Precedence, last wins: `--annotations-from-git`, then the named flags, then
`--annotation`. `--annotations-from-git` reads the repository in the working
directory and fails when there is no `origin` or no commit.

- The `origin` URL is turned into an `https://` URL without user info and
  without a trailing `.git`: `git@host:org/repo.git` becomes
  `https://host/org/repo`.
- A value that is a URL carrying user info (`https://user:token@host/…`) is
  **refused**, whichever flag it came from, and the error does not repeat it.
- Annotations are part of the manifest and so of its digest: the same flags give
  the same digest, other flags give another. `digest` and `static push --dry-run`
  take the flags, so they print the digest a push with them would give.

## GHCR package link

GitHub links a container package to its repository from
`org.opencontainers.image.source` on the manifest. Pushing a chart with
`--source https://github.com/<org>/<repo>` (or `--annotations-from-git` in a
checkout whose `origin` is that repository) sets it. This is GitHub's documented
behaviour; it has not been exercised against ghcr.io from this repository's
tests (they use a fake registry).
