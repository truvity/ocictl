# RFC: `fleetctl` — identity- and namespace-aware Kubernetes primitives

Status: **draft v3** (2026-08-04, amended after the pilot: tenancy is
standing, not ephemeral). Freeze at v1 once the pilot's harness
re-alignment lands.

## Problem

A repository that ships services onto a Kubernetes fleet needs two
capabilities nothing currently owns:

1. **Deploy into the right namespace with the right cluster facts** —
   "right namespace" may depend on *who is running the command* (personal
   developer namespaces), and cluster facts (account IDs, IAM boundaries,
   auth mode) live in the infrastructure repository, which product
   engineers should not need access to.
2. **Integration-test against a real cluster in the same tenant an
   engineer and CI both use** — resolve the tenant, run, and leave the
   cluster exactly as found. Creating a namespace per run is one way to
   get a tenant; it is no longer the way this fleet gets one.

Everything else in the build-and-ship path already has an owner, and this
tool must not compete with any of them.

## Non-goals (each has an owner already)

| Concern | Owner |
|---|---|
| Task orchestration, caching, sequencing | the repo's build system (moon) |
| Building images | goreleaser / docker |
| Packaging & pushing charts | helmctl |
| Cluster registry ("which clusters exist, how to reach them") | kubeconfig |
| Cluster facts ("what is true about this cluster") | `fleet.yaml`, committed (see below) |
| Credentials: registry auth, AWS profiles, kubeconfig generation | ambient environment |
| Namespace lifecycle (who may exist, quotas, netpol) | the infrastructure repository |
| Reaping stale test installs | an external janitor |
| CD / promotion | a different tool |
| Secrets | never |

The result: `fleetctl` is a **lower-level tool designed to be called from
build-system tasks**, not an orchestrator with a project model.

## Design principles

- **kubectl is the abstraction.** Every cluster interaction shells out to
  `kubectl` (`exec.CommandContext`) — auth, exec credential plugins
  (browser SSO, cache, refresh), kubeconfig discovery are kubectl's job.
  This tool never parses kubeconfig and never imports client-go.
- **Every registry has exactly one owner.** No config section here may
  mirror kubeconfig, the infra repo, or the build system's task graph.
- **Org semantics are parameters.** Group prefixes and namespace
  templates are configuration, always overridable; the conventional
  `emp:` / `emp-{slug}` pair ships as a default so the common case needs
  no config, and nothing else about an organization is baked in.
- **Never destroy what you did not create.** The harness owns a lifecycle
  only when a caller explicitly asks for one, and even then only over the
  namespaces it created itself (see Teardown).

## Commands

A standalone binary, following this repository's one-binary-per-tool
convention (`cmd/crdctl`, `cmd/helmctl` → `cmd/fleetctl`); consumers run
it the same way (`go run github.com/truvity/ocictl/cmd/fleetctl@vX`).

```
fleetctl deploy -c <kubecontext> --chart <ref|path> [--build] [release]
    # helm upgrade --install into the resolved tenant, with the
    # cluster's values from fleet.yaml merged as .Values.fleet;
    # --chart takes a LOCAL PATH or an OCI ref — the local-chart dev
    # loop is first-class

fleetctl test [-c <kubecontext>] [--ephemeral [--prefix it-] [--keep]] [--build] -- <command...>
    # resolve the tenant → export FLEET_* → run command
    # --ephemeral opts into namespace lifecycle: create → run → teardown

fleetctl whoami [-c <kubecontext>]
    # debug: the cluster's view of the caller + the resolved tenant
    # (namespace AND release)
```

`--build` runs the repo's configured build hook first (see Build
integration) — the one-command dev loop without absorbing the build.

`-c` takes a **kubecontext name directly** (e.g. `devel@oidc`) — there is
no cluster-name indirection, because kubeconfig is the cluster registry.

## Tenancy: the tenant is `(namespace, release)`

