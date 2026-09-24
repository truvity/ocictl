# Deterministic, immutable Helm charts with GoReleaser

`helmctl` turns a GoReleaser build into a **self-contained Helm chart**: the
chart version comes from the release, and every image reference inside the
chart's `values.yaml` is pinned by its **multi-arch manifest digest** — no
`:latest`, no mutable tags, no "works because something overrode the values
at install time".

Two commands, run after `goreleaser release` exits (any invoker works —
CI step, `justfile`, Makefile, a wrapper tool; the free GoReleaser tier is
sufficient because nothing hooks *into* GoReleaser):

```bash
goreleaser release --clean            # builds + publishes images, writes dist/

helmctl goreleaser-manifest \
  --goreleaser-dist dist/myproject \
  -o dist/myproject/chart-manifest.yaml

helmctl package \
  --chart charts/myproject \
  --manifest dist/myproject/chart-manifest.yaml \
  --require-image-digests \
  --output dist/myproject/charts/

# optional, wherever your policy says charts get published:
helmctl push --tgz dist/myproject/charts/myproject-1.2.3.tgz \
  --registry my.registry.example.com --repository myproject/charts/myproject \
  --name myproject --version 1.2.3
```

Because the two `helmctl` steps only read files from `dist/`, ordering is
trivially safe **within one job**: once the `goreleaser` process has
exited, `artifacts.json` and `metadata.json` are complete.

> **Across jobs it is not safe, and the failure is silent.** A pipeline
> that packages charts in one job and builds images in another has a
> `dist/` without images wherever the chart step runs first. There is
> nothing to read, so the chart is packaged from the tag with its image
> values left at their defaults — usually empty. It renders, it pushes,
> the run is green, and the published chart cannot install.
>
> Nothing inside the repository can see this: every chart test supplies
> images of its own. It surfaces the first time somebody installs the
> published artifact.
>
> Keep `goreleaser` and both `helmctl` steps in **one job**, in that
> order. Then the chart is packaged from a file that does not exist until
> the images are pushed, and the ordering cannot be got wrong.
> `--require-image-digests` is the belt to that braces.

## What `goreleaser-manifest` reads

| File | Used for |
|---|---|
| `dist/<project>/artifacts.json` | published images + their digests |
| `dist/<project>/metadata.json` | release `version`, git `tag`, `commit` |

`digests.txt` is deliberately **ignored** — it only lists `dockers_v2`
images, not ko images. `artifacts.json` covers both:

