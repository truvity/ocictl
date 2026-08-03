# RFC: `fleetctl` — identity- and namespace-aware Kubernetes primitives

Status: **draft v2** (2026-08-03, rewritten after design review). Pilot
target: one downstream project before anything freezes.

## Problem

A repository that ships services onto a Kubernetes fleet needs two
capabilities nothing currently owns:

1. **Deploy into the right namespace with the right cluster facts** —
   "right namespace" may depend on *who is running the command* (personal
   developer namespaces), and cluster facts (account IDs, IAM boundaries,
   auth mode) live in the infrastructure repository, which product
   engineers should not need access to.
2. **Integration-test against a real cluster in an ephemeral namespace** —
   create, deploy, run, tear down, reliably.

Everything else in the build-and-ship path already has an owner, and this
tool must not compete with any of them.

## Non-goals (each has an owner already)

| Concern | Owner |
|---|---|
| Task orchestration, caching, sequencing | the repo's build system (moon) |
| Building images | goreleaser / docker |
| Packaging & pushing charts | helmctl |
| Cluster registry ("which clusters exist, how to reach them") | kubeconfig |
| Cluster facts ("what is true about this cluster") | the cluster itself (see below) |
| Credentials: registry auth, AWS profiles, kubeconfig generation | ambient environment |
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
- **Org semantics are parameters.** Group prefixes and namespace templates
  are configuration; the tool ships no organization defaults.

## Commands

A standalone binary, following this repository's one-binary-per-tool
convention (`cmd/crdctl`, `cmd/helmctl` → `cmd/fleetctl`); consumers run
it the same way (`go run github.com/truvity/ocictl/cmd/fleetctl@vX`).

```
fleetctl deploy -c <kubecontext> --chart <ref|path> [release]
    # helm upgrade --install into the resolved namespace,
    # with the cluster's fleet-values merged as .Values.fleet

fleetctl test [-c <kubecontext>] [--prefix it-] [--keep] -- <command...>
    # ephemeral namespace → run command → teardown

fleetctl whoami [-c <kubecontext>]
    # debug: the cluster's view of the caller + the resolved namespace
```

`-c` takes a **kubecontext name directly** (e.g. `devel@oidc`) — there is
no cluster-name indirection, because kubeconfig is the cluster registry.

## Identity and namespace resolution

Commands that target a namespace resolve it in this order:

1. `--namespace` flag
2. **`FLEET_NAMESPACE`** env var — the CI/robot path: robots name a
   namespace outright and no identity machinery runs. Deliberately
   bidirectional: read as an override when the caller set it, and always
   re-exported with the RESOLVED value into child processes — the same
   pattern as `KUBECONFIG`.
3. Derived personal namespace: `kubectl auth whoami -o json` against the
   target kubecontext returns the cluster's own view of the caller; the
   organization's identity platform embeds a short identifier as a
   prefixed entry in the groups claim (prefix is configuration, e.g.
   `emp:`), and the personal-namespace template renders it
   (`emp-{slug}`). **Exactly-one semantics** on the prefixed group: zero
   or multiple matches are hard errors, never guesses.

Resolution is **lazy**: only commands that need a namespace and were
given none resolve identity. Machine identities (CI runners) never carry
the prefixed group by construction, so a robot falling into the human
code path fails crisply instead of impersonating anyone.

Tuning (applies to `auth whoami` and every other kubectl this tool
spawns, so there is never a which-kubeconfig split-brain):

- `FLEET_KUBECONTEXT` — default for `-c`
- `FLEET_KUBECONFIG` — overrides the ambient `KUBECONFIG`

## Cluster facts: the `fleet-values` ConfigMap

A cluster is the authority on itself. The infrastructure pipeline stamps
a ConfigMap (default `kube-public/fleet-values`) into every cluster,
holding an **opaque** values map (account IDs, region, auth mode, IAM
boundaries, …). `fleetctl deploy` reads it from the target cluster and
merges it into the release as `.Values.fleet`, verbatim. The tool never
interprets the contents — organization semantics live in the charts.

Properties: always fresh (no committed-file staleness), readable by
anyone who can deploy, and fully generic for this tool.

## Test harness: one core, two entry points

**CLI** (any language, natural in build-system tasks):

```bash
fleetctl test -c devel@oidc --prefix it- -- go test ./svc/... -tags integration
```

**Go library** (`pkg/fleettest`, the TestMain path):

```go
func TestMain(m *testing.M) {
    os.Exit(fleettest.Run(m, fleettest.Options{
        Kubecontext:     "devel@oidc",
        NamespacePrefix: "it-",
    }))
}
```

Both share one implementation: create `{prefix}{git-sha}`, set/export
`FLEET_NAMESPACE` + `FLEET_KUBECONTEXT`, run, tear down (`--keep` /
`Options.Keep` preserves the namespace for debugging). The library adds
in-process accessors (`fleettest.Namespace()`) so tests avoid env-var
spelunking. The core shells out to kubectl for namespace lifecycle —
same zero-dependency rule as everything else.

## Libraries

- `pkg/kubewho` — `kubectl auth whoami` wrapper: caller identity +
  prefixed-group extraction (exactly-one semantics).
- `pkg/fleettest` — the ephemeral-namespace harness above.

Both stdlib-only; kubectl in `PATH` is the single runtime requirement.

## Configuration

There is **no required config file**. The two organization knobs —
personal-namespace template and identity group prefix — are flags with
env fallbacks (`FLEET_PERSONAL_NAMESPACE`, `FLEET_GROUP_PREFIX`),
typically set once in the build system's task definitions. An optional
`fleet.yaml` may carry repo-level defaults for the same knobs; it holds
nothing else.

## Out of scope — explicitly

- Generating or simplifying `aws.ini` / `kubeconfig` (decided
  2026-08-03: ambient environment, untouched).
- Everything in the Non-goals table.

## Open questions for the pilot (v0 may change freely)

1. Does `deploy` stay composite, or decompose into `fleetctl ns` /
   `fleetctl values` primitives piped into plain `helm upgrade` by the
   build system? (Current stance: composite — the values-merge deserves
   one tested implementation.)
2. ConfigMap location/name (`kube-public/fleet-values`) and whether a
   fallback flag (`--fleet-values file.yaml`) is worth having for
   clusters outside the stamping pipeline.

## Stability contract

Stabilize **once**: v0 during the pilot (breaking changes allowed),
frozen at v1 immediately after — additive-only and bugfix releases from
then on. The surface is deliberately tiny (three commands, four env
vars, two libraries), so there is little to churn.

## Rollout

1. `pkg/kubewho` + `pkg/fleettest` + the three commands.
2. Infrastructure side: stamp `fleet-values` ConfigMaps; add the
   identity-platform claim the prefixed-group extraction reads.
3. **Pilot on exactly one downstream project** (smallest real service),
   wired through its build-system tasks. Friction changes v0 freely.
4. Freeze v1. Other projects adopt at their own pace; incumbent tooling
   keeps serving them until they do.