A tenant is one install. Both halves resolve together, through **one
path** (`fleettest.Resolve`) that `deploy`, `test` and `whoami` all call,
so no two commands can disagree about which install they mean.

Two modes:

| Mode | Namespace | Lifecycle |
|---|---|---|
| **standing** (default) | already exists — an employee's `emp-{slug}`, or CI's static `ci-<org>-<repo>` | **none**: nothing is created, nothing is deleted |
| **ephemeral** (`--ephemeral` / `Options.Ephemeral`) | `{prefix}{git-sha}` | create → run → **synchronous** delete, and only of a namespace this run both named and created (`--keep` preserves it) |

Standing is the default because it is the safe one: a harness that never
creates and never deletes cannot destroy anything. Ephemeral remains a
generic library capability for consumers that want a throwaway namespace.
**The fleet operating this tool uses standing tenancy exclusively** —
employees run a standing install in their own namespace, CI runs a
TTL-reaped release in the repo's static namespace; namespaces are
infrastructure rows, and an external janitor owns expiry.

**Namespace ladder:**

1. `--namespace` flag / `Options.Namespace`
2. **`FLEET_NAMESPACE`** env var — the CI/robot path: robots name a
   namespace outright and no identity machinery runs. Deliberately
   bidirectional: read as an override when the caller set it, and always
   re-exported with the RESOLVED value into child processes — the same
   pattern as `KUBECONFIG`.
3. Standing: derived personal namespace — `kubectl auth whoami -o json`
   against the target kubecontext returns the cluster's own view of the
   caller; the organization's identity platform embeds a short identifier
   as a prefixed entry in the groups claim (prefix is configuration,
   default `emp:`), and the personal-namespace template renders it
   (default `emp-{slug}`). **Exactly-one semantics** on the prefixed
   group: zero or multiple matches are hard errors, never guesses.
   Ephemeral: `{prefix}{git-sha}` instead — no identity involved.

**Release ladder:**

1. `--release` flag / `Options.Release` (`deploy`'s positional argument
   wins over both)
2. **`FLEET_RELEASE`** env var — bidirectional, like `FLEET_NAMESPACE`
3. CI, detected by environment rather than by flag: `GITHUB_RUN_NUMBER`
   present → `r{run_number}-a{attempt}` (`GITHUB_RUN_ATTEMPT`, default
   `1`). Run numbers are per-repo-unique and the namespace carries the
   repo, so the pair is unique.
4. `--app` / `Options.App` — the plain app name, i.e. an employee's
   standing install
5. ephemeral only: the ephemeral namespace (one release per namespace)

No rung matching is an **error**, never a guess.

**Bounds are asserted, not discovered in production**: release names are
capped at 53 characters (Helm), namespaces at 63 (RFC1123 label), and
both must be RFC1123 labels. A name that helm or the API server would
reject fails here instead, with the offending name in the message.

Resolution is **lazy**: only commands that need a tenant and were given
none resolve identity. Machine identities (CI runners) never carry the
prefixed group by construction, so a robot falling into the human code
path fails crisply instead of impersonating anyone.

Tuning (applies to `auth whoami` and every other kubectl this tool
spawns, so there is never a which-kubeconfig split-brain):

- `FLEET_KUBECONTEXT` — default for `-c`
- `FLEET_KUBECONFIG` — overrides the ambient `KUBECONFIG`

## The config file: a small, committed `fleet.yaml`

One small file, committed in the consumer repository, refreshed
out-of-band by whoever owns the infrastructure source of truth — the
`aws.ini` trust model. (An in-cluster shared ConfigMap was considered
and rejected 2026-08-03: a repo-local file is simpler to reason about,
works offline, and the staleness class is already accepted for
`aws.ini`/`kubeconfig`.)

