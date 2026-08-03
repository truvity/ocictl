# RFC: `fleet` — config-driven build, publish, deploy and integration testing

Status: **draft** (2026-08-03). Pilot target: one downstream project before
the schema freezes.

## Problem

A repository that ships services onto a Kubernetes fleet needs four
capabilities: build docker images, package and push helm charts, deploy a
release to a named cluster with that cluster's values, and run integration
tests against a real cluster in an ephemeral namespace. Today each
organization hand-rolls this, and the tooling usually ends up importing the
infrastructure repository's config packages — a private-module dependency
that couples every product engineer's build to infrastructure-repo access.

The fix is to move the **schema and the engine** into a public tool and
leave the **data** as a small committed file in each consumer repository,
refreshed out-of-band by whoever owns the infrastructure source of truth —
the same trust model as a committed `aws.ini` or `kubeconfig`.

## The config: `fleet.yaml`

One file, committed in the consumer repository. Everything the engine
needs; nothing about how the values were derived.

```yaml
# fleet.yaml — committed artifact; regenerate from your infra source of
# truth, do not edit by hand unless you are that source of truth.
registries:
  images: 123456789.dkr.ecr.eu-central-1.amazonaws.com   # docker push/pull
  charts: oci://123456789.dkr.ecr.eu-central-1.amazonaws.com/charts

clusters:
  devel:
    kubecontext: devel@oidc      # resolved against the ambient kubeconfig
    values:                      # OPAQUE map, merged into chart values as .fleet.*
      accountID: "..."
      region: eu-central-1
      authMode: pod-identity
      permissionsBoundary: arn:aws:iam::...:policy/...

projects:
  url-shortener:
    images:
      - name: url-shortener
        dockerfile: url-shortener/Dockerfile
        context: .
    charts:
      - path: url-shortener/chart
    test:
      command: ["moon", "run", "url-shortener:test/integration"]
      cluster: devel
      namespacePrefix: it-
```

Design rules:

- **`values` is opaque.** The engine never interprets it; it is handed to
  charts verbatim under `.Values.fleet`. Organization semantics live in the
  charts, not in this tool.
- **No credentials.** Registry auth, AWS profiles and kubeconfig are the
  ambient environment's job. The engine never generates or manages them.
- **Small on purpose.** Anything expressible as a chart value or a test
  command stays out of the schema.

## Commands

A third standalone binary, following this repository's one-binary-per-tool
convention (`cmd/crdctl`, `cmd/helmctl` → `cmd/fleetctl`); consumers run it
the same way (`go run github.com/truvity/ocictl/cmd/fleetctl@vX`).

```
fleetctl build  [-p project]              # images (buildx, git-sha tags) + chart packages
fleetctl push   [-p project]              # to registries.images / registries.charts
fleetctl deploy [-p project] -c cluster   # helm upgrade --install, cluster values merged
fleetctl test   [-p project]              # ephemeral namespace → deploy → run test.command → teardown
```

`fleetctl test` is the flagship: bring a kubeconfig and a chart, get
integration tests against a real cluster — create `{namespacePrefix}{git-sha}`,
deploy the project's charts, wait ready, run the command with
`FLEET_NAMESPACE`/`FLEET_CLUSTER` exported, tear down on exit (keep on
`--keep` for debugging).

## Identity: who is deploying?

Personal-namespace workflows (a developer deploying to their own
namespace) need the caller's **slug** — a short identifier the
organization's identity platform embeds in the OIDC token as a prefixed
entry in the groups claim (the prefix is configuration, e.g. `emp:`).

Resolution order:

1. `--slug` flag
2. `FLEET_SLUG` env var (the CI/bot path — same family as
   `FLEET_NAMESPACE` / `FLEET_CLUSTER`)
3. `kubectl auth whoami -o json` against the target cluster's
   kubecontext — the cluster's own view of the caller; the exec
   credential plugin (browser SSO, cache, refresh) is kubectl's job,
   never this tool's. Exactly-one semantics on the prefixed group:
   zero or multiple matches are hard errors, never guesses.

Resolution is **lazy**: only commands that actually target a personal
namespace resolve a slug. Machine identities (CI runners) never carry
the prefixed group by construction, so a robot in a human code path
fails crisply instead of impersonating anyone.

## Stability contract

The schema stabilizes **once**: v0 during the pilot (breaking changes
allowed), frozen at v1 immediately after — from then on additive-only
fields and bugfix releases. Consumers pin ocictl versions; the engine
carries no organization-specific behavior, so there is no reason for churn.

## Out of scope — explicitly

- Generating or simplifying `aws.ini` / `kubeconfig` (decided 2026-08-03:
  ambient environment, untouched).
- Secrets of any kind.
- CD/promotion (a different tool's job).

## Rollout

1. Schema v0 + `fleetctl build|push|test` extracted from the incumbent
   private implementation, generalized (the org-specific conveniences stay
   behind in the consumer repo).
2. **Pilot on exactly one downstream project** (smallest real service).
   Friction found there changes the schema freely.
3. Freeze v1. Further downstream projects adopt at their own pace; the
   incumbent tooling keeps serving them until they do.
