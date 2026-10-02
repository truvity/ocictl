package helmctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// chartLockGeneratedRe matches Chart.lock's `generated:` wall-clock line —
// the one non-deterministic field helm writes into the lock.
var chartLockGeneratedRe = regexp.MustCompile(`(?m)^generated:.*$`)

// hasDependencies reports whether the chart declares a dependencies block.
func hasDependencies(chartDir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml")) //nolint:gosec // caller-config path
	if err != nil {
		return false, fmt.Errorf("read Chart.yaml: %w", err)
	}

	var meta struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return false, fmt.Errorf("parse Chart.yaml: %w", err)
	}

	return len(meta.Dependencies) > 0, nil
}

// vendorDependencies runs `helm dependency build` against the SOURCE chart
// directory. It must run there, not on the temp copy: file:// repositories
// resolve relative to the chart directory, so a repo-internal dependency
// (the monorepo pattern — e.g. a service chart depending on a library chart
// in the same tree and released under the same tag) only resolves from the
// chart's real location.
//
// `build` — not `update` — is deliberate: it honours a committed Chart.lock
// (fetching exactly the locked versions, so the dependency set is reviewed
// rather than floating) and refuses a lock that no longer matches the
// Chart.yaml dependencies, instead of silently re-locking. With no lock yet
// it behaves like `update` and writes one. file://, oci:// and https
// repositories are all handled by helm itself.
//
// This is the one deliberate exception to "never alters source": it drops
// charts/*.tgz (and Chart.lock when none was committed) into the source
// chart — build artifacts the owning repo is expected to gitignore (charts/)
// or commit (Chart.lock).
func vendorDependencies(ctx context.Context, logger *slog.Logger, chartDir string) error {
	logger.InfoContext(ctx, "resolving chart dependencies",
		slog.String("chart", filepath.Base(chartDir)),
	)

	var stderr bytes.Buffer

	//nolint:gosec // caller-config path
	cmd := exec.CommandContext(ctx, "helm", "dependency", "build", "--skip-refresh", chartDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())

		hint := ""
		if strings.Contains(msg, "out of sync") {
			hint = " (Chart.lock is stale: run `helm dependency update` in the chart and commit the new Chart.lock)"
		}

		return fmt.Errorf("resolve dependencies of chart %s: %w: %s%s", chartDir, err, msg, hint)
	}

	return nil
}

// normalizeVendoredDependencies rewrites every charts/*.tgz in the temp copy
// through NormalizeTgz, so embedded dependency archives carry no timestamps
// or ownership. Helm 4 re-expands them at package time (making this a no-op
// for the output), but an older helm embeds the tarballs verbatim, and an
// embedded tgz is opaque bytes to any outer normalization pass.
func normalizeVendoredDependencies(chartTmp string) error {
	matches, err := filepath.Glob(filepath.Join(chartTmp, "charts", "*.tgz"))
	if err != nil {
		return fmt.Errorf("list dependency archives: %w", err)
	}

	for _, m := range matches {
		data, err := os.ReadFile(m) //nolint:gosec // under the temp copy
		if err != nil {
			return fmt.Errorf("read %s: %w", m, err)
		}

		norm, err := NormalizeTgz(data)
		if err != nil {
			return fmt.Errorf("normalize %s: %w", filepath.Base(m), err)
		}

		if err := os.WriteFile(m, norm, 0o644); err != nil { //nolint:gosec // chart archive, world-readable
			return fmt.Errorf("write %s: %w", m, err)
		}
	}

	return nil
}

// pinChartLock pins Chart.lock's `generated:` wall-clock field to epoch in
// the TEMP copy, so the packaged parent's bytes — and therefore its OCI
// digest — depend only on content. (The vendored charts/*.tgz need no such
// treatment: `helm package` loads dependencies and re-serializes them as
// expanded charts/<name>/ file trees, discarding the timestamped tarball
// wrapper.) The source tree keeps helm's original lock.
func pinChartLock(chartTmp string) error {
	lockPath := filepath.Join(chartTmp, "Chart.lock")

	lock, err := os.ReadFile(lockPath) //nolint:gosec // fixed name under the temp copy
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("read Chart.lock: %w", err)
	}

	pinned := chartLockGeneratedRe.ReplaceAll(lock, []byte(`generated: "1970-01-01T00:00:00Z"`))
	if err := os.WriteFile(lockPath, pinned, 0o644); err != nil { //nolint:gosec // chart metadata, world-readable
		return fmt.Errorf("write Chart.lock: %w", err)
	}

	return nil
}
