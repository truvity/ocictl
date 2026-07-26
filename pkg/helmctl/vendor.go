package helmctl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

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

// vendorDependencies runs `helm dependency update` against the SOURCE chart
// directory. It must run there, not on the temp copy: file:// repositories
// resolve relative to the chart directory, so a repo-internal dependency
// (the monorepo pattern — e.g. a project chart depending on a shared chart
// in the same tree) only resolves from the chart's real location.
//
// This is the one deliberate exception to "never alters source": it drops
// charts/*.tgz and Chart.lock into the source chart — build artifacts the
// owning repo is expected to gitignore. Both are re-derived on every run.
func vendorDependencies(ctx context.Context, logger *slog.Logger, chartDir string) error {
	logger.InfoContext(ctx, "vendoring chart dependencies",
		slog.String("chart", filepath.Base(chartDir)),
	)

	//nolint:gosec // caller-config path
	cmd := exec.CommandContext(ctx, "helm", "dependency", "update", "--skip-refresh", chartDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm dependency update %s: %w", chartDir, err)
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