```yaml
# fleet.yaml — committed; regenerate from your infra source of truth.
personalNamespace: "emp-{slug}"
groupPrefix: "emp:"

clusters:
  devel@oidc:              # keys ARE kubecontext names — no indirection,
    values:                # kubeconfig remains the cluster registry
      accountID: "..."
      region: eu-central-1
      authMode: pod-identity
      permissionsBoundary: arn:aws:iam::...:policy/...

hooks:                     # repo-global fallback
  build: ["goreleaser", "release", "--snapshot", "--clean"]

projects:                  # optional per-project override — HOOKS ONLY
  url-shortener:
    hooks:
      build: ["moon", "run", "url-shortener:build"]
```

`values` is **opaque**: merged into the release as `.Values.fleet`,
verbatim; organization semantics live in the charts. The file holds
nothing that mirrors kubeconfig (no endpoints, no auth) and nothing the
build system owns (no task graph).

## Build integration: assured, not absorbed

The full path an engineer or CI needs is covered without this tool
owning any build step:

| Capability | Owner | fleetctl's part |
|---|---|---|
| Build + push docker images | goreleaser / docker | none — or via the build hook |
| Build helm chart (push optional) | helmctl (this repository) | none |
| Roll out a LOCAL chart | helm | `fleetctl deploy --chart ./path` |

**The build hook** (`hooks.build` in `fleet.yaml`, an opaque argv) wires
them together for the one-command loop: `fleetctl deploy --build` /
`fleetctl test --build` executes the hook first, with the resolved
`FLEET_*` env exported to it. Tag coordination is by convention — both
the hook's build and the chart's values derive tags from the git sha —
not by protocol. Without `--build` (the CI/moon path), the build system
sequences goreleaser/helmctl itself and fleetctl only deploys/tests.

A monorepo builds each project its own way, so the hook resolves
**per project**: `projects.<name>.hooks.build` when set, the global
`hooks.build` otherwise. `<name>` is `--project`, defaulting to `--app`.
This is the only per-project key — there is still no project model here
(see Non-goals).

## Test harness: one core, two entry points

**CLI** (any language, natural in build-system tasks):

```bash
fleetctl test -c devel@oidc --app url-shortener -- go test ./svc/... -tags integration
```

**Go library** (`pkg/fleettest`, the TestMain path):

```go
func TestMain(m *testing.M) {
    os.Exit(fleettest.Run(m, fleettest.Options{
        Kubecontext: "devel@oidc",
        App:         "url-shortener",
    }))
}
```

Both share one implementation: resolve the tenant, set/export
`FLEET_NAMESPACE` + `FLEET_RELEASE` + `FLEET_KUBECONTEXT`, run. Standing
mode stops there — **no teardown, because there was no setup**. Ephemeral
mode adds the lifecycle around it, and creation is **idempotent**: a
namespace left behind by a previous run (a crash, `--keep`, a deliberate
re-run) is reused with a log line, never a hard failure.

### Teardown

Two rules, both learned from the pilot's consumer.

**Teardown deletes only what this run created, under a name this harness
chose.** "Never destroy what you did not create" is a design principle
above, and reuse is the case that quietly broke it: the idempotent-create
path adopts a pre-existing namespace, and deleting it on the way out
destroys somebody else's tenant. A namespace named by the caller
(`--namespace` / `Options.Namespace` / `FLEET_NAMESPACE`) is likewise left
alone even when this run happened to create it — a pinned name belongs to
whoever pinned it. Both cases log the reason they are being kept.
Reaping what is left behind is the external janitor's job (see
Non-goals), not this harness's.