- **ko builds** (`kos:` in `.goreleaser.yaml`) appear as
  `Docker Manifest` artifacts named `repo@sha256:…`. ko publishes one
  multi-platform image index; the recorded digest **is** the index digest.
  These artifacts carry no tag, so the manifest uses the release version
  (matching ko's usual `{{ .Version }}` tag template).
- **dockers_v2 builds** appear as one `Docker Image` artifact per tag,
  named `repo:tag`, with the digest in `extra.Digest`. For
  `platforms: [linux/amd64, linux/arm64]` this is again the
  **manifest-index** digest — verified against ECR
  (`application/vnd.oci.image.index.v1+json`). Duplicate tags of the same
  image (e.g. `:latest`) are deduplicated; a concrete version tag wins.

Every image **must** have a digest recorded (i.e. must actually have been
published — snapshot modes that skip publishing produce no digests and the
manifest step fails loudly rather than emitting mutable references).

## The release manifest

The intermediate file is a small, reviewable YAML — one build's truth:

```yaml
version: 1.6.1          # -> Chart.yaml version
appVersion: 1.6.1       # -> Chart.yaml appVersion
commit: 648e1a2…        # traceability
values:                 # deep-merged into the chart's values.yaml
  images:
    redirect:
      registry: 721506300184.dkr.ecr.eu-central-1.amazonaws.com
      repository: url-shortener/redirect
      tag: "1.6.1"
      digest: sha256:90c13588…
```

The component key is the repository's **last path segment**
(`url-shortener/redirect` → `images.redirect`). If two images collide on a
key, `goreleaser-manifest` errors — restrict with `--images a,b,c` or
restructure repositories. Archive the manifest as a build artifact and
"what exactly went into chart X" always has a one-file answer.

`helmctl package --manifest` is not GoReleaser-specific: any build system
(or a human) can produce this file. The `goreleaser-manifest` subcommand is
just one converter.

## The repository the manifest names is the one ko published to

`goreleaser-manifest` reports what the build *did*, not what it was asked
to do. That is the point — but it means a build that published to the
wrong place produces a manifest that faithfully names the wrong place, and
the chart is pinned to it.

The way this happens in practice: **ko reads `KO_DOCKER_REPO` from the
environment, and it overrides a `repositories:` set in the GoReleaser
configuration.** Silently. Combined with `bare: true` the images land at
exactly that value, so six components can all publish to one repository
and the first sign is an unrelated error afterwards — an SBOM write
rejected by a registry path nobody meant to use.

If you name repositories in the configuration, make sure nothing is
setting `KO_DOCKER_REPO` behind you. If something is (a shared CI
workflow, say), the reliable shape is the one ko has always had:
`KO_DOCKER_REPO` holds the destination and `base_import_paths: true`
appends each command's own name.

## Chart-side conventions

Declare per-component image values with the standard split fields, and
compose them in one helper so a missing digest fails at render time:

```yaml
# values.yaml — no defaults: values are baked at packaging time
# (or supplied explicitly at install time). Never hardcode registries.
images:
  redirect:
    registry: ""
    repository: ""
    tag: ""
    digest: ""
```

```gotemplate
{{/* image composes a pinned reference: registry/repository:tag@digest.
     Digest is REQUIRED — charts produced by this pipeline are immutable. */}}
{{- define "mychart.image" -}}
{{- $img := index .root.Values.images .component -}}
{{- if not $img -}}{{- fail (printf "images.%s is not set" .component) -}}{{- end -}}
{{- if not $img.digest -}}{{- fail (printf "images.%s.digest is required (charts are digest-pinned)" .component) -}}{{- end -}}
{{- printf "%s/%s:%s@%s" $img.registry $img.repository $img.tag $img.digest -}}
{{- end -}}
```

```yaml
# templates/deployment.yaml
image: {{ include "mychart.image" (dict "root" $ "component" "redirect") }}
```

Registries resolve by **digest** when both are present; the tag rides along
purely for humans (`kubectl describe` shows something readable).

`--require-image-digests` adds the same guarantee at packaging time: the
build fails if any `images.*` entry in the final values lacks a digest.

## Snapshot vs release builds

The pipeline is identical for both — if the build published images,
`artifacts.json` has digests and the chart packages the same way. Whether a
snapshot's chart is *pushed* anywhere is your policy, not this tool's.

**A `--snapshot` build publishes no images at all**, and that is worth
stating because it makes snapshots useless for proving this pipeline.
GoReleaser says so (`snapshot build: will not push any images`), ko writes
to `goreleaser.ko.local/<hash>` whatever repository is configured, and
`dockers_v2` produces per-architecture local tags rather than one index. A
snapshot therefore cannot tell you that your images land where you meant,
or under the name you meant. Only a real publish can.

## Go API

Everything the CLI does is available as libraries:

```go
import (
    "github.com/truvity/ocictl/pkg/goreleaserdist"
    "github.com/truvity/ocictl/pkg/helmctl"
)

dist, _ := goreleaserdist.Load("dist/myproject")
manifest, _ := dist.Manifest(goreleaserdist.ManifestOptions{})

result, _ := helmctl.Package(ctx, logger, helmctl.PackageConfig{
    ChartDir:            "charts/myproject",
    Version:             manifest.Version,
    AppVersion:          manifest.AppVersion,
    ValuesOverlay:       manifest.Values,
    RequireImageDigests: true,
    OutputDir:           "dist/myproject/charts",
})
```