**Teardown is synchronous, on its own budget.** `kubectl delete namespace`
returns as soon as the API accepts it, but the namespace then sits in
`Terminating` until every finalizer on its content has run — CNPG
clusters, ACK resources and NATS accounts take minutes. Returning early
makes two failures indistinguishable from success: the caller cannot tell
teardown finished, and the next run of the same commit meets a namespace
that is neither present nor absent, so it either mistakes it for reusable
or fails its own create. So the delete waits, passes kubectl an explicit
`--timeout` (a stuck namespace is reported as "timed out waiting for the
condition", not killed by a context deadline), and `--ignore-not-found`
keeps a namespace the janitor already reaped from counting as a failure.

That wait is a different order of magnitude from the create/probe path, so
it gets a **separate** budget — `Options.TeardownTimeout`, default 10m,
against `Options.Timeout`'s 60s for the API round-trips. One number could
not serve both.

Re-runs against a standing install are the normal case, so tests must
tolerate pre-existing data: tag what you create with an execution id and
assert only on that. Global-count assertions ("expect 0 records") do not
survive a standing tenant — that is a property of the tests, which this
tool cannot enforce and does not try to.

The library adds in-process accessors (`fleettest.Namespace()`,
`fleettest.Release()`) so tests avoid env-var spelunking, and
`fleettest.Resolve` for callers that want the tenant and nothing else.
The core shells out to kubectl for namespace lifecycle — same
zero-dependency rule as everything else.

## Libraries

- `pkg/kubewho` — `kubectl auth whoami` wrapper: caller identity +
  prefixed-group extraction (exactly-one semantics).
- `pkg/fleettest` — tenant resolution (`Resolve`, `Tenant`) plus the
  harness above; `Options.WhoAmI` is the seam that keeps resolution
  unit-testable without a cluster.

Both stdlib-only; kubectl in `PATH` is the single runtime requirement.
(`pkg/fleetcfg` parses `fleet.yaml` and therefore carries the YAML
dependency; nothing in the test path imports it.)

## Configuration precedence

Flags → `FLEET_*` env (`FLEET_NAMESPACE`, `FLEET_RELEASE`, `FLEET_APP`,
`FLEET_PROJECT`, `FLEET_PERSONAL_NAMESPACE`, `FLEET_GROUP_PREFIX`,
`FLEET_KUBECONTEXT`, `FLEET_KUBECONFIG`, `FLEET_CONFIG`) → `fleet.yaml`.
The file is the committed shared default; env/flags are the
per-invocation override, never the other way around. `GITHUB_RUN_NUMBER`
/ `GITHUB_RUN_ATTEMPT` are read, never written: they are facts about the
runner, not configuration.

## Out of scope — explicitly

- Generating or simplifying `aws.ini` / `kubeconfig` (decided
  2026-08-03: ambient environment, untouched).
- Everything in the Non-goals table.

## Open questions for the pilot (v0 may change freely)

1. Does `deploy` stay composite, or decompose into `fleetctl ns` /
   `fleetctl values` primitives piped into plain `helm upgrade` by the
   build system? (Current stance: composite — the values-merge deserves
   one tested implementation.)
2. Build-hook shape: ~~single `hooks.build` vs named hooks per
   command~~ — **answered 2026-08-04**: one hook name, resolved per
   project (`projects.<name>.hooks.build` → `hooks.build`). Per-command
   hooks were not the missing axis; per-project was.

## Stability contract

Stabilize **once**: v0 during the pilot (breaking changes allowed),
frozen at v1 immediately after — additive-only and bugfix releases from
then on. The surface is deliberately tiny (three commands, nine env
vars, two libraries), so there is little to churn.

## Rollout

1. `pkg/kubewho` + `pkg/fleettest` + the three commands.
2. Infrastructure side: stamp `fleet-values` ConfigMaps; add the
   identity-platform claim the prefixed-group extraction reads.
3. **Pilot on exactly one downstream project** (smallest real service),
   wired through its build-system tasks. Friction changes v0 freely.
4. **Realign on the pilot's findings** (2026-08-04): standing tenancy
   becomes the default, the release joins the namespace in the tenant
   identity, hooks resolve per project; ephemeral teardown becomes
   synchronous, separately budgeted, and scoped to what the run created.
5. Freeze v1. Other projects adopt at their own pace; incumbent tooling
   keeps serving them until they do.
